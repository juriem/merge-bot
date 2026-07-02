// Package ghclient wraps the GitHub REST API calls that mergebot needs.
package ghclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v66/github"
	"mergebot/internal/merge"
	"mergebot/internal/review"
)

// Client implements the review dashboard's Fetcher in addition to merge.GitHub.
var _ review.Fetcher = (*Client)(nil)

// Client is a thin wrapper around go-github exposing only the operations the
// merge runner relies on.
type Client struct {
	gh    *github.Client
	http  *http.Client
	token string
}

// Option customises a Client built by New.
type Option func(*clientOptions)

type clientOptions struct {
	maxRateLimitWait time.Duration
	rateLimitRetries int
}

// WithRateLimitWait caps how long a single request waits for a rate limit to
// reset before the error is surfaced. A non-positive value disables waiting.
func WithRateLimitWait(d time.Duration) Option {
	return func(o *clientOptions) { o.maxRateLimitWait = d }
}

// WithRateLimitRetries sets how many times a request retries while waiting out a
// rate limit.
func WithRateLimitRetries(n int) Option {
	return func(o *clientOptions) { o.rateLimitRetries = n }
}

// New builds a Client authenticated with the given personal access token. Both
// the REST and GraphQL HTTP clients wait out GitHub rate limits transparently.
func New(token string, opts ...Option) *Client {
	o := clientOptions{
		maxRateLimitWait: defaultMaxRateLimitWait,
		rateLimitRetries: defaultRateLimitRetries,
	}
	for _, opt := range opts {
		opt(&o)
	}

	transport := func() http.RoundTripper {
		return newRateLimitTransport(http.DefaultTransport, o.maxRateLimitWait, o.rateLimitRetries)
	}

	return &Client{
		gh: github.NewClient(&http.Client{Transport: transport()}).WithAuthToken(token),
		http: &http.Client{
			// Bound the whole exchange (including any rate-limit wait) so a hung
			// request cannot block a worker forever.
			Timeout:   o.maxRateLimitWait + time.Minute,
			Transport: transport(),
		},
		token: token,
	}
}

// GetPullRequest fetches the current state of a pull request.
func (c *Client) GetPullRequest(ctx context.Context, owner, repo string, number int) (*github.PullRequest, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, number)

	return pr, err
}

// PullState returns a pull request's mergeable_state along with its base branch
// and head SHA (needed to compare with base and list check runs).
func (c *Client) PullState(ctx context.Context, owner, repo string, number int) (state, base, head string, err error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return "", "", "", err
	}

	return pr.GetMergeableState(), pr.GetBase().GetRef(), pr.GetHead().GetSHA(), nil
}

// BehindBy returns how many commits head is behind base (0 when head already
// contains everything in base) — i.e. whether "Update branch" would do anything.
func (c *Client) BehindBy(ctx context.Context, owner, repo, base, head string) (int, error) {
	cmp, _, err := c.gh.Repositories.CompareCommits(ctx, owner, repo, base, head, &github.ListOptions{PerPage: 1})
	if err != nil {
		return 0, err
	}

	return cmp.GetBehindBy(), nil
}

// CurrentUser returns the login of the authenticated token owner.
func (c *Client) CurrentUser(ctx context.Context) (string, error) {
	user, _, err := c.gh.Users.Get(ctx, "")
	if err != nil {
		return "", err
	}

	return user.GetLogin(), nil
}

