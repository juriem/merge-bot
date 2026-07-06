package merge

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-github/v66/github"
)

// fakeLabelClient serves a scripted sequence of PR snapshots, one per poll.
type fakeLabelClient struct {
	snapshots []*github.PullRequest
	call      int
	added     []string
	removed   []string
}

func (f *fakeLabelClient) GetPullRequest(context.Context, string, string, int) (*github.PullRequest, error) {
	i := f.call
	if i >= len(f.snapshots) {
		i = len(f.snapshots) - 1
	}
	f.call++
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

func delegatePR(state string, merged bool, labels ...string) *github.PullRequest {
	pr := &github.PullRequest{
		State:  github.String(state),
		Merged: github.Bool(merged),
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
	// Arrange: unlabeled → labeled (by us) → merged.
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
