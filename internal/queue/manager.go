// Package queue implements a merge queue: a pool of workers drives queued pull
// requests to a merged state, up to Config.Concurrency at a time (one by
// default). The queue is persisted to disk so it survives daemon restarts.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"mergebot/internal/merge"
)

// Phase is the lifecycle state of a queued pull request.
type Phase string

const (
	PhaseQueued         Phase = "queued"
	PhaseActive         Phase = "active"
	PhaseMerged         Phase = "merged"
	PhaseFailed         Phase = "failed"
	PhaseConflicts      Phase = "conflicts"
	PhaseNeedsApprovals Phase = "needs_approvals"
	PhaseStopped        Phase = "stopped"
)

// Item is one pull request tracked by the queue.
type Item struct {
	Number    int        `json:"number"`
	Phase     Phase      `json:"phase"`
	Message   string     `json:"message"`
	Error     string     `json:"error,omitempty"`
	AddedAt   time.Time  `json:"added_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	MergedAt  *time.Time `json:"merged_at,omitempty"` // set once merged; AddedAt→MergedAt is the time to merge
}

// Config holds the merge settings shared by every processed pull request.
type Config struct {
	Owner           string
	Repo            string
	Interval        time.Duration
	Timeout         time.Duration
	RecheckInterval time.Duration // how often to re-check parked (needs-approvals) PRs; 0 disables
	MergeMethod     string
	MinApprovals    int
	Concurrency     int // how many PRs to drive in parallel; <1 means 1
	AllowUnstable   bool
	AllowUnresolved bool
}

// Manager owns the ordered queue and the worker pool that drains it.
type Manager struct {
	cfg       Config
	client    merge.GitHub
	statePath string
	logf      func(format string, args ...any)

	mu      sync.Mutex
	items   []*Item
	wake    chan struct{}
	cancels map[int]context.CancelFunc // number -> cancel for the in-flight PRs
}

// New builds a Manager. statePath may be empty to disable persistence.
func New(client merge.GitHub, cfg Config, statePath string, logf func(format string, args ...any)) *Manager {
	return &Manager{
		cfg:       cfg,
		client:    client,
		statePath: statePath,
		logf:      logf,
		cancels:   make(map[int]context.CancelFunc),
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

		// A manual re-add starts a fresh cycle: reset the timing so the time to
		// merge is measured from now, not from the original enqueue.
		it.Phase = PhaseQueued
		it.Message = "re-queued"
		it.Error = ""
		it.AddedAt = now
		it.UpdatedAt = now
		it.MergedAt = nil
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
			if cancel := m.cancels[number]; cancel != nil {
				cancel()
				delete(m.cancels, number)
			}
			it.Phase = PhaseStopped
			it.Message = "stopped"
			it.UpdatedAt = time.Now()
			m.save()
		}

		return
	}
}

// Clear drops every item currently in one of the given phases. Queued and active
// items are never removed, so an in-flight PR cannot be cleared out from under
// the worker.
func (m *Manager) Clear(phases []Phase) {
	want := make(map[Phase]bool, len(phases))
	for _, p := range phases {
		if p != PhaseQueued && p != PhaseActive {
			want[p] = true
		}
	}
	if len(want) == 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	kept := m.items[:0]
	for _, it := range m.items {
		if want[it.Phase] {
			continue
		}
		kept = append(kept, it)
	}
	for i := len(kept); i < len(m.items); i++ {
		m.items[i] = nil // release dropped items for GC
	}
	m.items = kept
	m.save()
}

// Requeue moves a parked PR (merge conflicts or missing approvals) back to the
// main queue right away, without waiting for the background recheck. The worker
// re-evaluates it and may park it again if it is not actually ready. Timing is
// preserved (unlike a fresh Add), since this continues the same attempt.
// Returns false if the PR is not currently parked.
func (m *Manager) Requeue(number int) bool {
	m.mu.Lock()
	requeued := false
	for _, it := range m.items {
		if it.Number != number {
			continue
		}
		if it.Phase == PhaseConflicts || it.Phase == PhaseNeedsApprovals {
			it.Phase = PhaseQueued
			it.Message = "re-checking"
			it.Error = ""
			it.UpdatedAt = time.Now()
			m.save()
			requeued = true
		}
		break
	}
	m.mu.Unlock()

	if requeued {
		m.notify()
	}

	return requeued
}

// Run drains the queue until ctx is cancelled, driving up to Concurrency PRs in
// parallel. A single dispatcher claims queued PRs and hands each to a worker
// goroutine, bounded by a slot semaphore. It also starts a slower background
// loop that re-checks parked PRs.
func (m *Manager) Run(ctx context.Context) {
	go m.recheckLoop(ctx)

	workers := m.cfg.Concurrency
	if workers < 1 {
		workers = 1
	}
	slots := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case slots <- struct{}{}:
		}

		it, procCtx := m.claim(ctx)
		if it == nil {
			<-slots // release the unused slot and wait for new work
			select {
			case <-ctx.Done():
				wg.Wait()
				return
			case <-m.wake:
				continue
			}
		}

		wg.Add(1)
		go func(it *Item, procCtx context.Context) {
			defer wg.Done()
			defer func() { <-slots }()
			m.process(procCtx, it)
		}(it, procCtx)
	}
}

// claim atomically selects the first queued PR, marks it active and registers a
// cancel function so Remove (or shutdown) can stop it. Doing this under one lock
// closes the race where two workers grab the same PR or a Remove slips in
// between selection and registration.
func (m *Manager) claim(parent context.Context) (*Item, context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, it := range m.items {
		if it.Phase != PhaseQueued {
			continue
		}

		procCtx, cancel := context.WithCancel(parent)
		it.Phase = PhaseActive
		it.Message = "processing"
		it.UpdatedAt = time.Now()
		m.cancels[it.Number] = cancel
		m.save()

		return it, procCtx
	}

	return nil, nil
}

// recheckLoop periodically revisits parked PRs (waiting for approvals or blocked
// by conflicts) and moves any that are now ready back into the main queue. It is
// intentionally slower than the per-PR poll to limit API calls. A non-positive
// interval disables it.
func (m *Manager) recheckLoop(ctx context.Context) {
	if m.cfg.RecheckInterval <= 0 {
		return
	}

	ticker := time.NewTicker(m.cfg.RecheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.recheckParked(ctx)
		}
	}
}

// recheckParked re-queues parked PRs that are ready to make progress again: a
// needs-approvals PR that now meets the approval gate, or a conflicts PR whose
// conflict is gone. Both go back to the main queue, where the worker re-runs the
// full gate (so a de-conflicted-but-under-approved PR lands in needs-approvals).
// The API lookups run without the queue lock held.
func (m *Manager) recheckParked(ctx context.Context) {
	m.mu.Lock()
	var needApprovals, conflicts []int
	for _, it := range m.items {
		switch it.Phase {
		case PhaseNeedsApprovals:
			needApprovals = append(needApprovals, it.Number)
		case PhaseConflicts:
			conflicts = append(conflicts, it.Number)
		}
	}
	m.mu.Unlock()

	for _, number := range needApprovals {
		if ctx.Err() != nil {
			return
		}

		status, err := m.client.ReviewStatus(ctx, m.cfg.Owner, m.cfg.Repo, number)
		if err != nil {
			m.logf("recheck approvals PR #%d: %v", number, err)
			continue
		}

		if merge.ApprovalsMet(status, m.cfg.MinApprovals) {
			m.requeueParked(number, PhaseNeedsApprovals, "approved; back in queue")
		}
	}

	for _, number := range conflicts {
		if ctx.Err() != nil {
			return
		}

		pr, err := m.client.GetPullRequest(ctx, m.cfg.Owner, m.cfg.Repo, number)
		if err != nil {
			m.logf("recheck conflict PR #%d: %v", number, err)
			continue
		}

		if conflictResolved(pr.GetMergeableState()) {
			m.requeueParked(number, PhaseConflicts, "conflict resolved; back in queue")
		}
	}
}

// conflictResolved reports whether a mergeable_state indicates the merge
// conflict is gone. Unknown/blank means GitHub is still computing, so we wait.
func conflictResolved(state string) bool {
	switch state {
	case "dirty", "", "unknown":
		return false
	default:
		return true
	}
}

// requeueParked moves a still-parked PR back to the queued phase and wakes the
// worker. A PR that has meanwhile changed phase (removed, re-added) is left as is.
func (m *Manager) requeueParked(number int, from Phase, msg string) {
	m.mu.Lock()
	for _, it := range m.items {
		if it.Number == number && it.Phase == from {
			it.Phase = PhaseQueued
			it.Message = msg
			it.Error = ""
			it.UpdatedAt = time.Now()
			m.save()
			break
		}
	}
	m.mu.Unlock()
	m.notify()
}

func (m *Manager) process(ctx context.Context, it *Item) {
	runner := merge.Runner{
		Client:          m.client,
		Owner:           m.cfg.Owner,
		Repo:            m.cfg.Repo,
		Number:          it.Number,
		Interval:        m.cfg.Interval,
		Timeout:         m.cfg.Timeout,
		MergeMethod:     m.cfg.MergeMethod,
		MinApprovals:    m.cfg.MinApprovals,
		AllowUnstable:   m.cfg.AllowUnstable,
		AllowUnresolved: m.cfg.AllowUnresolved,
		Logf:            m.itemLogger(it),
	}

	err := runner.Run(ctx)

	m.mu.Lock()
	if cancel := m.cancels[it.Number]; cancel != nil {
		cancel()
		delete(m.cancels, it.Number)
	}
	now := time.Now()

	switch {
	case it.Phase == PhaseStopped:
		// Removed by the user while active; keep the stopped state.
	case err == nil:
		merged := now
		it.Phase = PhaseMerged
		it.MergedAt = &merged
		it.Message = "merged in " + now.Sub(it.AddedAt).Round(time.Second).String()
		it.Error = ""
	case errors.Is(err, context.Canceled):
		it.Phase = PhaseStopped
		it.Message = "stopped"
	case errors.Is(err, merge.ErrConflicts):
		it.Phase = PhaseConflicts
		it.Message = "merge conflicts"
		it.Error = err.Error()
	case errors.Is(err, merge.ErrInsufficientApprovals):
		it.Phase = PhaseNeedsApprovals
		it.Message = "needs approvals"
		it.Error = err.Error()
	case errors.Is(err, merge.ErrRequiredCheckFailed):
		it.Phase = PhaseFailed
		it.Message = "required check failed"
		it.Error = err.Error()
	case errors.Is(err, merge.ErrBlocked):
		it.Phase = PhaseFailed
		it.Message = "blocked by branch protection"
		it.Error = err.Error()
	default:
		it.Phase = PhaseFailed
		it.Message = "failed"
		it.Error = err.Error()
	}

	it.UpdatedAt = now
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
