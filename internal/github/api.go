package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// user is a GitHub account in a response; nil when there is none.
type user struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

func (u *user) login() string {
	if u == nil {
		return ""
	}
	return u.Login
}

type label struct {
	Name string `json:"name"`
}

func labelNames(labels []label) []string {
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	return names
}

// PullRequest is an open pull request as the API lists it.
type PullRequest struct {
	Repo     string
	Number   int
	Title    string
	Body     string
	Author   string
	HeadRepo string
	HeadRef  string
	HeadSHA  string
	BaseRef  string
	BaseSHA  string
	Draft    bool
	Labels   []string
	URL      string
	Merged   bool
	State    string
	// MergedAt is zero unless merged.
	MergedAt  time.Time
	CreatedAt time.Time
}

type pullJSON struct {
	Number int     `json:"number"`
	Title  string  `json:"title"`
	Body   *string `json:"body"`
	User   *user   `json:"user"`
	Head   struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
	Draft     bool       `json:"draft"`
	Labels    []label    `json:"labels"`
	HTMLURL   string     `json:"html_url"`
	State     string     `json:"state"`
	Merged    bool       `json:"merged"`
	MergedAt  *time.Time `json:"merged_at"`
	CreatedAt time.Time  `json:"created_at"`
}

func (p pullJSON) pull(repo string) PullRequest {
	pr := PullRequest{
		Repo: repo, Number: p.Number, Title: p.Title, Author: p.User.login(),
		HeadRef: p.Head.Ref, HeadSHA: p.Head.SHA, BaseRef: p.Base.Ref, BaseSHA: p.Base.SHA,
		Draft: p.Draft, Labels: labelNames(p.Labels), URL: p.HTMLURL, State: p.State, Merged: p.Merged, CreatedAt: p.CreatedAt,
	}
	if p.Body != nil {
		pr.Body = *p.Body
	}
	if p.Head.Repo != nil {
		pr.HeadRepo = p.Head.Repo.FullName
	}
	if p.MergedAt != nil {
		pr.MergedAt = *p.MergedAt
		pr.Merged = true
	}
	return pr
}

// ListInstallationRepos returns the repositories the token's installation covers,
// as owner/name.
func (c *Client) ListInstallationRepos(ctx context.Context) ([]string, error) {
	var repos []string
	err := c.getAll(ctx, "/installation/repositories?per_page=100", func(page []byte) error {
		var body struct {
			Repositories []struct {
				FullName string `json:"full_name"`
				Archived bool   `json:"archived"`
			} `json:"repositories"`
		}
		if err := decode("/installation/repositories", page, &body); err != nil {
			return err
		}
		for _, r := range body.Repositories {
			if !r.Archived {
				repos = append(repos, r.FullName)
			}
		}
		return nil
	})
	return repos, err
}

// ListOpenPulls returns a repository's open pull requests.
func (c *Client) ListOpenPulls(ctx context.Context, repo string) ([]PullRequest, error) {
	path, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	raw, err := pages[pullJSON](ctx, c, path+"/pulls?state=open&per_page=100")
	if err != nil {
		return nil, err
	}
	pulls := make([]PullRequest, 0, len(raw))
	for _, p := range raw {
		pulls = append(pulls, p.pull(repo))
	}
	return pulls, nil
}

// GetPull returns one pull request, whatever its state.
func (c *Client) GetPull(ctx context.Context, repo string, number int) (PullRequest, error) {
	path, err := repoPath(repo)
	if err != nil {
		return PullRequest{}, err
	}
	var p pullJSON
	if err := c.get(ctx, fmt.Sprintf("%s/pulls/%d", path, number), &p); err != nil {
		return PullRequest{}, err
	}
	return p.pull(repo), nil
}

// Commit is one commit of a pull request.
type Commit struct {
	SHA string
	// Author and Committer are the logins GitHub maps the addresses to; empty when
	// it maps them to nobody.
	Author         string
	Committer      string
	AuthorName     string
	AuthorEmail    string
	CommitterName  string
	CommitterEmail string
	Message        string
}

