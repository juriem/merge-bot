package queue

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v66/github"
	"mergebot/internal/merge"
)

// conflictGitHub reports every PR as having merge conflicts.
type conflictGitHub struct{ merge.GitHub }

func (conflictGitHub) GetPullRequest(context.Context, string, string, int) (*github.PullRequest, error) {
	return &github.PullRequest{
		State:          github.String("open"),
		Draft:          github.Bool(false),
		Merged:         github.Bool(false),
		MergeableState: github.String("dirty"),
		Head:           &github.PullRequestBranch{SHA: github.String("headsha")},
		Base:           &github.PullRequestBranch{Ref: github.String("main")},
	}, nil
}

// teamQueueGitHub implements both merge.GitHub and merge.QueueClient: the PR
// reports merged as soon as /queue has been posted.
type teamQueueGitHub struct {
	merge.GitHub
	mu     sync.Mutex
	posted []string
}

func (g *teamQueueGitHub) GetPullRequest(context.Context, string, string, int) (*github.PullRequest, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pr := &github.PullRequest{
		State:  github.String("open"),
		Merged: github.Bool(false),
		Head:   &github.PullRequestBranch{SHA: github.String("headsha")},
	}
	if len(g.posted) > 0 {
		pr.Merged = github.Bool(true)
		pr.State = github.String("closed")
	}
	return pr, nil
}

func (g *teamQueueGitHub) CreateComment(_ context.Context, _, _ string, _ int, body string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.posted = append(g.posted, body)
	return nil
}

func (g *teamQueueGitHub) CheckRuns(context.Context, string, string, string) ([]merge.CheckRun, error) {
	return nil, nil // no checks — counts as green
}

func (g *teamQueueGitHub) ListComments(context.Context, string, string, int, time.Time) ([]merge.Comment, error) {
	return nil, nil
}

func Test_effectiveConcurrency_IgnoresLimitInMergeQueueMode(t *testing.T) {
	// self mode honours --concurrency (with a floor of 1)
	self := New(&teamQueueGitHub{}, Config{Concurrency: 3}, "", func(string, ...any) {})
	if got := self.effectiveConcurrency(); got != 3 {
		t.Fatalf("self concurrency = %d, want 3", got)
	}
	selfZero := New(&teamQueueGitHub{}, Config{Concurrency: 0}, "", func(string, ...any) {})
	if got := selfZero.effectiveConcurrency(); got != 1 {
		t.Fatalf("self concurrency floor = %d, want 1", got)
	}

	// merge-queue mode ignores --concurrency and hands everything over at once
	mq := New(&teamQueueGitHub{}, Config{Concurrency: 1, MergeMode: ModeMergeQueue}, "", func(string, ...any) {})
	if got := mq.effectiveConcurrency(); got != mergeQueueWorkers {
		t.Fatalf("merge-queue concurrency = %d, want %d", got, mergeQueueWorkers)
	}
}

func Test_process_MergeQueueModeDelegatesAndTracksMerge(t *testing.T) {
	// Arrange
	g := &teamQueueGitHub{}
	m := New(g, Config{
		Owner: "o", Repo: "r", Interval: time.Millisecond, MergeMode: ModeMergeQueue,
	}, "", func(string, ...any) {})
	it := &Item{Number: 1, Phase: PhaseQueued, AddedAt: time.Now()}

	// Act
	m.process(context.Background(), it)

	// Assert
	if it.Phase != PhaseMerged {
		t.Fatalf("expected phase %q, got %q", PhaseMerged, it.Phase)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.posted) != 1 || g.posted[0] != "/queue" {
		t.Fatalf("posted = %v, want [/queue]", g.posted)
	}
}

func Test_process_MovesConflictingPRToConflictsPhase(t *testing.T) {
	// Arrange
	m := New(conflictGitHub{}, Config{Owner: "o", Repo: "r", MergeMethod: "squash"}, "", func(string, ...any) {})
	it := &Item{Number: 1, Phase: PhaseQueued}

	// Act
	m.process(context.Background(), it)

	// Assert
	if it.Phase != PhaseConflicts {
		t.Fatalf("expected phase %q, got %q", PhaseConflicts, it.Phase)
	}
}

// underApprovedGitHub reports a clean PR that lacks approvals.
type underApprovedGitHub struct{ merge.GitHub }

