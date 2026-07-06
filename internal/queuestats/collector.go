// Package queuestats silently samples the external (label-driven) merge queue
// via the GitHub search API — no comments are posted — and keeps a persisted
// history so the UI can chart queue depth and throughput.
package queuestats

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// Searcher is the GitHub subset the collector needs.
type Searcher interface {
	CountOpenPRsWithLabel(ctx context.Context, owner, repo, label string) (int, error)
	CountMergedWithLabelSince(ctx context.Context, owner, repo, label string, since time.Time) (int, error)
}

// Snapshot is one sample of the external queue.
type Snapshot struct {
	At          time.Time `json:"at"`
	Waiting     int       `json:"waiting"`      // open PRs carrying the queue label
	MergedToday int       `json:"merged_today"` // labelled PRs merged since local midnight
}

// maxHistory bounds the persisted series (288 samples = 24h at 5m).
const maxHistory = 288

// Collector polls the queue label counts and keeps a bounded history.
type Collector struct {
	searcher Searcher
	owner    string
	repo     string
	label    string
	path     string // persistence file; empty disables
	logf     func(format string, args ...any)

	mu      sync.Mutex
	history []Snapshot
}

// New builds a Collector. path may be empty to disable persistence.
func New(s Searcher, owner, repo, label, path string, logf func(format string, args ...any)) *Collector {
	return &Collector{searcher: s, owner: owner, repo: repo, label: label, path: path, logf: logf}
}

// Load restores previously persisted history.
func (c *Collector) Load() error {
	if c.path == "" {
		return nil
	}

	data, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	var history []Snapshot
	if err := json.Unmarshal(data, &history); err != nil {
		return err
	}

	c.mu.Lock()
	c.history = history
	c.mu.Unlock()

	return nil
}

// History returns a copy of the sampled series, oldest first.
func (c *Collector) History() []Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]Snapshot, len(c.history))
	copy(out, c.history)

	return out
}

// Poll takes one sample and appends it to the history.
func (c *Collector) Poll(ctx context.Context) error {
	waiting, err := c.searcher.CountOpenPRsWithLabel(ctx, c.owner, c.repo, c.label)
	if err != nil {
		return err
	}

	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	merged, err := c.searcher.CountMergedWithLabelSince(ctx, c.owner, c.repo, c.label, midnight)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.history = append(c.history, Snapshot{At: now, Waiting: waiting, MergedToday: merged})
	if len(c.history) > maxHistory {
		c.history = c.history[len(c.history)-maxHistory:]
	}
	c.save()
	c.mu.Unlock()

	return nil
}

// Run polls once immediately, then on every interval tick until ctx is
// cancelled. A non-positive interval falls back to five minutes.
func (c *Collector) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}

	if err := c.Poll(ctx); err != nil && ctx.Err() == nil {
		c.logf("queue stats: %v", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.Poll(ctx); err != nil && ctx.Err() == nil {
				c.logf("queue stats: %v", err)
			}
		}
	}
}

// save persists the history. The caller must hold c.mu.
func (c *Collector) save() {
	if c.path == "" {
		return
	}

	data, err := json.Marshal(c.history)
	if err != nil {
		c.logf("queue stats save: marshal: %v", err)
		return
	}

	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		c.logf("queue stats save: write: %v", err)
		return
	}

	if err := os.Rename(tmp, c.path); err != nil {
		c.logf("queue stats save: rename: %v", err)
	}
}
