// Package github is the small REST client the ladder's steps need: open pull
// requests and what is on them, issues and their timelines, comments, commit
// statuses, pull requests and comparisons. Standard library only.
//
// Every call retries: a request that never reaches the network is tried again with
// backoff, since a new pod on the maintainer's cluster reaches the network a few
// seconds late, and a rate-limited response waits for the limit to reset. Writes
// retry only when no response came back at all is impossible to tell from a
// response that was lost, so a write is retried only on a rate limit, never on a
// transport error.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is GitHub's REST API.
const DefaultBaseURL = "https://api.github.com"

// Client talks to one GitHub API as one token.
type Client struct {
	// BaseURL is the API's base; empty means DefaultBaseURL.
	BaseURL string
	// Token is the installation token, sent as a bearer token.
	Token string
	// HTTP is the client used; nil means http.DefaultClient with a timeout.
	HTTP *http.Client
	// Logger gets one line per retry; nil logs nothing.
	Logger *slog.Logger
	// Sleep waits between tries; nil means time.Sleep honouring the context.
	Sleep func(ctx context.Context, d time.Duration) error
	// Retries is how many times a request is tried again; zero means 5.
	Retries int
}

// Error is a response GitHub answered with a status outside 2xx.
type Error struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("github: %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// IsStatus reports whether err is a GitHub response with the given status.
func IsStatus(err error, status int) bool {
	var ghErr *Error
	return errors.As(err, &ghErr) && ghErr.Status == status
}

func (c *Client) base() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) retries() int {
	if c.Retries > 0 {
		return c.Retries
	}
	return 5
}

func (c *Client) log(ctx context.Context, msg string, args ...any) {
	if c.Logger != nil {
		c.Logger.InfoContext(ctx, msg, args...)
	}
}

// maxRateLimitWait caps how long one rate-limited request waits for the reset.
const maxRateLimitWait = 5 * time.Minute

// do sends one request, with retries, and returns the response body and headers.
// Transport errors and 5xx answers are retried for GET only; a rate limit (403 or
// 429 with the limit exhausted, or a Retry-After) is retried for any method.
func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, http.Header, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, nil, fmt.Errorf("github: encode %s %s: %w", method, path, err)
		}
	}
	target := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		target = c.base() + path
	}
	backoff := time.Second
	for attempt := 0; ; attempt++ {
		data, header, err := c.once(ctx, method, target, payload)
		if err == nil {
			return data, header, nil
		}
		wait, retry := c.shouldRetry(method, err, header, attempt)
		if !retry {
			return nil, header, err
		}
		if wait == 0 {
			wait = backoff
			backoff = min(backoff*2, 16*time.Second)
		}
		c.log(ctx, "github retry", "method", method, "path", path, "attempt", attempt+1, "wait", wait.String(), "err", err.Error())
		if err := c.sleep(ctx, wait); err != nil {
			return nil, nil, err
		}
	}
}

func (c *Client) shouldRetry(method string, err error, header http.Header, attempt int) (time.Duration, bool) {
	if attempt >= c.retries() {
		return 0, false
	}
	var ghErr *Error
	if !errors.As(err, &ghErr) {
		// Nothing came back: the network is not there yet. Safe to repeat a read;
		// a write might have landed.
		return 0, method == http.MethodGet
	}
	if ghErr.Status == http.StatusTooManyRequests || ghErr.Status == http.StatusForbidden && header.Get("X-RateLimit-Remaining") == "0" {
		return rateLimitWait(header, time.Now()), true
	}
	if after := header.Get("Retry-After"); after != "" {
		return rateLimitWait(header, time.Now()), true
	}
	return 0, ghErr.Status >= 500 && method == http.MethodGet
}

// rateLimitWait reads how long GitHub asks a client to wait: Retry-After in
// seconds, or the time until X-RateLimit-Reset, capped.
func rateLimitWait(header http.Header, now time.Time) time.Duration {
	if after, err := strconv.Atoi(header.Get("Retry-After")); err == nil && after > 0 {
		return min(time.Duration(after)*time.Second, maxRateLimitWait)
	}
	if reset, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		if wait := time.Unix(reset, 0).Sub(now) + time.Second; wait > 0 {
			return min(wait, maxRateLimitWait)
		}
	}
	return 5 * time.Second
}

func (c *Client) once(ctx context.Context, method, target string, payload []byte) ([]byte, http.Header, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("github: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("github: %s %s: %w", method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.Header, fmt.Errorf("github: read %s %s: %w", method, req.URL.Path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var problem struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &problem)
		return nil, resp.Header, &Error{Method: method, Path: req.URL.Path, Status: resp.StatusCode, Message: problem.Message}
	}
	return data, resp.Header, nil
}

// get decodes one GET.
func (c *Client) get(ctx context.Context, path string, into any) error {
	data, _, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return decode(path, data, into)
}

// getAll follows Link: rel="next" and decodes every page into one slice.
func (c *Client) getAll(ctx context.Context, path string, each func(page []byte) error) error {
	next := path
	for next != "" {
		data, header, err := c.do(ctx, http.MethodGet, next, nil)
		if err != nil {
			return err
		}
		if err := each(data); err != nil {
			return err
		}
		next = nextLink(header.Get("Link"))
	}
	return nil
}

var nextLinkRE = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextLink(header string) string {
	if m := nextLinkRE.FindStringSubmatch(header); m != nil {
		return m[1]
	}
	return ""
}

// pages collects every page of a list of T.
func pages[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var all []T
	err := c.getAll(ctx, path, func(page []byte) error {
		var items []T
		if err := decode(path, page, &items); err != nil {
			return err
		}
		all = append(all, items...)
		return nil
	})
	return all, err
}

func decode(path string, data []byte, into any) error {
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("github: %s: %w", path, err)
	}
	return nil
}

func (c *Client) post(ctx context.Context, path string, body, into any) error {
	data, _, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	if into == nil {
		return nil
	}
	return decode(path, data, into)
}

func (c *Client) patch(ctx context.Context, path string, body any) error {
	_, _, err := c.do(ctx, http.MethodPatch, path, body)
	return err
}

// WaitForNetwork makes one cheap, unauthenticated-safe call, with retries, so a
// step that starts before the pod's network is up waits for it rather than failing
// its first real request.
func (c *Client) WaitForNetwork(ctx context.Context) error {
	var limits struct {
		Resources map[string]any `json:"resources"`
	}
	return c.get(ctx, "/rate_limit", &limits)
}

// repoPath checks a repository name and returns its API path.
func repoPath(repo string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.ContainsAny(repo, " ?#%") || strings.Contains(name, "/") {
		return "", fmt.Errorf("github: %q is not owner/name", repo)
	}
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}
