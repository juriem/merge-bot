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
	"github.com/wallester/mergebot/internal/merge"
)

// Client is a thin wrapper around go-github exposing only the operations the
// merge runner relies on.
type Client struct {
	gh    *github.Client
	http  *http.Client
	token string
}

// New builds a Client authenticated with the given personal access token.
func New(token string) *Client {
	return &Client{
		gh:    github.NewClient(nil).WithAuthToken(token),
		http:  &http.Client{Timeout: 30 * time.Second},
		token: token,
	}
}

// GetPullRequest fetches the current state of a pull request.
func (c *Client) GetPullRequest(ctx context.Context, owner, repo string, number int) (*github.PullRequest, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, number)

	return pr, err
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
	`reviewDecision reviewThreads(first:100){nodes{isResolved}pageInfo{hasNextPage}}}}}`

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
					ReviewDecision string `json:"reviewDecision"`
					ReviewThreads  struct {
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

	return status, nil
}