func (underApprovedGitHub) GetPullRequest(context.Context, string, string, int) (*github.PullRequest, error) {
	return &github.PullRequest{
		State:          github.String("open"),
		Draft:          github.Bool(false),
		Merged:         github.Bool(false),
		MergeableState: github.String("clean"),
		Head:           &github.PullRequestBranch{SHA: github.String("headsha")},
		Base:           &github.PullRequestBranch{Ref: github.String("main")},
	}, nil
}

func (underApprovedGitHub) ReviewStatus(context.Context, string, string, int) (merge.ReviewStatus, error) {
	return merge.ReviewStatus{Approvals: 1}, nil
}

func Test_process_MovesUnderApprovedPRToNeedsApprovalsPhase(t *testing.T) {
	// Arrange
	m := New(underApprovedGitHub{}, Config{Owner: "o", Repo: "r", MergeMethod: "squash", MinApprovals: 2}, "", func(string, ...any) {})
	it := &Item{Number: 1, Phase: PhaseQueued}

	// Act
	m.process(context.Background(), it)

	// Assert
	if it.Phase != PhaseNeedsApprovals {
		t.Fatalf("expected phase %q, got %q", PhaseNeedsApprovals, it.Phase)
	}
}

// reviewedGitHub reports the requested approval count for every PR.
type reviewedGitHub struct {
	merge.GitHub
	approvals int
}

func (g reviewedGitHub) ReviewStatus(context.Context, string, string, int) (merge.ReviewStatus, error) {
	return merge.ReviewStatus{Approvals: g.approvals}, nil
}

func Test_recheckParked_RequeuesApprovedPR(t *testing.T) {
	// Arrange
	m := New(reviewedGitHub{approvals: 2}, Config{Owner: "o", Repo: "r", MinApprovals: 2}, "", func(string, ...any) {})
	it := &Item{Number: 1, Phase: PhaseNeedsApprovals}
	m.items = []*Item{it}

	// Act
	m.recheckParked(context.Background())

	// Assert
	if it.Phase != PhaseQueued {
		t.Fatalf("expected phase %q, got %q", PhaseQueued, it.Phase)
	}
}

func Test_recheckParked_LeavesStillUnapprovedPRParked(t *testing.T) {
	// Arrange
	m := New(reviewedGitHub{approvals: 1}, Config{Owner: "o", Repo: "r", MinApprovals: 2}, "", func(string, ...any) {})
	it := &Item{Number: 1, Phase: PhaseNeedsApprovals}
	m.items = []*Item{it}

	// Act
	m.recheckParked(context.Background())

	// Assert
	if it.Phase != PhaseNeedsApprovals {
		t.Fatalf("expected phase %q, got %q", PhaseNeedsApprovals, it.Phase)
	}
}

// statedGitHub reports a fixed mergeable_state for every PR.
type statedGitHub struct {
	merge.GitHub
	state string
}

func (g statedGitHub) GetPullRequest(context.Context, string, string, int) (*github.PullRequest, error) {
	return &github.PullRequest{MergeableState: github.String(g.state)}, nil
}

func Test_recheckParked_RequeuesResolvedConflict(t *testing.T) {
	// Arrange
	m := New(statedGitHub{state: "clean"}, Config{Owner: "o", Repo: "r"}, "", func(string, ...any) {})
	it := &Item{Number: 1, Phase: PhaseConflicts}
	m.items = []*Item{it}

	// Act
	m.recheckParked(context.Background())

	// Assert
	if it.Phase != PhaseQueued {
		t.Fatalf("expected phase %q, got %q", PhaseQueued, it.Phase)
	}
}

func Test_recheckParked_LeavesStillConflictingPRParked(t *testing.T) {
	// Arrange
	m := New(statedGitHub{state: "dirty"}, Config{Owner: "o", Repo: "r"}, "", func(string, ...any) {})
	it := &Item{Number: 1, Phase: PhaseConflicts}
	m.items = []*Item{it}

	// Act
	m.recheckParked(context.Background())

	// Assert
	if it.Phase != PhaseConflicts {
		t.Fatalf("expected phase %q, got %q", PhaseConflicts, it.Phase)
	}
}

