package merge

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-github/v66/github"
)

type fakeGitHub struct {
	pr          *github.PullRequest
	required    []string
	requiredErr error
	runs        []CheckRun
	runsErr     error
	review      ReviewStatus

	merged  bool
	updated bool
}

func (f *fakeGitHub) GetPullRequest(context.Context, string, string, int) (*github.PullRequest, error) {
	return f.pr, nil
}

func (f *fakeGitHub) UpdateBranch(context.Context, string, string, int, string) error {
	f.updated = true
	return nil
}

func (f *fakeGitHub) Merge(context.Context, string, string, int, string) error {
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

func Test_step_Waits_InCaseOfUnstableAndRequiredCheckFailed(t *testing.T) {
	// Arrange
	f := &fakeGitHub{
		pr:       openPR("unstable"),
		required: []string{"build"},
		runs:     []CheckRun{{Name: "build", Completed: true, Conclusion: "failure"}},
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
