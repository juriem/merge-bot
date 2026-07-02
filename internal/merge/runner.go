// Package merge contains the polling loop that drives a pull request to a
// merged state: it updates the branch when it falls behind the base and merges
// as soon as GitHub reports the PR is ready.
package merge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/go-github/v66/github"
)

// Terminal conditions the poll loop cannot resolve on its own. They are wrapped
// into the returned error so callers (e.g. the queue) can route the PR to the
// right bucket instead of retrying until the timeout.
var (
	// ErrConflicts marks a PR with merge conflicts: it needs a manual rebase,
	// so the bot stops and sets it aside rather than polling.
	ErrConflicts = errors.New("merge conflicts")

	// ErrRequiredCheckFailed marks a required check that completed unsuccessfully
	// while the branch is up to date (state "unstable"): there is nothing to
	// update against base, so the check will not recover by waiting.
	ErrRequiredCheckFailed = errors.New("required check failed")

	// ErrInsufficientApprovals marks a PR that lacks the required number of
	// approving reviews. Waiting on approvals would block the single-worker
	// queue, so the PR is moved aside to be re-queued once it has been reviewed.
	ErrInsufficientApprovals = errors.New("insufficient approvals")

	// ErrBlocked marks a PR that GitHub reports as "blocked" for a branch-
	// protection reason the bot cannot satisfy (e.g. require_last_push_approval)
	// with no checks pending. Nothing the poll loop does will clear it, so it is
	// surfaced instead of spun until the timeout.
	ErrBlocked = errors.New("blocked by branch protection")
)

// ReviewStatus captures the review-side gates that GitHub's mergeable_state may
// not surface on its own: unresolved review conversations, the overall review
// decision, and the number of approving reviews.
type ReviewStatus struct {
	UnresolvedThreads int
	MoreThreads       bool   // true when the PR has more threads than we fetched
	ReviewDecision    string // APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED or ""
	Approvals         int    // reviewers whose latest opinionated review is an approval
}

// CheckRun is the state of a single check on the head commit, reduced to what
// the required-checks gate needs.
type CheckRun struct {
	Name       string
	Completed  bool   // false while the check is still queued or running
	Conclusion string // valid once Completed: success, failure, skipped, ...
}

