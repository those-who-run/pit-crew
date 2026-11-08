// Package apptoken mints a GitHub App installation token: an hour-long credential
// scoped to named repositories and to the permissions asked for, which is all a job
// step ever holds. The App's private key is read from a file the step alone has
// mounted, used to sign a short-lived JWT, and never written anywhere else.
//
// It uses the standard library only: an RS256 JWT is a base64url header and claims
// signed with RSA PKCS #1 v1.5 over SHA-256.
package apptoken

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultAPIURL is GitHub's REST API.
const DefaultAPIURL = "https://api.github.com"

// Config says which App, with which key, against which API.
type Config struct {
	// ClientID is the App's client ID, which GitHub accepts as the JWT issuer.
	ClientID string
	// KeyPath is the PEM file holding the App's private key.
	KeyPath string
	// InstallationID is the installation to mint for. Zero means the App's
	// installation on Owner, or on the organisation that owns the first
	// repository asked for.
	InstallationID int64
	// Owner is the organisation, for the installation lookup when no repository
	// is named.
	Owner string
	// APIURL is the API's base URL; empty means DefaultAPIURL.
	APIURL string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Token is an installation token and when it stops working.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// String hides the token, so a Token in a log line or an error shows nothing.
func (t Token) String() string { return "<installation token>" }

// Permissions are what the token may do, by GitHub's names: contents, pull_requests,
// statuses, issues, metadata; each read or write.
type Permissions map[string]string

// Mint returns an installation token for repos (as owner/name; all on one owner)
// with the permissions asked for. No repositories means every repository the
// installation covers, which the poller needs to list them. Each call mints a
// fresh token; GitHub's expire after an hour.
func Mint(ctx context.Context, client *http.Client, cfg Config, repos []string, permissions Permissions) (Token, error) {
	owner, names, err := splitRepos(repos)
	if err != nil {
		return Token{}, err
	}
	if owner == "" {
		owner = cfg.Owner
	}
	if owner == "" && cfg.InstallationID == 0 {
		return Token{}, errors.New("apptoken: no repositories, owner or installation to mint for")
	}
	key, err := loadKey(cfg.KeyPath)
	if err != nil {
		return Token{}, err
	}
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	jwt, err := signJWT(key, cfg.ClientID, now())
	if err != nil {
		return Token{}, err
	}
	api := strings.TrimRight(cfg.APIURL, "/")
	if api == "" {
		api = DefaultAPIURL
	}
	installation := cfg.InstallationID
	if installation == 0 {
		installation, err = findInstallation(ctx, client, api, jwt, owner)
		if err != nil {
			return Token{}, err
		}
	}
	return createToken(ctx, client, api, jwt, installation, names, permissions)
}

func splitRepos(repos []string) (owner string, names []string, err error) {
	for _, repo := range repos {
		o, name, ok := strings.Cut(repo, "/")
		if !ok || o == "" || name == "" || strings.Contains(name, "/") {
			return "", nil, fmt.Errorf("apptoken: %q is not owner/name", repo)
		}
		if owner != "" && !strings.EqualFold(o, owner) {
			return "", nil, fmt.Errorf("apptoken: %s and %s/%s have different owners; one token covers one installation", repos[0], o, name)
		}
		owner = o
		names = append(names, name)
	}
	return owner, names, nil
}

// loadKey reads an RSA private key in PKCS #1 (`RSA PRIVATE KEY`, which GitHub
// downloads) or PKCS #8 (`PRIVATE KEY`) PEM form. The error never includes the
// file's contents.
func loadKey(path string) (*rsa.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("apptoken: no key path")
	}
	data, err := os.ReadFile(path) //nolint:gosec // the path is the operator's configuration: where the Secret is mounted
	if err != nil {
		return nil, fmt.Errorf("apptoken: read key: %w", err)
	}
	defer clear(data)
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("apptoken: %s is not a PEM file", path)
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("apptoken: %s: %w", path, err)
		}
		return key, nil
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("apptoken: %s: %w", path, err)
		}
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("apptoken: %s holds a %T, not an RSA key", path, parsed)
		}
		return key, nil
	default:
		return nil, fmt.Errorf("apptoken: %s holds a %q block, not a private key", path, block.Type)
	}
}

