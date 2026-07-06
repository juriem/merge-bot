package merge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v66/github"
)

// fakeLabelClient serves scripted sequences of PR snapshots and check runs, one
// per call (the last element repeats).
type fakeLabelClient struct {
	snapshots []*github.PullRequest
	runsSeq   [][]CheckRun
	comments  []Comment

	prCall   int
	runsCall int
	added    []string
	removed  []string
}

func (f *fakeLabelClient) GetPullRequest(context.Context, string, string, int) (*github.PullRequest, error) {
	i := f.prCall
	if i >= len(f.snapshots) {
		i = len(f.snapshots) - 1
	}
	f.prCall++
	return f.snapshots[i], nil
}

func (f *fakeLabelClient) AddLabel(_ context.Context, _, _ string, _ int, label string) error {
	f.added = append(f.added, label)
	return nil
}

func (f *fakeLabelClient) RemoveLabel(_ context.Context, _, _ string, _ int, label string) error {
	f.removed = append(f.removed, label)
	return nil
}

func (f *fakeLabelClient) CheckRuns(context.Context, string, string, string) ([]CheckRun, error) {
	if len(f.runsSeq) == 0 {
		return nil, nil // no checks at all counts as green
	}
	i := f.runsCall
	if i >= len(f.runsSeq) {
		i = len(f.runsSeq) - 1
	}
	f.runsCall++
	return f.runsSeq[i], nil
}

func (f *fakeLabelClient) ListComments(context.Context, string, string, int, time.Time) ([]Comment, error) {
	return f.comments, nil
}

func delegatePR(state string, merged bool, labels ...string) *github.PullRequest {
	pr := &github.PullRequest{
		State:  github.String(state),
		Merged: github.Bool(merged),
		Head:   &github.PullRequestBranch{SHA: github.String("headsha")},
	}
	for _, l := range labels {
		pr.Labels = append(pr.Labels, &github.Label{Name: github.String(l)})
	}
	return pr
}

func newDelegate(f *fakeLabelClient) DelegateRunner {
	return DelegateRunner{
		Client:   f,
		Owner:    "o",
		Repo:     "r",
		Number:   1,
		Label:    "merge-queue",
		Interval: time.Millisecond,
		Logf:     func(string, ...any) {},
	}
}

func Test_Delegate_LabelsThenFinishesOnMerge(t *testing.T) {
	// Arrange: unlabeled (checks green) → labeled (by us) → merged.
	f := &fakeLabelClient{snapshots: []*github.PullRequest{
		delegatePR("open", false),
		delegatePR("open", false, "merge-queue"),
		delegatePR("closed", true, "merge-queue"),
	}}

	// Act
	err := newDelegate(f).Run(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.added) != 1 || f.added[0] != "merge-queue" {
		t.Fatalf("labels added = %v, want [merge-queue]", f.added)
	}
}

func Test_Delegate_WaitsForPendingChecksBeforeHandover(t *testing.T) {
	// Arrange: a check is still running on the first pass; the handover must
	// wait for it and only label once everything is green.
	f := &fakeLabelClient{
		snapshots: []*github.PullRequest{
			delegatePR("open", false),
			delegatePR("open", false),
			delegatePR("closed", true, "merge-queue"),
		},
		runsSeq: [][]CheckRun{
			{{Name: "ci", Completed: false}},
			{{Name: "ci", Completed: true, Conclusion: "success"}},
		},
	}

	// Act
	err := newDelegate(f).Run(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.added) != 1 {
		t.Fatalf("labels added = %v, want exactly one after checks went green", f.added)
	}
	if f.runsCall < 2 {
		t.Fatalf("expected at least 2 check polls, got %d", f.runsCall)
	}
}

func Test_Delegate_DeclinesHandoverOnFailedChecks(t *testing.T) {
	// Arrange: a check has failed with nothing pending — the queue bot would
	// reject the PR, so the handover must not happen at all.
	f := &fakeLabelClient{
		snapshots: []*github.PullRequest{delegatePR("open", false)},
		runsSeq:   [][]CheckRun{{{Name: "aikido", Completed: true, Conclusion: "failure"}}},
	}

	// Act
	err := newDelegate(f).Run(context.Background())

	// Assert
	if err == nil {
		t.Fatal("expected an error for red checks")
	}
	if len(f.added) != 0 {
		t.Fatalf("labels added = %v, want none", f.added)
	}
}

func Test_Delegate_FailsOnBotRejectionAndRemovesLabel(t *testing.T) {
	// Arrange: we label the PR, then the queue bot replies "Could not queue PR".
	f := &fakeLabelClient{
		snapshots: []*github.PullRequest{
			delegatePR("open", false),
			delegatePR("open", false, "merge-queue"),
		},
		comments: []Comment{{
			Author:    "wallester-releases",
			Body:      "Could not queue PR:\n\nPR checks are not green",
			CreatedAt: time.Now().Add(time.Hour),
		}},
	}

	// Act
	err := newDelegate(f).Run(context.Background())

	// Assert
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected a rejection error, got: %v", err)
	}
	if len(f.removed) != 1 || f.removed[0] != "merge-queue" {
		t.Fatalf("labels removed = %v, want [merge-queue] so a retry re-triggers the bot", f.removed)
	}
}

func Test_Delegate_DoesNotRelabelWhenAlreadyQueued(t *testing.T) {
	// Arrange: the label is already there (queued via GitHub), then merged.
	f := &fakeLabelClient{snapshots: []*github.PullRequest{
		delegatePR("open", false, "merge-queue"),
		delegatePR("closed", true, "merge-queue"),
	}}

	// Act
	err := newDelegate(f).Run(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.added) != 0 {
		t.Fatalf("labels added = %v, want none", f.added)
	}
}

func Test_Delegate_FailsWhenDequeued(t *testing.T) {
	// Arrange: labeled, then the label disappears while the PR is still open.
	f := &fakeLabelClient{snapshots: []*github.PullRequest{
		delegatePR("open", false, "merge-queue"),
		delegatePR("open", false),
	}}

	// Act
	err := newDelegate(f).Run(context.Background())

	// Assert
	if err == nil {
		t.Fatal("expected an error after the label was removed")
	}
}

func Test_Delegate_FailsWhenClosedWithoutMerge(t *testing.T) {
	// Arrange
	f := &fakeLabelClient{snapshots: []*github.PullRequest{
		delegatePR("closed", false, "merge-queue"),
	}}

	// Act
	err := newDelegate(f).Run(context.Background())

	// Assert
	if err == nil {
		t.Fatal("expected an error for a closed, unmerged PR")
	}
}
