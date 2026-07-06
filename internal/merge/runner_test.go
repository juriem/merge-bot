package merge

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/go-github/v66/github"
)

type fakeGitHub struct {
	pr          *github.PullRequest
	prErr       error
	required    []string
	requiredErr error
	runs        []CheckRun
	runsErr     error
	review      ReviewStatus
	mergeErr    error
	updateErr   error
	behindBy    int

	merged  bool
	updated bool
}

func (f *fakeGitHub) GetPullRequest(context.Context, string, string, int) (*github.PullRequest, error) {
	return f.pr, f.prErr
}

func (f *fakeGitHub) UpdateBranch(context.Context, string, string, int, string) error {
	f.updated = true
	return f.updateErr
}

func (f *fakeGitHub) Merge(context.Context, string, string, int, string) error {
	if f.mergeErr != nil {
		return f.mergeErr
	}
	f.merged = true
	return nil
}

func (f *fakeGitHub) CheckSummary(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (f *fakeGitHub) ReviewStatus(context.Context, string, string, int) (ReviewStatus, error) {
	return f.review, nil
}

func (f *fakeGitHub) RequiredChecks(context.Context, string, string, string) ([]string, error) {
	return f.required, f.requiredErr
}

func (f *fakeGitHub) CheckRuns(context.Context, string, string, string) ([]CheckRun, error) {
	return f.runs, f.runsErr
}

func (f *fakeGitHub) BehindBy(context.Context, string, string, string, string) (int, error) {
	return f.behindBy, nil
}

func openPR(state string) *github.PullRequest {
	return &github.PullRequest{
		State:          github.String("open"),
		Draft:          github.Bool(false),
		Merged:         github.Bool(false),
		MergeableState: github.String(state),
		Head:           &github.PullRequestBranch{SHA: github.String("headsha")},
		Base:           &github.PullRequestBranch{Ref: github.String("main")},
	}
}

func passedRun(name string) CheckRun {
	return CheckRun{Name: name, Completed: true, Conclusion: "success"}
}

func newRunner(f *fakeGitHub) Runner {
	return Runner{
		Client:      f,
		Owner:       "o",
		Repo:        "r",
		Number:      1,
		MergeMethod: "squash",
		Logf:        func(string, ...any) {},
	}
}

func Test_step_Merges_InCaseOfUnstableButRequiredChecksGreen(t *testing.T) {
	// Arrange
	f := &fakeGitHub{
		pr:       openPR("unstable"),
		required: []string{"build", "unit"},
		runs:     []CheckRun{passedRun("build"), passedRun("unit"), {Name: "lint", Completed: true, Conclusion: "failure"}},
	}
	r := newRunner(f)

	// Act
	done, err := r.step(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Fatal("expected done=true (merged)")
	}
	if !f.merged {
		t.Fatal("expected the PR to be merged")
	}
}

func Test_step_Declines_InCaseOfUnstableAndRequiredCheckFailed(t *testing.T) {
	// Arrange
	f := &fakeGitHub{
		pr:       openPR("unstable"),
		required: []string{"build"},
		runs:     []CheckRun{{Name: "build", Completed: true, Conclusion: "failure"}},
	}
	r := newRunner(f)

	// Act
	_, err := r.step(context.Background())

	// Assert
	if !errors.Is(err, ErrRequiredCheckFailed) {
		t.Fatalf("expected ErrRequiredCheckFailed, got: %v", err)
	}
	if f.merged {
		t.Fatal("expected the PR not to be merged")
	}
}

func Test_step_ReturnsErrConflicts_InCaseOfDirty(t *testing.T) {
	// Arrange
	f := &fakeGitHub{pr: openPR("dirty")}
	r := newRunner(f)

	// Act
	_, err := r.step(context.Background())

	// Assert
	if !errors.Is(err, ErrConflicts) {
		t.Fatalf("expected ErrConflicts, got: %v", err)
	}
	if f.merged {
		t.Fatal("expected the PR not to be merged")
	}
}

func Test_step_Waits_InCaseOfUnstableAndRequiredCheckPending(t *testing.T) {
	// Arrange
	f := &fakeGitHub{
		pr:       openPR("unstable"),
		required: []string{"build"},
		runs:     []CheckRun{{Name: "build", Completed: false}},
	}
	r := newRunner(f)

	// Act
	done, err := r.step(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Fatal("expected done=false (waiting)")
	}
	if f.merged {
		t.Fatal("expected the PR not to be merged")
	}
}

func Test_step_Waits_InCaseOfUnstableAndRequiredCheckMissing(t *testing.T) {
	// Arrange
	f := &fakeGitHub{
		pr:       openPR("unstable"),
		required: []string{"build"},
		runs:     []CheckRun{passedRun("unit")},
	}
	r := newRunner(f)

	// Act
	done, err := r.step(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Fatal("expected done=false (waiting)")
	}
	if f.merged {
		t.Fatal("expected the PR not to be merged")
	}
}

func Test_step_Merges_InCaseOfUnstableAndNoRequiredChecks(t *testing.T) {
	// Arrange
	f := &fakeGitHub{
		pr:       openPR("unstable"),
		required: nil,
		runs:     []CheckRun{{Name: "lint", Completed: true, Conclusion: "failure"}},
	}
	r := newRunner(f)

	// Act
	done, err := r.step(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Fatal("expected done=true (merged)")
	}
	if !f.merged {
		t.Fatal("expected the PR to be merged")
	}
}

func Test_step_Waits_InCaseOfUnstableAndRequiredChecksLookupFails(t *testing.T) {
	// Arrange
	f := &fakeGitHub{
		pr:          openPR("unstable"),
		requiredErr: errors.New("boom"),
	}
	r := newRunner(f)

	// Act
	done, err := r.step(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Fatal("expected done=false (waiting)")
	}
	if f.merged {
		t.Fatal("expected the PR not to be merged")
	}
}

func Test_step_Merges_InCaseOfUnstableAndAllowUnstableSkipsRequiredLookup(t *testing.T) {
	// Arrange
	f := &fakeGitHub{
		pr:          openPR("unstable"),
		requiredErr: errors.New("must not be called"),
	}
	r := newRunner(f)
	r.AllowUnstable = true

	// Act
	done, err := r.step(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Fatal("expected done=true (merged)")
	}
	if !f.merged {
		t.Fatal("expected the PR to be merged")
	}
}

func Test_step_MovesToNeedsApprovals_InCaseOfTooFewApprovals(t *testing.T) {
	// Arrange
	f := &fakeGitHub{
		pr:     openPR("clean"),
		review: ReviewStatus{Approvals: 1},
	}
	r := newRunner(f)
	r.MinApprovals = 2

	// Act
	_, err := r.step(context.Background())

	// Assert
	if !errors.Is(err, ErrInsufficientApprovals) {
		t.Fatalf("expected ErrInsufficientApprovals, got: %v", err)
	}
	if f.merged {
		t.Fatal("expected the PR not to be merged")
	}
}

func Test_step_MovesToNeedsApprovals_InCaseOfReviewRequired(t *testing.T) {
	// Arrange
	f := &fakeGitHub{
		pr:     openPR("clean"),
		review: ReviewStatus{ReviewDecision: "REVIEW_REQUIRED"},
	}
	r := newRunner(f)

	// Act
	_, err := r.step(context.Background())

	// Assert
	if !errors.Is(err, ErrInsufficientApprovals) {
		t.Fatalf("expected ErrInsufficientApprovals, got: %v", err)
	}
	if f.merged {
		t.Fatal("expected the PR not to be merged")
	}
}

func Test_step_Merges_WhenRequiredApprovalsMet(t *testing.T) {
	// Arrange
	f := &fakeGitHub{
		pr:     openPR("clean"),
		review: ReviewStatus{Approvals: 2},
	}
	r := newRunner(f)
	r.MinApprovals = 2

	// Act
	done, err := r.step(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Fatal("expected done=true (merged)")
	}
	if !f.merged {
		t.Fatal("expected the PR to be merged")
	}
}

func Test_step_Retries_InCaseOfRateLimitedGetPR(t *testing.T) {
	// Arrange
	f := &fakeGitHub{prErr: &github.RateLimitError{Message: "rate limited"}}
	r := newRunner(f)

	// Act
	done, err := r.step(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("rate limit should be transient, got error: %v", err)
	}
	if done {
		t.Fatal("expected done=false (will re-check)")
	}
}

func Test_step_Retries_InCaseOfRateLimitedMerge(t *testing.T) {
	// Arrange
	f := &fakeGitHub{
		pr:       openPR("clean"),
		mergeErr: &github.AbuseRateLimitError{Message: "secondary rate limit"},
	}
	r := newRunner(f)

	// Act
	done, err := r.step(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("rate limit should be transient, got error: %v", err)
	}
	if done {
		t.Fatal("expected done=false (will re-check)")
	}
}

func Test_step_MovesToNeedsApprovals_InCaseOfBlockedAndTooFewApprovals(t *testing.T) {
	// Arrange: branch protection reports an under-approved PR as "blocked".
	f := &fakeGitHub{
		pr:     openPR("blocked"),
		review: ReviewStatus{Approvals: 1},
	}
	r := newRunner(f)
	r.MinApprovals = 2

	// Act
	_, err := r.step(context.Background())

	// Assert
	if !errors.Is(err, ErrInsufficientApprovals) {
		t.Fatalf("expected ErrInsufficientApprovals, got: %v", err)
	}
}

func Test_step_Declines_InCaseOfBlockedWithFailedCheck(t *testing.T) {
	// Arrange: approved but blocked, and a required check has failed (no pending).
	f := &fakeGitHub{
		pr:     openPR("blocked"),
		review: ReviewStatus{Approvals: 2},
		runs: []CheckRun{
			passedRun("unit"),
			{Name: "frontend-api", Completed: true, Conclusion: "failure"},
		},
	}
	r := newRunner(f)
	r.MinApprovals = 2

	// Act
	_, err := r.step(context.Background())

	// Assert
	if !errors.Is(err, ErrRequiredCheckFailed) {
		t.Fatalf("expected ErrRequiredCheckFailed, got: %v", err)
	}
}

func Test_step_UpdatesBranch_InCaseOfBlockedFailedCheckButBehind(t *testing.T) {
	// Arrange: approved, a check failed with nothing pending, but the branch is
	// behind base (the #7416 case) — update it so CI re-runs, don't declare dead.
	f := &fakeGitHub{
		pr:       openPR("blocked"),
		review:   ReviewStatus{Approvals: 2},
		runs:     []CheckRun{{Name: "aikido", Completed: true, Conclusion: "failure"}},
		behindBy: 4,
	}
	r := newRunner(f)
	r.MinApprovals = 2

	// Act
	done, err := r.step(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Fatal("expected done=false (updating, not declining)")
	}
	if !f.updated {
		t.Fatal("expected the branch to be updated to re-run CI")
	}
}

func Test_step_Waits_InCaseOfBlockedFailedCheckWithPending(t *testing.T) {
	// Arrange: one check failed but others are still running. The failed one may
	// be non-required (the required list is hidden), so the bot must keep waiting
	// instead of bouncing the PR out of the queue.
	f := &fakeGitHub{
		pr:     openPR("blocked"),
		review: ReviewStatus{Approvals: 2},
		runs: []CheckRun{
			{Name: "pre-commit", Completed: true, Conclusion: "failure"},
			{Name: "svc-a", Completed: false},
			{Name: "svc-b", Completed: false},
		},
	}
	r := newRunner(f)
	r.MinApprovals = 2

	// Act
	done, err := r.step(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("expected to keep waiting, got error: %v", err)
	}
	if done {
		t.Fatal("expected done=false (waiting for pending checks)")
	}
}

func Test_step_Waits_InCaseOfBlockedWithPendingCheck(t *testing.T) {
	// Arrange: blocked, approved, but a check is still running.
	f := &fakeGitHub{
		pr:     openPR("blocked"),
		review: ReviewStatus{Approvals: 2},
		runs:   []CheckRun{passedRun("unit"), {Name: "build", Completed: false}},
	}
	r := newRunner(f)
	r.MinApprovals = 2

	// Act
	done, err := r.step(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Fatal("expected done=false (waiting on pending check)")
	}
}

func Test_step_Surfaces_InCaseOfBlockedAllChecksGreen(t *testing.T) {
	// Arrange: approved, all checks green, yet still blocked (e.g. a
	// require_last_push_approval gate the bot cannot satisfy).
	f := &fakeGitHub{
		pr:     openPR("blocked"),
		review: ReviewStatus{Approvals: 2},
		runs:   []CheckRun{passedRun("unit"), passedRun("build")},
	}
	r := newRunner(f)
	r.MinApprovals = 2

	// Act
	_, err := r.step(context.Background())

	// Assert
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("expected ErrBlocked, got: %v", err)
	}
}