// ListPullCommits returns a pull request's commits, oldest first.
func (c *Client) ListPullCommits(ctx context.Context, repo string, number int) ([]Commit, error) {
	path, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	type identity struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	type commitJSON struct {
		SHA       string `json:"sha"`
		Author    *user  `json:"author"`
		Committer *user  `json:"committer"`
		Commit    struct {
			Author    identity `json:"author"`
			Committer identity `json:"committer"`
			Message   string   `json:"message"`
		} `json:"commit"`
	}
	raw, err := pages[commitJSON](ctx, c, fmt.Sprintf("%s/pulls/%d/commits?per_page=100", path, number))
	if err != nil {
		return nil, err
	}
	commits := make([]Commit, 0, len(raw))
	for _, r := range raw {
		commits = append(commits, Commit{
			SHA: r.SHA, Author: r.Author.login(), Committer: r.Committer.login(),
			AuthorName: r.Commit.Author.Name, AuthorEmail: r.Commit.Author.Email,
			CommitterName: r.Commit.Committer.Name, CommitterEmail: r.Commit.Committer.Email,
			Message: r.Commit.Message,
		})
	}
	return commits, nil
}

// Comment is one comment on an issue or a pull request's conversation.
type Comment struct {
	ID        int64
	Author    string
	Body      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type commentJSON struct {
	ID        int64     `json:"id"`
	User      *user     `json:"user"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListComments returns the comments on an issue or a pull request's conversation,
// oldest first.
func (c *Client) ListComments(ctx context.Context, repo string, number int) ([]Comment, error) {
	path, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	raw, err := pages[commentJSON](ctx, c, fmt.Sprintf("%s/issues/%d/comments?per_page=100", path, number))
	if err != nil {
		return nil, err
	}
	comments := make([]Comment, 0, len(raw))
	for _, r := range raw {
		comments = append(comments, Comment{ID: r.ID, Author: r.User.login(), Body: r.Body, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt})
	}
	return comments, nil
}

// CreateComment posts a comment and returns its ID.
func (c *Client) CreateComment(ctx context.Context, repo string, number int, body string) (int64, error) {
	path, err := repoPath(repo)
	if err != nil {
		return 0, err
	}
	var created struct {
		ID int64 `json:"id"`
	}
	err = c.post(ctx, fmt.Sprintf("%s/issues/%d/comments", path, number), map[string]string{"body": body}, &created)
	return created.ID, err
}

// UpdateComment replaces a comment's body.
func (c *Client) UpdateComment(ctx context.Context, repo string, id int64, body string) error {
	path, err := repoPath(repo)
	if err != nil {
		return err
	}
	return c.patch(ctx, fmt.Sprintf("%s/issues/comments/%d", path, id), map[string]string{"body": body})
}

// RemoveLabel takes a label off an issue or a pull request. A label that is not
// there (GitHub answers 404) is not an error: the point is that it is gone.
func (c *Client) RemoveLabel(ctx context.Context, repo string, number int, name string) error {
	path, err := repoPath(repo)
	if err != nil {
		return err
	}
	err = c.delete(ctx, fmt.Sprintf("%s/issues/%d/labels/%s", path, number, url.PathEscape(name)))
	if IsStatus(err, 404) {
		return nil
	}
	return err
}

// AddLabel puts a label on an issue or a pull request. GitHub records a
// `labeled` event for it, by the token's account, only when the label was not
// already there.
func (c *Client) AddLabel(ctx context.Context, repo string, number int, name string) error {
	path, err := repoPath(repo)
	if err != nil {
		return err
	}
	return c.post(ctx, fmt.Sprintf("%s/issues/%d/labels", path, number), map[string][]string{"labels": {name}}, nil)
}

// Status is one commit status.
type Status struct {
	Context     string
	State       string
	Description string
	Creator     string
	CreatedAt   time.Time
}

// ListStatuses returns the commit statuses on a commit, newest first.
func (c *Client) ListStatuses(ctx context.Context, repo, sha string) ([]Status, error) {
	path, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	type statusJSON struct {
		Context     string    `json:"context"`
		State       string    `json:"state"`
		Description string    `json:"description"`
		Creator     *user     `json:"creator"`
		CreatedAt   time.Time `json:"created_at"`
	}
	raw, err := pages[statusJSON](ctx, c, fmt.Sprintf("%s/commits/%s/statuses?per_page=100", path, url.PathEscape(sha)))
	if err != nil {
		return nil, err
	}
	statuses := make([]Status, 0, len(raw))
	for _, r := range raw {
		statuses = append(statuses, Status{Context: r.Context, State: r.State, Description: r.Description, Creator: r.Creator.login(), CreatedAt: r.CreatedAt})
	}
	return statuses, nil
}

// SetStatus sets a commit status. Description is cut to GitHub's 140 characters.
func (c *Client) SetStatus(ctx context.Context, repo, sha, context, state, description string) error {
	path, err := repoPath(repo)
	if err != nil {
		return err
	}
	if runes := []rune(description); len(runes) > 140 {
		description = string(runes[:139]) + "…"
	}
	body := map[string]string{"state": state, "context": context, "description": description}
	return c.post(ctx, fmt.Sprintf("%s/statuses/%s", path, url.PathEscape(sha)), body, nil)
}

// ListChecks returns the state of each check on a commit by name: check runs by
// their name (the conclusion once complete, else the status), and commit statuses
// by their context, the newest of each.
func (c *Client) ListChecks(ctx context.Context, repo, sha string) (map[string]string, error) {
	path, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	checks := map[string]string{}
	err = c.getAll(ctx, fmt.Sprintf("%s/commits/%s/check-runs?per_page=100", path, url.PathEscape(sha)), func(page []byte) error {
		var body struct {
			CheckRuns []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		if err := decode("check-runs", page, &body); err != nil {
			return err
		}
		for _, run := range body.CheckRuns {
			state := run.Status
			if run.Status == "completed" {
				state = run.Conclusion
			}
			// The newest run of a name is listed first.
			if _, seen := checks[run.Name]; !seen {
				checks[run.Name] = state
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	statuses, err := c.ListStatuses(ctx, repo, sha)
	if err != nil {
		return nil, err
	}
	for _, status := range statuses {
		if _, seen := checks[status.Context]; !seen {
			checks[status.Context] = status.State
		}
	}
	return checks, nil
}

// ListRequiredChecks returns the status checks the branch's rulesets require.
func (c *Client) ListRequiredChecks(ctx context.Context, repo, branch string) ([]string, error) {
	path, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			Checks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	if err := c.get(ctx, path+"/rules/branches/"+url.PathEscape(branch), &rules); err != nil {
		return nil, err
	}
	var contexts []string
	for _, rule := range rules {
		if rule.Type != "required_status_checks" {
			continue
		}
		for _, check := range rule.Parameters.Checks {
			contexts = append(contexts, check.Context)
		}
	}
	return contexts, nil
}

// CreatePull opens a pull request and returns its number and URL.
func (c *Client) CreatePull(ctx context.Context, repo, title, body, head, base string) (int, string, error) {
	path, err := repoPath(repo)
	if err != nil {
		return 0, "", err
	}
	var created struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	payload := map[string]any{"title": title, "body": body, "head": head, "base": base, "draft": false}
	if err := c.post(ctx, path+"/pulls", payload, &created); err != nil {
		return 0, "", err
	}
	return created.Number, created.HTMLURL, nil
}

// UpdatePullBody replaces a pull request's body.
func (c *Client) UpdatePullBody(ctx context.Context, repo string, number int, body string) error {
	path, err := repoPath(repo)
	if err != nil {
		return err
	}
	return c.patch(ctx, fmt.Sprintf("%s/pulls/%d", path, number), map[string]string{"body": body})
}

// Comparison is how head stands against base.
type Comparison struct {
	// Status is identical, ahead, behind or diverged.
	Status       string
	AheadBy      int
	BehindBy     int
	MergeBaseSHA string
	Files        []ChangedFile
}

// ChangedFile is one file a comparison changes.
type ChangedFile struct {
	Path   string
	Status string
}

// Compare compares base...head, as `git diff` from their merge base does.
func (c *Client) Compare(ctx context.Context, repo, base, head string) (Comparison, error) {
	path, err := repoPath(repo)
	if err != nil {
		return Comparison{}, err
	}
	var cmp Comparison
	err = c.getAll(ctx, fmt.Sprintf("%s/compare/%s...%s?per_page=100", path, url.PathEscape(base), url.PathEscape(head)), func(page []byte) error {
		var body struct {
			Status          string `json:"status"`
			AheadBy         int    `json:"ahead_by"`
			BehindBy        int    `json:"behind_by"`
			MergeBaseCommit struct {
				SHA string `json:"sha"`
			} `json:"merge_base_commit"`
			Files []struct {
				Filename string `json:"filename"`
				Status   string `json:"status"`
			} `json:"files"`
		}
		if err := decode("compare", page, &body); err != nil {
			return err
		}
		if cmp.Status == "" {
			cmp.Status, cmp.AheadBy, cmp.BehindBy, cmp.MergeBaseSHA = body.Status, body.AheadBy, body.BehindBy, body.MergeBaseCommit.SHA
		}
		for _, f := range body.Files {
			cmp.Files = append(cmp.Files, ChangedFile{Path: f.Filename, Status: f.Status})
		}
		return nil
	})
	return cmp, err
}

// Issue is an issue as the API lists it.
type Issue struct {
	Repo      string
	Number    int
	Title     string
	Body      string
	Author    string
	State     string
	Labels    []string
	URL       string
	CreatedAt time.Time
}

type issueJSON struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        *string   `json:"body"`
	User        *user     `json:"user"`
	State       string    `json:"state"`
	Labels      []label   `json:"labels"`
	HTMLURL     string    `json:"html_url"`
	CreatedAt   time.Time `json:"created_at"`
	PullRequest *struct {
		MergedAt *time.Time `json:"merged_at"`
	} `json:"pull_request"`
}

func (i issueJSON) issue(repo string) Issue {
	issue := Issue{Repo: repo, Number: i.Number, Title: i.Title, Author: i.User.login(), State: i.State,
		Labels: labelNames(i.Labels), URL: i.HTMLURL, CreatedAt: i.CreatedAt}
	if i.Body != nil {
		issue.Body = *i.Body
	}
	return issue
}

// ListOpenIssues returns a repository's open issues carrying every label named.
// Pull requests, which the API lists among issues, are left out.
func (c *Client) ListOpenIssues(ctx context.Context, repo string, labels []string) ([]Issue, error) {
	path, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	query := url.Values{"state": {"open"}, "per_page": {"100"}}
	if len(labels) > 0 {
		query.Set("labels", strings.Join(labels, ","))
	}
	raw, err := pages[issueJSON](ctx, c, path+"/issues?"+query.Encode())
	if err != nil {
		return nil, err
	}
	var issues []Issue
	for _, r := range raw {
		if r.PullRequest == nil {
			issues = append(issues, r.issue(repo))
		}
	}
	return issues, nil
}

// GetIssue returns one issue.
func (c *Client) GetIssue(ctx context.Context, repo string, number int) (Issue, error) {
	path, err := repoPath(repo)
	if err != nil {
		return Issue{}, err
	}
	var i issueJSON
	if err := c.get(ctx, fmt.Sprintf("%s/issues/%d", path, number), &i); err != nil {
		return Issue{}, err
	}
	return i.issue(repo), nil
}

// TimelineEvent is one of the issue timeline's events the ladder reads: a label
// added or removed, with who did it, or a pull request that references the issue.
type TimelineEvent struct {
	// Event is labeled, unlabeled or cross-referenced.
	Event string
	Actor string
	Label string
	At    time.Time
	// Source is the referencing pull request, for cross-referenced.
	Source *PullRequest
}

// ListTimeline returns an issue's timeline, in order.
func (c *Client) ListTimeline(ctx context.Context, repo string, number int) ([]TimelineEvent, error) {
	path, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	type eventJSON struct {
		Event     string    `json:"event"`
		Actor     *user     `json:"actor"`
		Label     *label    `json:"label"`
		CreatedAt time.Time `json:"created_at"`
		Source    *struct {
			Type  string `json:"type"`
			Issue *struct {
				issueJSON
				Repository struct {
					FullName string `json:"full_name"`
				} `json:"repository"`
			} `json:"issue"`
		} `json:"source"`
	}
	raw, err := pages[eventJSON](ctx, c, fmt.Sprintf("%s/issues/%d/timeline?per_page=100", path, number))
	if err != nil {
		return nil, err
	}
	var events []TimelineEvent
	for _, r := range raw {
		event := TimelineEvent{Event: r.Event, Actor: r.Actor.login(), At: r.CreatedAt}
		if r.Label != nil {
			event.Label = r.Label.Name
		}
		if r.Source != nil && r.Source.Issue != nil && r.Source.Issue.PullRequest != nil {
			i := r.Source.Issue
			pr := PullRequest{Repo: i.Repository.FullName, Number: i.Number, Title: i.Title, Author: i.User.login(),
				State: i.State, URL: i.HTMLURL, CreatedAt: i.CreatedAt}
			if i.Body != nil {
				pr.Body = *i.Body
			}
			if i.PullRequest.MergedAt != nil {
				pr.Merged, pr.MergedAt = true, *i.PullRequest.MergedAt
			}
			event.Source = &pr
		}
		events = append(events, event)
	}
	return events, nil
}

// ListBranches returns the names of the branches whose name starts with prefix.
func (c *Client) ListBranches(ctx context.Context, repo, prefix string) ([]string, error) {
	path, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	type refJSON struct {
		Ref string `json:"ref"`
	}
	raw, err := pages[refJSON](ctx, c, path+"/git/matching-refs/heads/"+escapePath(prefix))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, r := range raw {
		if name, ok := strings.CutPrefix(r.Ref, "refs/heads/"); ok {
			names = append(names, name)
		}
	}
	return names, nil
}

// escapePath escapes each segment of a path, keeping its slashes.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
