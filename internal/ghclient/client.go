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
