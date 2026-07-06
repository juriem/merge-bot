package queuestats

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

type fakeSearcher struct {
	waiting int
	merged  int
}

func (f fakeSearcher) CountOpenPRsWithLabel(context.Context, string, string, string) (int, error) {
	return f.waiting, nil
}

func (f fakeSearcher) CountMergedWithLabelSince(context.Context, string, string, string, time.Time) (int, error) {
	return f.merged, nil
}

func Test_Poll_AppendsSnapshotAndPersists(t *testing.T) {
	// Arrange
	path := filepath.Join(t.TempDir(), "stats.json")
	c := New(fakeSearcher{waiting: 4, merged: 7}, "o", "r", "merge-queue", path, func(string, ...any) {})

	// Act
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert: snapshot recorded and survives a reload.
	got := c.History()
	if len(got) != 1 || got[0].Waiting != 4 || got[0].MergedToday != 7 {
		t.Fatalf("history = %+v, want one snapshot 4/7", got)
	}

	reloaded := New(fakeSearcher{}, "o", "r", "merge-queue", path, func(string, ...any) {})
	if err := reloaded.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(reloaded.History()) != 1 {
		t.Fatalf("reloaded history = %+v, want 1 snapshot", reloaded.History())
	}
}

func Test_Poll_TrimsHistory(t *testing.T) {
	// Arrange
	c := New(fakeSearcher{}, "o", "r", "merge-queue", "", func(string, ...any) {})
	for i := 0; i < maxHistory+10; i++ {
		if err := c.Poll(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	// Assert
	if n := len(c.History()); n != maxHistory {
		t.Fatalf("history length = %d, want %d", n, maxHistory)
	}
}