func Test_step_Retries_InCaseOfStaleHeadUpdateBranch(t *testing.T) {
	// Arrange: update-branch loses the head-SHA race (GitHub 422).
	f := &fakeGitHub{
		pr: openPR("behind"),
		updateErr: &github.ErrorResponse{
			Response: &http.Response{StatusCode: http.StatusUnprocessableEntity},
			Message:  "expected head sha didn't match current head ref",
		},
	}
	r := newRunner(f)

	// Act
	done, err := r.step(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("stale head should be transient, got error: %v", err)
	}
	if done {
		t.Fatal("expected done=false (will re-check)")
	}
}

func Test_CountChecks(t *testing.T) {
	runs := []CheckRun{
		passedRun("a"),
		{Name: "b", Completed: false},
		{Name: "c", Completed: true, Conclusion: "failure"},
		{Name: "d", Completed: true, Conclusion: "neutral"},
		{Name: "e", Completed: true, Conclusion: "timed_out"},
	}
	pending, failed := CountChecks(runs)
	if pending != 1 || failed != 2 {
		t.Fatalf("CountChecks = (pending %d, failed %d), want (1, 2)", pending, failed)
	}
}

func Test_ApprovalsMet(t *testing.T) {
	cases := []struct {
		name   string
		status ReviewStatus
		min    int
		want   bool
	}{
		{"enough", ReviewStatus{Approvals: 2}, 2, true},
		{"too few", ReviewStatus{Approvals: 1}, 2, false},
		{"review required despite count", ReviewStatus{Approvals: 2, ReviewDecision: "REVIEW_REQUIRED"}, 2, false},
		{"changes requested despite count", ReviewStatus{Approvals: 2, ReviewDecision: "CHANGES_REQUESTED"}, 2, false},
		{"min zero is always met", ReviewStatus{}, 0, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ApprovalsMet(c.status, c.min); got != c.want {
				t.Fatalf("ApprovalsMet = %v, want %v", got, c.want)
			}
		})
	}
}

func Test_step_Waits_InCaseOfUnstableRequiredGreenButChangesRequested(t *testing.T) {
	// Arrange
	f := &fakeGitHub{
		pr:       openPR("unstable"),
		required: []string{"build"},
		runs:     []CheckRun{passedRun("build")},
		review:   ReviewStatus{ReviewDecision: "CHANGES_REQUESTED"},
	}
	r := newRunner(f)

	// Act
	done, err := r.step(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Fatal("expected done=false (waiting on review gate)")
	}
	if f.merged {
		t.Fatal("expected the PR not to be merged")
	}
}
