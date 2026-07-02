package review

import (
	"context"
	"errors"
	"testing"

	"mergebot/internal/merge"
)

type fakeFetcher struct {
	user     string
	prs      []PR
	states   map[int]string
	statuses map[int]merge.ReviewStatus
	statErr  map[int]error
}

func (f fakeFetcher) CurrentUser(context.Context) (string, error) { return f.user, nil }

func (f fakeFetcher) ListOpenPRsByAuthor(context.Context, string, string, string) ([]PR, error) {
	return f.prs, nil
}

func (f fakeFetcher) MergeableState(_ context.Context, _, _ string, number int) (string, error) {
	if s, ok := f.states[number]; ok {
		return s, nil
	}
	return "clean", nil
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