func Test_recheckParked_RequeuesRecoveredFailed(t *testing.T) {
	// Arrange: a failed PR whose blocking check recovered (now clean).
	m := New(statedGitHub{state: "clean"}, Config{Owner: "o", Repo: "r"}, "", func(string, ...any) {})
	it := &Item{Number: 1, Phase: PhaseFailed}
	m.items = []*Item{it}

	// Act
	m.recheckParked(context.Background())

	// Assert
	if it.Phase != PhaseQueued {
		t.Fatalf("expected phase %q, got %q", PhaseQueued, it.Phase)
	}
}

func Test_recheckParked_LeavesStillFailedPRParked(t *testing.T) {
	// Arrange: still blocked (check still failing) → stays failed, no churn.
	m := New(statedGitHub{state: "blocked"}, Config{Owner: "o", Repo: "r"}, "", func(string, ...any) {})
	it := &Item{Number: 1, Phase: PhaseFailed}
	m.items = []*Item{it}

	// Act
	m.recheckParked(context.Background())

	// Assert
	if it.Phase != PhaseFailed {
		t.Fatalf("expected phase %q, got %q", PhaseFailed, it.Phase)
	}
}

func Test_Requeue_MovesParkedPRBackToQueuedPreservingTiming(t *testing.T) {
	// Arrange
	added := time.Now().Add(-time.Hour)
	m := New(nil, Config{}, "", func(string, ...any) {})
	m.items = []*Item{{Number: 1, Phase: PhaseConflicts, AddedAt: added}}

	// Act
	ok := m.Requeue(1)

	// Assert
	if !ok {
		t.Fatal("expected Requeue to report success")
	}
	it := m.List()[0]
	if it.Phase != PhaseQueued {
		t.Fatalf("expected phase %q, got %q", PhaseQueued, it.Phase)
	}
	if !it.AddedAt.Equal(added) {
		t.Fatalf("AddedAt changed: got %v, want %v", it.AddedAt, added)
	}
}

func Test_Requeue_IgnoresNonParkedPR(t *testing.T) {
	// Arrange
	m := New(nil, Config{}, "", func(string, ...any) {})
	m.items = []*Item{{Number: 1, Phase: PhaseMerged}}

	// Act & Assert
	if m.Requeue(1) {
		t.Fatal("expected Requeue to refuse a non-parked PR")
	}
	if m.Requeue(999) {
		t.Fatal("expected Requeue to report false for an unknown PR")
	}
}

// blockingGitHub parks every GetPullRequest until its ctx is cancelled, so a
// test can cancel mid-flight and observe the resulting phase.
type blockingGitHub struct{ merge.GitHub }

func (blockingGitHub) GetPullRequest(ctx context.Context, _, _ string, _ int) (*github.PullRequest, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func Test_process_RequeuesOnShutdownCancel(t *testing.T) {
	// Arrange: the daemon shuts down (parent ctx cancelled) while a PR is active.
	m := New(blockingGitHub{}, Config{Owner: "o", Repo: "r", Interval: time.Millisecond, Timeout: time.Minute}, "", func(string, ...any) {})
	it := &Item{Number: 1, Phase: PhaseActive, AddedAt: time.Now()}
	m.items = []*Item{it}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.process(ctx, it); close(done) }()
	cancel()
	<-done

	// Assert: back to queued so it resumes after restart, not stranded stopped.
	if it.Phase != PhaseQueued {
		t.Fatalf("expected phase %q after shutdown, got %q", PhaseQueued, it.Phase)
	}
}

func Test_process_KeepsStoppedOnUserRemove(t *testing.T) {
	// Arrange: the user removes the PR while active — Remove flips the phase to
	// stopped and cancels; the phase must stay stopped.
	m := New(blockingGitHub{}, Config{Owner: "o", Repo: "r", Interval: time.Millisecond, Timeout: time.Minute}, "", func(string, ...any) {})
	it := &Item{Number: 1, Phase: PhaseQueued, AddedAt: time.Now()}
	m.items = []*Item{it}

	claimed, procCtx := m.claim(context.Background())
	if claimed == nil {
		t.Fatal("expected to claim the queued item")
	}
	done := make(chan struct{})
	go func() { m.process(procCtx, claimed); close(done) }()
	m.Remove(1)
	<-done

	// Assert
	if it.Phase != PhaseStopped {
		t.Fatalf("expected phase %q after user remove, got %q", PhaseStopped, it.Phase)
	}
}

