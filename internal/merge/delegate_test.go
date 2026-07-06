package merge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v66/github"
)

// fakeQueueClient serves scripted sequences of PR snapshots and check runs, one
// per call (the last element repeats). Comments are returned as-is; posting a
// comment records it and can trigger extra comments (bot replies).
type fakeQueueClient struct {
	snapshots []*github.PullRequest
	runsSeq   [][]CheckRun
	comments  []Comment
	replyWith []Comment // appended to comments when a command is posted

	prCall   int
	runsCall int
	posted   []string
}

func (f *fakeQueueClient) GetPullRequest(context.Context, string, string, int) (*github.PullRequest, error) {
	i := f.prCall
	if i >= len(f.snapshots) {
		i = len(f.snapshots) - 1
	}
	f.prCall++
	return f.snapshots[i], nil
}

func (f *fakeQueueClient) CheckRuns(context.Context, string, string, string) ([]CheckRun, error) {
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

func (f *fakeQueueClient) CreateComment(_ context.Context, _, _ string, _ int, body string) error {
	f.posted = append(f.posted, body)
	f.comments = append(f.comments, f.replyWith...)
	f.replyWith = nil
	return nil
}

func (f *fakeQueueClient) ListComments(context.Context, string, string, int, time.Time) ([]Comment, error) {
	return f.comments, nil
}

func delegatePR(state string, merged bool) *github.PullRequest {
	return &github.PullRequest{
		State:  github.String(state),
		Merged: github.Bool(merged),
		Head:   &github.PullRequestBranch{SHA: github.String("headsha")},
	}
}

func botComment(body string, at time.Time) Comment {
	return Comment{Author: "wallester-releases", Body: body, CreatedAt: at}
}

func newDelegate(f *fakeQueueClient) DelegateRunner {
	return DelegateRunner{
		Client:   f,
		Owner:    "o",
		Repo:     "r",
		Number:   1,
		Interval: time.Millisecond,
		Logf:     func(string, ...any) {},
	}
}

func Test_Delegate_QueuesThenFinishesOnMerge(t *testing.T) {
	// Arrange: open with green checks → /queue posted → confirmed → merged.
	f := &fakeQueueClient{
		snapshots: []*github.PullRequest{
			delegatePR("open", false),
			delegatePR("open", false),
			delegatePR("closed", true),
		},
		replyWith: []Comment{botComment("This PR: waiting at queue position `2` of `2` currently waiting PRs", time.Now().Add(time.Hour))},
	}

	// Act
	err := newDelegate(f).Run(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.posted) != 1 || f.posted[0] != "/queue" {
		t.Fatalf("posted = %v, want [/queue]", f.posted)
	}
}

func Test_Delegate_WaitsForPendingChecksBeforeQueueing(t *testing.T) {
	// Arrange: a check is still running on the first pass; /queue must wait.
	f := &fakeQueueClient{
		snapshots: []*github.PullRequest{
			delegatePR("open", false),
			delegatePR("open", false),
			delegatePR("closed", true),
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
	if len(f.posted) != 1 {
		t.Fatalf("posted = %v, want exactly one /queue after checks went green", f.posted)
	}
	if f.runsCall < 2 {
		t.Fatalf("expected at least 2 check polls, got %d", f.runsCall)
	}
}

func Test_Delegate_DeclinesQueueingOnFailedChecks(t *testing.T) {
	// Arrange: a completed failure — the bot would reject, so don't even ask.
	f := &fakeQueueClient{
		snapshots: []*github.PullRequest{delegatePR("open", false)},
		runsSeq:   [][]CheckRun{{{Name: "aikido", Completed: true, Conclusion: "failure"}}},
	}

	// Act
	err := newDelegate(f).Run(context.Background())

	// Assert
	if err == nil {
		t.Fatal("expected an error for red checks")
	}
	if len(f.posted) != 0 {
		t.Fatalf("posted = %v, want none", f.posted)
	}
}

func Test_Delegate_FailsOnBotRejection(t *testing.T) {
	// Arrange: /queue posted, the bot replies "Could not queue PR".
	f := &fakeQueueClient{
		snapshots: []*github.PullRequest{delegatePR("open", false)},
		replyWith: []Comment{botComment("Could not queue PR:\n\nPR checks are not green", time.Now().Add(time.Hour))},
	}

	// Act
	err := newDelegate(f).Run(context.Background())

	// Assert
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected a rejection error, got: %v", err)
	}
}

func Test_Delegate_WatchesWithoutAskingWhenAlreadyQueued(t *testing.T) {
	// Arrange: a fresh queued-confirmation already exists (queued via GitHub) —
	// the runner must not post /queue again, just watch until merge.
	f := &fakeQueueClient{
		snapshots: []*github.PullRequest{
			delegatePR("open", false),
			delegatePR("closed", true),
		},
		comments: []Comment{botComment("This PR: waiting at queue position `1` of `1` currently waiting PRs", time.Now().Add(-time.Minute))},
	}

	// Act
	err := newDelegate(f).Run(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.posted) != 0 {
		t.Fatalf("posted = %v, want none (already queued)", f.posted)
	}
}

func Test_Delegate_FailsWhenClosedWithoutMerge(t *testing.T) {
	// Arrange
	f := &fakeQueueClient{snapshots: []*github.PullRequest{delegatePR("closed", false)}}

	// Act
	err := newDelegate(f).Run(context.Background())

	// Assert
	if err == nil {
		t.Fatal("expected an error for a closed, unmerged PR")
	}
}

func Test_QueuedFromComments(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name     string
		comments []Comment
		want     bool
	}{
		{"no signals", []Comment{botComment("Task linked: WA-1", now)}, false},
		{"queued", []Comment{botComment("This PR: waiting at queue position `1`", now)}, true},
		{"queued then rejected", []Comment{
			botComment("This PR: waiting at queue position `1`", now.Add(-time.Hour)),
			botComment("Could not queue PR: checks are not green", now),
		}, false},
		{"rejected then queued", []Comment{
			botComment("Could not queue PR: checks are not green", now.Add(-time.Hour)),
			botComment("This PR: waiting at queue position `1`", now),
		}, true},
		{"queued then not-in-queue", []Comment{
			botComment("This PR: waiting at queue position `1`", now.Add(-time.Hour)),
			botComment("`o/r#1` is not currently in the custom merge queue.", now),
		}, false},
		{"batch in progress", []Comment{
			botComment("This PR: waiting at queue position `1`", now.Add(-time.Hour)),
			botComment("Validation batch `batch-1` started on `mq/main/batch-1` at `abc`.", now),
		}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := QueuedFromComments(c.comments); got != c.want {
				t.Fatalf("QueuedFromComments = %v, want %v", got, c.want)
			}
		})
	}
}
