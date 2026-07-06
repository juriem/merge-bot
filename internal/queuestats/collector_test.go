package queuestats

import (
	"path/filepath"
	"testing"
)

func Test_Record_AppendsSnapshotAndPersists(t *testing.T) {
	// Arrange
	path := filepath.Join(t.TempDir(), "stats.json")
	c := New(path, func(string, ...any) {})

	// Act
	c.Record(4)

	// Assert: snapshot recorded and survives a reload.
	got := c.History()
	if len(got) != 1 || got[0].Waiting != 4 {
		t.Fatalf("history = %+v, want one snapshot waiting=4", got)
	}

	reloaded := New(path, func(string, ...any) {})
	if err := reloaded.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(reloaded.History()) != 1 {
		t.Fatalf("reloaded history = %+v, want 1 snapshot", reloaded.History())
	}
}

func Test_Record_TrimsHistory(t *testing.T) {
	// Arrange
	c := New("", func(string, ...any) {})
	for i := 0; i < maxHistory+10; i++ {
		c.Record(i)
	}

	// Assert
	if n := len(c.History()); n != maxHistory {
		t.Fatalf("history length = %d, want %d", n, maxHistory)
	}
}