func Test_Clear_RemovesTerminalItemsButKeepsActive(t *testing.T) {
	// Arrange
	m := New(nil, Config{}, "", func(string, ...any) {})
	m.items = []*Item{
		{Number: 1, Phase: PhaseFailed},
		{Number: 2, Phase: PhaseActive},
		{Number: 3, Phase: PhaseFailed},
		{Number: 4, Phase: PhaseMerged},
	}

	// Act: clearing failed must not touch active or merged.
	m.Clear([]Phase{PhaseFailed, PhaseActive})

	// Assert
	got := m.List()
	if len(got) != 2 {
		t.Fatalf("expected 2 items left, got %d: %+v", len(got), got)
	}
	for _, it := range got {
		if it.Phase == PhaseFailed {
			t.Fatalf("failed item #%d was not cleared", it.Number)
		}
	}
}

// gatedGitHub blocks each PR inside GetPullRequest until proceed is closed,
// signalling entry so a test can observe how many PRs run concurrently.
type gatedGitHub struct {
	merge.GitHub
	entered chan struct{}
	proceed chan struct{}
}

func (g *gatedGitHub) GetPullRequest(ctx context.Context, _, _ string, _ int) (*github.PullRequest, error) {
	select {
	case g.entered <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-g.proceed:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return &github.PullRequest{
		State:          github.String("open"),
		Draft:          github.Bool(false),
		Merged:         github.Bool(false),
		MergeableState: github.String("clean"),
		Head:           &github.PullRequestBranch{SHA: github.String("headsha")},
		Base:           &github.PullRequestBranch{Ref: github.String("main")},
	}, nil
}

func (g *gatedGitHub) ReviewStatus(context.Context, string, string, int) (merge.ReviewStatus, error) {
	return merge.ReviewStatus{}, nil
}

func (g *gatedGitHub) Merge(context.Context, string, string, int, string) error {
	return nil
}

func Test_Run_RespectsConcurrencyLimit(t *testing.T) {
	// Arrange
	const concurrency = 2
	g := &gatedGitHub{entered: make(chan struct{}, 16), proceed: make(chan struct{})}
	m := New(g, Config{Owner: "o", Repo: "r", MergeMethod: "squash", Interval: time.Millisecond, Timeout: time.Minute, Concurrency: concurrency}, "", func(string, ...any) {})
	for i := 1; i <= 4; i++ {
		m.items = append(m.items, &Item{Number: i, Phase: PhaseQueued, AddedAt: time.Now()})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	// Assert: exactly `concurrency` PRs enter processing and then block.
	for i := 0; i < concurrency; i++ {
		select {
		case <-g.entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d PRs started, expected %d", i, concurrency)
		}
	}
	select {
	case <-g.entered:
		t.Fatal("more than the concurrency limit started in parallel")
	case <-time.After(150 * time.Millisecond):
	}

	// Releasing the gate lets all four finish.
	close(g.proceed)
}

// mergeableGitHub reports a clean, sufficiently-approved PR and accepts the merge.
type mergeableGitHub struct{ merge.GitHub }

func (mergeableGitHub) GetPullRequest(context.Context, string, string, int) (*github.PullRequest, error) {
	return &github.PullRequest{
		State:          github.String("open"),
		Draft:          github.Bool(false),
		Merged:         github.Bool(false),
		MergeableState: github.String("clean"),
		Head:           &github.PullRequestBranch{SHA: github.String("headsha")},
		Base:           &github.PullRequestBranch{Ref: github.String("main")},
	}, nil
}

func (mergeableGitHub) ReviewStatus(context.Context, string, string, int) (merge.ReviewStatus, error) {
	return merge.ReviewStatus{Approvals: 2}, nil
}

func (mergeableGitHub) Merge(context.Context, string, string, int, string) error {
	return nil
}

func Test_process_RecordsMergeDuration(t *testing.T) {
	// Arrange
	m := New(mergeableGitHub{}, Config{Owner: "o", Repo: "r", MergeMethod: "squash", MinApprovals: 2}, "", func(string, ...any) {})
	it := &Item{Number: 1, Phase: PhaseQueued, AddedAt: time.Now().Add(-90 * time.Second)}

	// Act
	m.process(context.Background(), it)

	// Assert
	if it.Phase != PhaseMerged {
		t.Fatalf("expected phase %q, got %q", PhaseMerged, it.Phase)
	}
	if it.MergedAt == nil {
		t.Fatal("expected MergedAt to be set")
	}
	if !strings.Contains(it.Message, "merged in") {
		t.Fatalf("expected duration in message, got %q", it.Message)
	}
}
