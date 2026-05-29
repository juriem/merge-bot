// Package queue implements a sequential merge queue: a single worker processes
// one pull request at a time, driving it to a merged state before moving on to
// the next. The queue is persisted to disk so it survives daemon restarts.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/wallester/mergebot/internal/merge"
)

// Phase is the lifecycle state of a queued pull request.
type Phase string

const (
	PhaseQueued  Phase = "queued"
	PhaseActive  Phase = "active"
	PhaseMerged  Phase = "merged"
	PhaseFailed  Phase = "failed"
	PhaseStopped Phase = "stopped"
)

// Item is one pull request tracked by the queue.
type Item struct {
	Number    int       `json:"number"`
	Phase     Phase     `json:"phase"`
	Message   string    `json:"message"`
	Error     string    `json:"error,omitempty"`
	AddedAt   time.Time `json:"added_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Config holds the merge settings shared by every processed pull request.
type Config struct {
	Owner           string
	Repo            string
	Interval        time.Duration
	Timeout         time.Duration
	MergeMethod     string
	AllowUnstable   bool
	AllowUnresolved bool
}

// Manager owns the ordered queue and the single worker that drains it.
type Manager struct {
	cfg       Config
	client    merge.GitHub
	statePath string
	logf      func(format string, args ...any)

	mu           sync.Mutex
	items        []*Item
	wake         chan struct{}
	activeCancel context.CancelFunc
}

// New builds a Manager. statePath may be empty to disable persistence.
func New(client merge.GitHub, cfg Config, statePath string, logf func(format string, args ...any)) *Manager {
	return &Manager{
		cfg:       cfg,
		client:    client,
		statePath: statePath,
		logf:      logf,
		wake:      make(chan struct{}, 1),
	}
}

// Load restores a previously persisted queue. Items that were mid-flight are
// reset to queued so they get reprocessed.
func (m *Manager) Load() error {
	if m.statePath == "" {
		return nil
	}

	data, err := os.ReadFile(m.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}

	var items []*Item
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("parse state: %w", err)
	}

	for _, it := range items {
		if it.Phase == PhaseActive {
			it.Phase = PhaseQueued
			it.Message = "re-queued after restart"
		}
	}

	m.mu.Lock()
	m.items = items
	m.mu.Unlock()

	return nil
}

// List returns a snapshot copy of the queue in order.
func (m *Manager) List() []Item {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Item, len(m.items))
	for i, it := range m.items {
		out[i] = *it
	}

	return out
}

// Add enqueues a pull request. A PR already queued or active is left untouched;
// one that previously finished is reset back to queued.
func (m *Manager) Add(number int) {
	m.mu.Lock()
	now := time.Now()

	for _, it := range m.items {
		if it.Number != number {
			continue
		}

		if it.Phase == PhaseQueued || it.Phase == PhaseActive {
			m.mu.Unlock()
			return
		}

		it.Phase = PhaseQueued
		it.Message = "re-queued"
		it.Error = ""
		it.UpdatedAt = now
		m.save()
		m.mu.Unlock()
		m.notify()

		return
	}

	m.items = append(m.items, &Item{
		Number:    number,
		Phase:     PhaseQueued,
		Message:   "queued",
		AddedAt:   now,
		UpdatedAt: now,
	})
	m.save()
	m.mu.Unlock()
	m.notify()
}

// Remove stops a queued or active pull request.
func (m *Manager) Remove(number int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, it := range m.items {
		if it.Number != number {
			continue
		}

		if it.Phase == PhaseQueued || it.Phase == PhaseActive {
			if it.Phase == PhaseActive && m.activeCancel != nil {
				m.activeCancel()
			}
			it.Phase = PhaseStopped
			it.Message = "stopped"
			it.UpdatedAt = time.Now()
			m.save()
		}

		return
	}
}

// Run drains the queue until ctx is cancelled, processing one PR at a time.
func (m *Manager) Run(ctx context.Context) {
	for {
		it := m.nextQueued()
		if it == nil {
			select {
			case <-ctx.Done():
				return
			case <-m.wake:
				continue
			}
		}

		m.process(ctx, it)

		if ctx.Err() != nil {
			return
		}
	}
}

func (m *Manager) nextQueued() *Item {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, it := range m.items {
		if it.Phase == PhaseQueued {
			return it
		}
	}

	return nil
}

func (m *Manager) process(ctx context.Context, it *Item) {
	procCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	m.mu.Lock()
	it.Phase = PhaseActive
	it.Message = "processing"
	it.UpdatedAt = time.Now()
	m.activeCancel = cancel
	m.save()
	m.mu.Unlock()

	runner := merge.Runner{
		Client:          m.client,
		Owner:           m.cfg.Owner,
		Repo:            m.cfg.Repo,
		Number:          it.Number,
		Interval:        m.cfg.Interval,
		Timeout:         m.cfg.Timeout,
		MergeMethod:     m.cfg.MergeMethod,
		AllowUnstable:   m.cfg.AllowUnstable,
		AllowUnresolved: m.cfg.AllowUnresolved,
		Logf:            m.itemLogger(it),
	}

	err := runner.Run(procCtx)

	m.mu.Lock()
	m.activeCancel = nil

	switch {
	case it.Phase == PhaseStopped:
		// Removed by the user while active; keep the stopped state.
	case err == nil:
		it.Phase = PhaseMerged
		it.Message = "merged"
		it.Error = ""
	case errors.Is(err, context.Canceled):
		it.Phase = PhaseStopped
		it.Message = "stopped"
	default:
		it.Phase = PhaseFailed
		it.Message = "failed"
		it.Error = err.Error()
	}

	it.UpdatedAt = time.Now()
	m.save()
	m.mu.Unlock()
}

func (m *Manager) itemLogger(it *Item) func(format string, args ...any) {
	return func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		m.logf("%s", msg)

		m.mu.Lock()
		if it.Phase == PhaseActive {
			it.Message = msg
			it.UpdatedAt = time.Now()
			m.save()
		}
		m.mu.Unlock()
	}
}

func (m *Manager) notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// save persists the queue. The caller must hold m.mu.
func (m *Manager) save() {
	if m.statePath == "" {
		return
	}

	data, err := json.MarshalIndent(m.items, "", "  ")
	if err != nil {
		m.logf("state save: marshal: %v", err)
		return
	}

	tmp := m.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		m.logf("state save: write: %v", err)
		return
	}

	if err := os.Rename(tmp, m.statePath); err != nil {
		m.logf("state save: rename: %v", err)
	}
}
