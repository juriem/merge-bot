// Package queuestats keeps a persisted history of team merge-queue depth
// samples. The team queue exposes no machine-readable API, so samples are
// extracted from the queue bot's comment replies (e.g. /status probes) and
// recorded here — no polling of its own.
package queuestats

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// Snapshot is one observed sample of the external queue.
type Snapshot struct {
	At      time.Time `json:"at"`
	Waiting int       `json:"waiting"` // PRs waiting, as reported by the queue bot
}

// maxHistory bounds the persisted series.
const maxHistory = 288

// Collector accumulates observed queue-depth samples.
type Collector struct {
	path string // persistence file; empty disables
	logf func(format string, args ...any)

	mu      sync.Mutex
	history []Snapshot
}

// New builds a Collector. path may be empty to disable persistence.
func New(path string, logf func(format string, args ...any)) *Collector {
	return &Collector{path: path, logf: logf}
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

// Record appends one observed queue-depth sample.
func (c *Collector) Record(waiting int) {
	c.mu.Lock()
	c.history = append(c.history, Snapshot{At: time.Now(), Waiting: waiting})
	if len(c.history) > maxHistory {
		c.history = c.history[len(c.history)-maxHistory:]
	}
	c.save()
	c.mu.Unlock()
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
