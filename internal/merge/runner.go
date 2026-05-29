// Package merge contains the polling loop that drives a pull request to a
// merged state: it updates the branch when it falls behind the base and merges
// as soon as GitHub reports the PR is ready.
package merge

import (
	"context"
	"fmt"
	"time"

	"github.com/google/go-github/v66/github"
)

// ReviewStatus captures the review-side gates that GitHub's mergeable_state may
// not enforce on its own (depending on branch protection): unresolved review
// conversations and the overall review decision.
type ReviewStatus struct {
	UnresolvedThreads int
	MoreThreads       bool   // true when the PR has more threads than we fetched
	ReviewDecision    string // APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED or ""
}

// GitHub is the subset of the GitHub API the runner depends on.
type GitHub interface {
	GetPullRequest(ctx context.Context, owner, repo string, number int) (*github.PullRequest, error)
	UpdateBranch(ctx context.Context, owner, repo string, number int, expectedHeadSHA string) error
	Merge(ctx context.Context, owner, repo string, number int, method string) error
	CheckSummary(ctx context.Context, owner, repo, ref string) (string, error)
	ReviewStatus(ctx context.Context, owner, repo string, number int) (ReviewStatus, error)
}

// Runner repeatedly inspects a pull request and brings it to a merged state.
type Runner struct {
	Client          GitHub
	Owner           string
	Repo            string
	Number          int
	Interval        time.Duration
	Timeout         time.Duration
	MergeMethod     string
	AllowUnstable   bool
	AllowUnresolved bool
	DryRun          bool
	Logf            func(format string, args ...any)
}

// Run polls the pull request until it is merged, a terminal problem is hit
// (conflicts, closed, draft), the timeout elapses or the context is cancelled.
func (r Runner) Run(ctx context.Context) error {
	deadline := time.Now().Add(r.Timeout)

	for {
		done, err := r.step(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}

		if r.DryRun {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("gave up after %s without merging PR #%d", r.Timeout, r.Number)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.Interval):
		}
	}
}

// step performs one inspection of the PR. It returns done=true once the PR is
// merged (or would be, in dry-run), and a non-nil error for terminal problems.
func (r Runner) step(ctx context.Context) (bool, error) {
	pr, err := r.Client.GetPullRequest(ctx, r.Owner, r.Repo, r.Number)
	if err != nil {
		return false, fmt.Errorf("get PR #%d: %w", r.Number, err)
	}

	if pr.GetMerged() {
		r.Logf("PR #%d is already merged", r.Number)
		return true, nil
	}

	if pr.GetState() != "open" {
		return false, fmt.Errorf("PR #%d is %s, not open", r.Number, pr.GetState())
	}

	if pr.GetDraft() {
		return false, fmt.Errorf("PR #%d is a draft", r.Number)
	}

	state := pr.GetMergeableState()
	r.Logf("PR #%d: mergeable_state=%s", r.Number, state)

	switch state {
	case "clean", "has_hooks":
		return r.tryMerge(ctx)
	case "unstable":
		if r.AllowUnstable {
			return r.tryMerge(ctx)
		}
		r.Logf("non-required checks are red; waiting (pass --allow-unstable to merge anyway)")
		return false, nil
	case "behind":
		return false, r.update(ctx, pr.GetHead().GetSHA())
	case "blocked":
		r.logBlockers(ctx, pr.GetHead().GetSHA())
		return false, nil
	case "dirty":
		return false, fmt.Errorf("PR #%d has merge conflicts; resolve them manually", r.Number)
	case "draft":
		return false, fmt.Errorf("PR #%d is a draft", r.Number)
	case "", "unknown":
		r.Logf("GitHub is still computing mergeability; will re-check")
		return false, nil
	default:
		r.Logf("unrecognized mergeable_state %q; will re-check", state)
		return false, nil
	}
}

// tryMerge applies the review gate (unless disabled) before merging. A failed
// gate is not an error: it logs the reason and lets the poll loop re-check.
func (r Runner) tryMerge(ctx context.Context) (bool, error) {
	if !r.AllowUnresolved {
		ok, reason, err := r.reviewGate(ctx)
		if err != nil {
			r.Logf("could not check review status: %v; will re-check", err)
			return false, nil
		}
		if !ok {
			r.Logf("holding merge: %s", reason)
			return false, nil
		}
	}

	return r.merge(ctx)
}

func (r Runner) reviewGate(ctx context.Context) (ok bool, reason string, err error) {
	status, err := r.Client.ReviewStatus(ctx, r.Owner, r.Repo, r.Number)
	if err != nil {
		return false, "", err
	}

	if status.UnresolvedThreads > 0 {
		more := ""
		if status.MoreThreads {
			more = "+"
		}
		return false, fmt.Sprintf("%d%s unresolved review thread(s)", status.UnresolvedThreads, more), nil
	}

	switch status.ReviewDecision {
	case "CHANGES_REQUESTED":
		return false, "changes requested by a reviewer", nil
	case "REVIEW_REQUIRED":
		return false, "review required", nil
	}

	return true, "", nil
}

func (r Runner) merge(ctx context.Context) (bool, error) {
	if r.DryRun {
		r.Logf("[dry-run] would merge PR #%d via %s", r.Number, r.MergeMethod)
		return true, nil
	}

	r.Logf("merging PR #%d via %s", r.Number, r.MergeMethod)
	if err := r.Client.Merge(ctx, r.Owner, r.Repo, r.Number, r.MergeMethod); err != nil {
		return false, fmt.Errorf("merge PR #%d: %w", r.Number, err)
	}

	r.Logf("PR #%d merged", r.Number)
	return true, nil
}

func (r Runner) update(ctx context.Context, headSHA string) error {
	if r.DryRun {
		r.Logf("[dry-run] would update branch of PR #%d (behind base)", r.Number)
		return nil
	}

	r.Logf("branch is behind base; requesting update-branch")
	if err := r.Client.UpdateBranch(ctx, r.Owner, r.Repo, r.Number, headSHA); err != nil {
		return fmt.Errorf("update branch of PR #%d: %w", r.Number, err)
	}

	r.Logf("branch update requested; waiting for CI to re-run")
	return nil
}

func (r Runner) logBlockers(ctx context.Context, headSHA string) {
	summary, err := r.Client.CheckSummary(ctx, r.Owner, r.Repo, headSHA)
	if err != nil {
		r.Logf("blocked by required checks or missing approvals (could not list checks: %v)", err)
		return
	}

	if summary == "" {
		r.Logf("blocked: required checks pass, likely waiting for required approvals")
		return
	}

	r.Logf("blocked by checks: %s", summary)
}