// ListOpenPRsByAuthor returns the open, ready-for-review (non-draft) pull
// requests that author opened in the repo.
func (c *Client) ListOpenPRsByAuthor(ctx context.Context, owner, repo, author string) ([]review.PR, error) {
	query := fmt.Sprintf("repo:%s/%s is:pr is:open draft:false author:%s", owner, repo, author)
	opts := &github.SearchOptions{ListOptions: github.ListOptions{PerPage: 100}}

	var out []review.PR
	for {
		result, resp, err := c.gh.Search.Issues(ctx, query, opts)
		if err != nil {
			return nil, err
		}

		for _, issue := range result.Issues {
			out = append(out, review.PR{Number: issue.GetNumber(), Title: issue.GetTitle()})
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return out, nil
}

// UpdateBranch merges the base branch into the PR head branch. GitHub answers
// with 202 Accepted, which go-github surfaces as *github.AcceptedError; that is
// the expected success path here.
func (c *Client) UpdateBranch(ctx context.Context, owner, repo string, number int, expectedHeadSHA string) error {
	opts := &github.PullRequestBranchUpdateOptions{}
	if expectedHeadSHA != "" {
		opts.ExpectedHeadSHA = github.String(expectedHeadSHA)
	}

	_, _, err := c.gh.PullRequests.UpdateBranch(ctx, owner, repo, number, opts)

	var accepted *github.AcceptedError
	if errors.As(err, &accepted) {
		return nil
	}

	return err
}

// Merge merges the pull request using the given method (squash, merge, rebase).
func (c *Client) Merge(ctx context.Context, owner, repo string, number int, method string) error {
	_, _, err := c.gh.PullRequests.Merge(ctx, owner, repo, number, "", &github.PullRequestOptions{
		MergeMethod: method,
	})

	return err
}

// CheckSummary returns a human-readable summary of the check runs on a ref that
// are not yet successful, so the operator can see what is blocking a merge.
func (c *Client) CheckSummary(ctx context.Context, owner, repo, ref string) (string, error) {
	runs, _, err := c.gh.Checks.ListCheckRunsForRef(ctx, owner, repo, ref, &github.ListCheckRunsOptions{})
	if err != nil {
		return "", err
	}

	var failed, pending []string
	for _, run := range runs.CheckRuns {
		if run.GetStatus() != "completed" {
			pending = append(pending, run.GetName())
			continue
		}

		switch run.GetConclusion() {
		case "success", "skipped", "neutral":
		default:
			failed = append(failed, run.GetName()+"="+run.GetConclusion())
		}
	}

	var parts []string
	if len(failed) > 0 {
		parts = append(parts, "failed: "+strings.Join(failed, ", "))
	}
	if len(pending) > 0 {
		parts = append(parts, "pending: "+strings.Join(pending, ", "))
	}

	return strings.Join(parts, "; "), nil
}

// RequiredChecks returns the names of the status checks that branch protection
// requires before merging into branch. A branch without protection (or without
// required checks) yields an empty slice rather than an error.
func (c *Client) RequiredChecks(ctx context.Context, owner, repo, branch string) ([]string, error) {
	rsc, resp, err := c.gh.Repositories.GetRequiredStatusChecks(ctx, owner, repo, branch)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, nil
		}

		return nil, err
	}

	// Checks is the current field; Contexts is its deprecated predecessor and
	// only one of them is ever populated.
	var names []string
	if rsc.Checks != nil {
		for _, check := range *rsc.Checks {
			if check != nil && check.Context != "" {
				names = append(names, check.Context)
			}
		}
	}
	if len(names) == 0 && rsc.Contexts != nil {
		names = append(names, *rsc.Contexts...)
	}

	return names, nil
}

