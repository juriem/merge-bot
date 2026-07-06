package review

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"mergebot/internal/merge"
)

// fakeFetcher keys behind/runs by PR number; PullState returns the number as the
// head SHA so BehindBy/CheckRuns can look them up.
type fakeFetcher struct {
	user      string
	prs       []PR
	states    map[int]string
	behind    map[int]int
	behindErr error
	runs      map[int][]merge.CheckRun
	statuses  map[int]merge.ReviewStatus
	statErr   map[int]error
	comments  map[int][]merge.Comment
}

func (f fakeFetcher) ListComments(_ context.Context, _, _ string, number int, _ time.Time) ([]merge.Comment, error) {
	return f.comments[number], nil
}

func (f fakeFetcher) CurrentUser(context.Context) (string, error) { return f.user, nil }

func (f fakeFetcher) ListOpenPRsByAuthor(context.Context, string, string, string) ([]PR, error) {
	return f.prs, nil
}

func (f fakeFetcher) PullState(_ context.Context, _, _ string, number int) (string, string, string, error) {
	state := "clean"
	if s, ok := f.states[number]; ok {
		state = s
	}
	return state, "main", strconv.Itoa(number), nil
}

func (f fakeFetcher) BehindBy(_ context.Context, _, _, _, head string) (int, error) {
	if f.behindErr != nil {
		return 0, f.behindErr
	}
	n, _ := strconv.Atoi(head)
	return f.behind[n], nil
}

func (f fakeFetcher) CheckRuns(_ context.Context, _, _, ref string) ([]merge.CheckRun, error) {
	n, _ := strconv.Atoi(ref)
	return f.runs[n], nil
}

func (f fakeFetcher) ReviewStatus(_ context.Context, _, _ string, number int) (merge.ReviewStatus, error) {
	if err := f.statErr[number]; err != nil {
		return merge.ReviewStatus{}, err
	}
	return f.statuses[number], nil
}

func Test_Refresh_ReportsApprovalRatio(t *testing.T) {
	// Arrange
	f := fakeFetcher{
		user: "me",
		prs:  []PR{{Number: 1, Title: "one"}, {Number: 2, Title: "two"}},
		statuses: map[int]merge.ReviewStatus{
			1: {Approvals: 1},
			2: {Approvals: 2},
		},
	}
	d := NewDashboard(f, "o", "r", 2, "", func(string, ...any) {})

	// Act
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert
	got := d.List()
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0].Approvals != 1 || got[0].Required != 2 || got[0].State == "dirty" {
		t.Fatalf("PR #1: got %+v, want 1/2 no conflict", got[0])
	}
	if got[1].Approvals != 2 || got[1].Required != 2 {
		t.Fatalf("PR #2: got %+v, want 2/2", got[1])
	}
}

func Test_Refresh_FlagsConflictAndSkipsReview(t *testing.T) {
	// Arrange: PR #1 is dirty, so its review status must not even be consulted.
	f := fakeFetcher{
		user:    "me",
		prs:     []PR{{Number: 1, Title: "one"}},
		states:  map[int]string{1: "dirty"},
		statErr: map[int]error{1: errors.New("must not be called")},
	}
	d := NewDashboard(f, "o", "r", 2, "", func(string, ...any) {})

	// Act
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert
	got := d.List()
	if len(got) != 1 || got[0].State != "dirty" {
		t.Fatalf("expected PR #1 flagged as conflict (dirty), got %+v", got)
	}
}

func Test_Refresh_CategorisesBlockedPRs(t *testing.T) {
	// Arrange
	f := fakeFetcher{
		user: "me",
		prs:  []PR{{Number: 1}, {Number: 2}, {Number: 3}, {Number: 4}, {Number: 5}},
		states: map[int]string{
			1: "blocked", // failed check, approved, up to date → Failed
			2: "blocked", // failed check but behind → still Mine (can recover)
			3: "blocked", // check still running → Mine (checking)
			4: "clean",   // ready → Mine
			5: "blocked", // failed (non-required) check but under-approved → Mine (needs approvals)
		},
		behind: map[int]int{2: 3},
		runs: map[int][]merge.CheckRun{
			1: {{Name: "ci", Completed: true, Conclusion: "failure"}},
			2: {{Name: "ci", Completed: true, Conclusion: "failure"}},
			3: {{Name: "ci", Completed: false}},
			5: {{Name: "pre-commit", Completed: true, Conclusion: "failure"}},
		},
		statuses: map[int]merge.ReviewStatus{
			1: {Approvals: 2}, 2: {Approvals: 2}, 3: {Approvals: 2}, 4: {Approvals: 2},
			5: {Approvals: 0, ReviewDecision: "REVIEW_REQUIRED"},
		},
	}
	d := NewDashboard(f, "o", "r", 2, "", func(string, ...any) {})

	// Act
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert
	cat := map[int]string{}
	for _, e := range d.List() {
		cat[e.Number] = e.Category
	}
	want := map[int]string{1: CategoryFailed, 2: CategoryMine, 3: CategoryMine, 4: CategoryMine, 5: CategoryMine}
	for n, w := range want {
		if cat[n] != w {
			t.Fatalf("PR #%d category = %q, want %q", n, cat[n], w)
		}
	}
}