// signJWT makes the App's JWT: issued a minute ago, for clock skew, and good for
// nine minutes, within GitHub's ten.
func signJWT(key *rsa.PrivateKey, clientID string, now time.Time) (string, error) {
	if clientID == "" {
		return "", errors.New("apptoken: no client ID")
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": clientID,
	})
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("apptoken: sign: %w", err)
	}
	return signing + "." + enc.EncodeToString(signature), nil
}

func request(ctx context.Context, client *http.Client, method, url, jwt string, body any) (*http.Response, error) {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("apptoken: encode: %w", err)
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, payload)
	if err != nil {
		return nil, fmt.Errorf("apptoken: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("apptoken: %s %s: %w", method, url, err)
	}
	return resp, nil
}

// Error is a response GitHub answered with a status outside 2xx while the token
// was minted. Its text has the status and GitHub's message, which names no
// credential.
type Error struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("apptoken: %s %s: %d %s: %s", e.Method, e.Path, e.Status, http.StatusText(e.Status), e.Message)
}

// HTTPStatus is the status GitHub answered with, for github.Unavailable.
func (e *Error) HTTPStatus() int { return e.Status }

// decode reads a JSON response, and on a non-2xx status returns an error with the
// status and GitHub's message, which names no credential.
func decode(resp *http.Response, into any) error {
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("apptoken: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var problem struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &problem)
		return &Error{Method: resp.Request.Method, Path: resp.Request.URL.Path, Status: resp.StatusCode, Message: problem.Message}
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("apptoken: %s: %w", resp.Request.URL.Path, err)
	}
	return nil
}

func findInstallation(ctx context.Context, client *http.Client, api, jwt, owner string) (int64, error) {
	resp, err := request(ctx, client, http.MethodGet, api+"/orgs/"+owner+"/installation", jwt, nil)
	if err != nil {
		return 0, err
	}
	var installation struct {
		ID int64 `json:"id"`
	}
	if err := decode(resp, &installation); err != nil {
		return 0, err
	}
	if installation.ID == 0 {
		return 0, fmt.Errorf("apptoken: the App is not installed on %s", owner)
	}
	return installation.ID, nil
}

func createToken(ctx context.Context, client *http.Client, api, jwt string, installation int64, names []string, permissions Permissions) (Token, error) {
	body := map[string]any{}
	if len(names) > 0 {
		body["repositories"] = names
	}
	if len(permissions) > 0 {
		body["permissions"] = permissions
	}
	resp, err := request(ctx, client, http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", api, installation), jwt, body)
	if err != nil {
		return Token{}, err
	}
	var token struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := decode(resp, &token); err != nil {
		return Token{}, err
	}
	if token.Token == "" {
		return Token{}, errors.New("apptoken: the response holds no token")
	}
	return Token{Value: token.Token, ExpiresAt: token.ExpiresAt}, nil
}

// FromEnv reads the App's configuration: APP_CLIENT_ID, APP_KEY_FILE,
// APP_INSTALLATION_ID (optional) and GITHUB_API_URL.
func FromEnv(getenv func(string) string, owner string) (Config, error) {
	cfg := Config{ClientID: getenv("APP_CLIENT_ID"), KeyPath: getenv("APP_KEY_FILE"), APIURL: getenv("GITHUB_API_URL"), Owner: owner}
	if cfg.ClientID == "" || cfg.KeyPath == "" {
		return cfg, errors.New("APP_CLIENT_ID and APP_KEY_FILE are needed to mint a token")
	}
	if v := getenv("APP_INSTALLATION_ID"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return cfg, fmt.Errorf("APP_INSTALLATION_ID: %q is not an installation ID", v)
		}
		cfg.InstallationID = id
	}
	return cfg, nil
}