// CheckRuns returns the latest check run per name on ref, merged with legacy
// commit statuses (some required contexts are posted via the statuses API
// rather than the Checks API).
func (c *Client) CheckRuns(ctx context.Context, owner, repo, ref string) ([]merge.CheckRun, error) {
	byName := make(map[string]merge.CheckRun)

	opts := &github.ListCheckRunsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := c.gh.Checks.ListCheckRunsForRef(ctx, owner, repo, ref, opts)
		if err != nil {
			return nil, err
		}

		for _, run := range runs.CheckRuns {
			name := run.GetName()
			if prev, ok := byName[name]; ok && prev.Completed && run.GetStatus() != "completed" {
				continue
			}

			byName[name] = merge.CheckRun{
				Name:       name,
				Completed:  run.GetStatus() == "completed",
				Conclusion: run.GetConclusion(),
			}
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	if err := c.addLegacyStatuses(ctx, owner, repo, ref, byName); err != nil {
		return nil, err
	}

	result := make([]merge.CheckRun, 0, len(byName))
	for _, run := range byName {
		result = append(result, run)
	}

	return result, nil
}

// addLegacyStatuses folds commit-status contexts into byName without
// overwriting a check run of the same name.
func (c *Client) addLegacyStatuses(ctx context.Context, owner, repo, ref string, byName map[string]merge.CheckRun) error {
	opts := &github.ListOptions{PerPage: 100}
	for {
		combined, resp, err := c.gh.Repositories.GetCombinedStatus(ctx, owner, repo, ref, opts)
		if err != nil {
			return err
		}

		for _, status := range combined.Statuses {
			name := status.GetContext()
			if _, ok := byName[name]; ok {
				continue
			}

			state := status.GetState()
			byName[name] = merge.CheckRun{
				Name:       name,
				Completed:  state == "success" || state == "failure" || state == "error",
				Conclusion: statusConclusion(state),
			}
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return nil
}

// statusConclusion maps a legacy commit-status state onto a check-run
// conclusion so the runner can treat both uniformly.
func statusConclusion(state string) string {
	switch state {
	case "success":
		return "success"
	case "failure", "error":
		return "failure"
	default:
		return ""
	}
}

const reviewStatusQuery = `query($owner:String!,$repo:String!,$number:Int!){` +
	`repository(owner:$owner,name:$repo){pullRequest(number:$number){` +
	`reviewDecision ` +
	`latestOpinionatedReviews(first:100){nodes{state}} ` +
	`reviewThreads(first:100){nodes{isResolved}pageInfo{hasNextPage}}}}}`

// ReviewStatus reports unresolved review threads and the overall review
// decision via the GraphQL API, which the REST endpoints do not expose.
func (c *Client) ReviewStatus(ctx context.Context, owner, repo string, number int) (merge.ReviewStatus, error) {
	payload, err := json.Marshal(map[string]any{
		"query": reviewStatusQuery,
		"variables": map[string]any{
			"owner":  owner,
			"repo":   repo,
			"number": number,
		},
	})
	if err != nil {
		return merge.ReviewStatus{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/graphql", bytes.NewReader(payload))
	if err != nil {
		return merge.ReviewStatus{}, err
	}
	req.Header.Set("Authorization", "bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return merge.ReviewStatus{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return merge.ReviewStatus{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return merge.ReviewStatus{}, fmt.Errorf("graphql %s: %s", resp.Status, string(body))
	}

	var out struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewDecision           string `json:"reviewDecision"`
					LatestOpinionatedReviews struct {
						Nodes []struct {
							State string `json:"state"`
						} `json:"nodes"`
					} `json:"latestOpinionatedReviews"`
					ReviewThreads struct {
						Nodes []struct {
							IsResolved bool `json:"isResolved"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool `json:"hasNextPage"`
						} `json:"pageInfo"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return merge.ReviewStatus{}, fmt.Errorf("decode graphql response: %w", err)
	}
	if len(out.Errors) > 0 {
		return merge.ReviewStatus{}, fmt.Errorf("graphql: %s", out.Errors[0].Message)
	}

	pr := out.Data.Repository.PullRequest
	status := merge.ReviewStatus{
		ReviewDecision: pr.ReviewDecision,
		MoreThreads:    pr.ReviewThreads.PageInfo.HasNextPage,
	}
	for _, node := range pr.ReviewThreads.Nodes {
		if !node.IsResolved {
			status.UnresolvedThreads++
		}
	}
	for _, node := range pr.LatestOpinionatedReviews.Nodes {
		if node.State == "APPROVED" {
			status.Approvals++
		}
	}

	return status, nil
}