func Test_Refresh_MarksQueuedPRsByBotComments(t *testing.T) {
	// Arrange: the bot confirmed #1 as queued; #2 has no queue signals.
	f := fakeFetcher{
		user:     "me",
		prs:      []PR{{Number: 1}, {Number: 2}},
		statuses: map[int]merge.ReviewStatus{1: {Approvals: 2}, 2: {Approvals: 2}},
		comments: map[int][]merge.Comment{
			1: {{Author: "bot", Body: "This PR: waiting at queue position `1` of `1`", CreatedAt: time.Now()}},
		},
	}
	d := NewDashboard(f, "o", "r", 2, "", func(string, ...any) {}).WithTeamQueue()

	// Act
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert
	queued := map[int]bool{}
	for _, e := range d.List() {
		queued[e.Number] = e.Queued
	}
	if !queued[1] || queued[2] {
		t.Fatalf("queued flags = %v, want #1 true, #2 false", queued)
	}
}

func Test_Refresh_BlockedStaysInMineWhenBehindUnknown(t *testing.T) {
	// Arrange: blocked with a failed check, but the behind-check errors — we can't
	// tell if it's recoverable, so it must NOT be declared dead-failed.
	f := fakeFetcher{
		user:      "me",
		prs:       []PR{{Number: 1}},
		states:    map[int]string{1: "blocked"},
		behindErr: errors.New("compare boom"),
		runs:      map[int][]merge.CheckRun{1: {{Name: "ci", Completed: true, Conclusion: "failure"}}},
		statuses:  map[int]merge.ReviewStatus{1: {Approvals: 2}},
	}
	d := NewDashboard(f, "o", "r", 2, "", func(string, ...any) {})

	// Act
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert
	got := d.List()
	if len(got) != 1 || got[0].Category != CategoryMine {
		t.Fatalf("expected PR #1 to stay in mine, got %+v", got)
	}
}

type recordingFetcher struct {
	fakeFetcher
	seenAuthor  string
	userQueried bool
}

func (f *recordingFetcher) CurrentUser(context.Context) (string, error) {
	f.userQueried = true
	return "token-owner", nil
}

func (f *recordingFetcher) ListOpenPRsByAuthor(_ context.Context, _, _, author string) ([]PR, error) {
	f.seenAuthor = author
	return f.prs, nil
}

func Test_Refresh_UsesConfiguredAuthorWithoutQueryingCurrentUser(t *testing.T) {
	// Arrange
	f := &recordingFetcher{fakeFetcher: fakeFetcher{prs: []PR{{Number: 1}}}}
	d := NewDashboard(f, "o", "r", 2, "explicit-login", func(string, ...any) {})

	// Act
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert
	if f.seenAuthor != "explicit-login" {
		t.Fatalf("searched author %q, want %q", f.seenAuthor, "explicit-login")
	}
	if f.userQueried {
		t.Fatal("CurrentUser must not be called when an author is configured")
	}
}

func Test_Refresh_SkipsPRsWithUnreadableState(t *testing.T) {
	// Arrange
	f := fakeFetcher{
		user:     "me",
		prs:      []PR{{Number: 1}, {Number: 2}},
		states:   map[int]string{1: "err"},
		statuses: map[int]merge.ReviewStatus{2: {Approvals: 2}},
		statErr:  map[int]error{1: errors.New("boom")},
	}
	d := NewDashboard(f, "o", "r", 2, "", func(string, ...any) {})

	// Act
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert: PR #1's review lookup errors, so only #2 survives.
	got := d.List()
	if len(got) != 1 || got[0].Number != 2 {
		t.Fatalf("expected only PR #2, got %+v", got)
	}
}