// GitHub is the subset of the GitHub API the runner depends on.
type GitHub interface {
	GetPullRequest(ctx context.Context, owner, repo string, number int) (*github.PullRequest, error)
	UpdateBranch(ctx context.Context, owner, repo string, number int, expectedHeadSHA string) error
	Merge(ctx context.Context, owner, repo string, number int, method string) error
	CheckSummary(ctx context.Context, owner, repo, ref string) (string, error)
	ReviewStatus(ctx context.Context, owner, repo string, number int) (ReviewStatus, error)
	RequiredChecks(ctx context.Context, owner, repo, branch string) ([]string, error)
	CheckRuns(ctx context.Context, owner, repo, ref string) ([]CheckRun, error)
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
	MinApprovals    int
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
		if isRateLimited(err) {
			r.Logf("rate limited fetching PR #%d: %v; will re-check", r.Number, err)
			return false, nil
		}
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
		return r.tryMergeUnstable(ctx, pr)
	case "behind":
		return false, r.update(ctx, pr.GetHead().GetSHA())
	case "blocked":
		return r.handleBlocked(ctx, pr)
	case "dirty":
		return false, fmt.Errorf("PR #%d has merge conflicts; resolve them manually: %w", r.Number, ErrConflicts)
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

// tryMerge applies the review gate (unless disabled) before merging. Most gate
// failures are transient — logged and left for the next poll — but a missing-
// approvals failure is returned so the queue can park the PR.
func (r Runner) tryMerge(ctx context.Context) (bool, error) {
	if !r.AllowUnresolved {
		ok, reason, err := r.reviewGate(ctx)
		if err != nil {
			if errors.Is(err, ErrInsufficientApprovals) {
				return false, err
			}
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

// handleBlocked deals with a PR GitHub reports as "blocked" (a required gate is
// unmet). The bot cannot merge it, so it reacts by inspecting the checks (this
// works even when the required-check list is hidden behind rulesets):
//   - under-approved            → park for approvals;
//   - a check has failed        → park in the Blocked lane (a failed check keeps
//     the PR blocked no matter how many others are still pending); the background
//     recheck re-queues it once the block clears (check re-run / branch updated);
//   - no failures, some pending → wait for them to finish;
//   - all green but still blocked → held by a gate the bot can't satisfy
//     (e.g. require_last_push_approval) → park; recheck re-queues when it clears.
func (r Runner) handleBlocked(ctx context.Context, pr *github.PullRequest) (bool, error) {
	if !r.AllowUnresolved {
		if _, _, err := r.reviewGate(ctx); errors.Is(err, ErrInsufficientApprovals) {
			return false, err
		}
	}

	head := pr.GetHead().GetSHA()
	runs, err := r.Client.CheckRuns(ctx, r.Owner, r.Repo, head)
	if err != nil {
		r.Logf("blocked; could not list checks: %v; will re-check", err)
		return false, nil
	}

	pending, failed := CountChecks(runs)

	switch {
	case pending > 0:
		// A failure alongside still-running checks is not a verdict: the failed
		// one may be non-required (the required list is hidden behind rulesets)
		// while required ones are still in flight. Once everything required is
		// green GitHub flips the state to unstable/clean and the merge proceeds;
		// declining now would bounce the PR out of the queue prematurely.
		r.Logf("blocked: %d check(s) still running (%d failed so far); waiting", pending, failed)
		return false, nil
	case failed > 0:
		r.logBlockers(ctx, head)
		return false, fmt.Errorf("PR #%d blocked by %d failed check(s): %w", r.Number, failed, ErrRequiredCheckFailed)
	default:
		return false, fmt.Errorf("PR #%d is blocked by branch protection and cannot be merged automatically: %w", r.Number, ErrBlocked)
	}
}

// tryMergeUnstable handles an "unstable" PR: GitHub reports it mergeable but
// some check is red or pending. We merge when every *required* check is green,
// ignoring non-required ones. --allow-unstable forces the merge without
// inspecting required checks (the old blunt behaviour).
func (r Runner) tryMergeUnstable(ctx context.Context, pr *github.PullRequest) (bool, error) {
	if r.AllowUnstable {
		return r.tryMerge(ctx)
	}

	ok, reason, err := r.requiredChecksGreen(ctx, pr.GetBase().GetRef(), pr.GetHead().GetSHA())
	if err != nil {
		if errors.Is(err, ErrRequiredCheckFailed) {
			return false, err
		}
		r.Logf("could not verify required checks: %v; will re-check", err)
		return false, nil
	}
	if !ok {
		r.Logf("waiting: %s", reason)
		return false, nil
	}

	r.Logf("all required checks pass; ignoring non-required checks")
	return r.tryMerge(ctx)
}

// requiredChecksGreen reports whether every status check that branch protection
// requires on baseBranch has a successful run on the head commit. With no
// required checks configured the gate passes (nothing to wait for).
func (r Runner) requiredChecksGreen(ctx context.Context, baseBranch, headSHA string) (bool, string, error) {
	required, err := r.Client.RequiredChecks(ctx, r.Owner, r.Repo, baseBranch)
	if err != nil {
		return false, "", err
	}
	if len(required) == 0 {
		return true, "", nil
	}

	runs, err := r.Client.CheckRuns(ctx, r.Owner, r.Repo, headSHA)
	if err != nil {
		return false, "", err
	}

	byName := make(map[string]CheckRun, len(runs))
	for _, run := range runs {
		byName[run.Name] = run
	}

	for _, name := range required {
		run, ok := byName[name]
		if !ok || !run.Completed {
			return false, fmt.Sprintf("required check %q has not completed", name), nil
		}
		if !checkSucceeded(run.Conclusion) {
			return false, "", fmt.Errorf("required check %q concluded %q: %w", name, run.Conclusion, ErrRequiredCheckFailed)
		}
	}

	return true, "", nil
}

func checkSucceeded(conclusion string) bool {
	switch conclusion {
	case "success", "skipped", "neutral":
		return true
	default:
		return false
	}
}

// CountChecks tallies check runs that are still pending and ones that completed
// unsuccessfully. Shared by the runner and the review dashboard.
func CountChecks(runs []CheckRun) (pending, failed int) {
	for _, run := range runs {
		switch {
		case !run.Completed:
			pending++
		case !checkSucceeded(run.Conclusion):
			failed++
		}
	}

	return pending, failed
}

func (r Runner) reviewGate(ctx context.Context) (ok bool, reason string, err error) {
	status, err := r.Client.ReviewStatus(ctx, r.Owner, r.Repo, r.Number)
	if err != nil {
		return false, "", err
	}

	// Missing approvals route the PR out of the queue (terminal) so the worker
	// is not blocked waiting on humans. This is checked before the unresolved-
	// thread hold so an under-approved PR is moved aside even when it also has
	// open threads.
	if status.ReviewDecision == "REVIEW_REQUIRED" {
		return false, "", fmt.Errorf("review required: %w", ErrInsufficientApprovals)
	}
	if status.Approvals < r.MinApprovals {
		return false, "", fmt.Errorf("need %d approval(s), have %d: %w", r.MinApprovals, status.Approvals, ErrInsufficientApprovals)
	}

	if status.ReviewDecision == "CHANGES_REQUESTED" {
		return false, "changes requested by a reviewer", nil
	}

	if status.UnresolvedThreads > 0 {
		more := ""
		if status.MoreThreads {
			more = "+"
		}
		return false, fmt.Sprintf("%d%s unresolved review thread(s)", status.UnresolvedThreads, more), nil
	}

	return true, "", nil
}

// ApprovalsMet reports whether a review status satisfies the approval gate for
// the given minimum. It is used by the queue to decide when a PR parked for
// missing approvals is ready to re-enter the main queue.
func ApprovalsMet(status ReviewStatus, minApprovals int) bool {
	switch status.ReviewDecision {
	case "REVIEW_REQUIRED", "CHANGES_REQUESTED":
		return false
	}

	return status.Approvals >= minApprovals
}

// isRateLimited reports whether err is a GitHub primary or secondary rate-limit
// error. Such errors are transient: the PR is left in place and re-checked on
// the next poll rather than failed.
func isRateLimited(err error) bool {
	var primary *github.RateLimitError
	var secondary *github.AbuseRateLimitError

	return errors.As(err, &primary) || errors.As(err, &secondary)
}

// isStaleHeadUpdate reports whether an update-branch call failed because the PR
// head moved since we read it (GitHub replies 422 "expected head sha didn't
// match"). It is benign — a concurrent/earlier update already advanced the head
// — so the PR is re-checked on the next poll instead of failed.
func isStaleHeadUpdate(err error) bool {
	var resp *github.ErrorResponse

	return errors.As(err, &resp) && resp.Response != nil && resp.Response.StatusCode == http.StatusUnprocessableEntity
}

func (r Runner) merge(ctx context.Context) (bool, error) {
	if r.DryRun {
		r.Logf("[dry-run] would merge PR #%d via %s", r.Number, r.MergeMethod)
		return true, nil
	}

	r.Logf("merging PR #%d via %s", r.Number, r.MergeMethod)
	if err := r.Client.Merge(ctx, r.Owner, r.Repo, r.Number, r.MergeMethod); err != nil {
		if isRateLimited(err) {
			r.Logf("rate limited merging PR #%d: %v; will re-check", r.Number, err)
			return false, nil
		}
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
		switch {
		case isRateLimited(err):
			r.Logf("rate limited updating PR #%d: %v; will re-check", r.Number, err)
			return nil
		case isStaleHeadUpdate(err):
			r.Logf("PR #%d head moved before update-branch; will re-check", r.Number)
			return nil
		default:
			return fmt.Errorf("update branch of PR #%d: %w", r.Number, err)
		}
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
