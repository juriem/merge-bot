// Package review builds a dashboard of the token owner's open pull requests that
// are ready for review, annotated with how many more approvals each still needs.
package review

import (
	"context"
	"sync"
	"time"

	"mergebot/internal/merge"
)

// PR is a pull request discovered by a search.
type PR struct {
	Number int
	Title  string
}

// Entry is one dashboard row: one of my open PRs with its approval ratio and its
// GitHub mergeable_state (clean / unstable / blocked / behind / dirty / unknown),
// which the UI uses to tell a merge-ready PR from a conflicting or check-blocked
// one. For a dirty (conflicting) PR the approval fields are not filled in.
type Entry struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Approvals int    `json:"approvals"`
	Required  int    `json:"required"`
	State     string `json:"state"`
}

// Fetcher is the GitHub subset the dashboard needs.
type Fetcher interface {
	CurrentUser(ctx context.Context) (string, error)
	ListOpenPRsByAuthor(ctx context.Context, owner, repo, author string) ([]PR, error)
	MergeableState(ctx context.Context, owner, repo string, number int) (string, error)
	ReviewStatus(ctx context.Context, owner, repo string, number int) (merge.ReviewStatus, error)
}

// Dashboard periodically lists the token owner's ready-for-review PRs and caches
// the result for the HTTP layer to serve.
type Dashboard struct {
	fetcher      Fetcher
	owner, repo  string
	minApprovals int
	logf         func(format string, args ...any)

	poke chan struct{}

	mu      sync.Mutex
	entries []Entry
	author  string
	loaded  bool
}

// dashboardConcurrency bounds how many PRs are fetched in parallel per refresh,
// so the first pass is fast without hammering the API.
const dashboardConcurrency = 8

// NewDashboard builds a Dashboard. minApprovals is the approval target used to
// compute how many more approvals each PR still needs. author is the GitHub login
// whose PRs to list; when empty, the token owner is used.
func NewDashboard(f Fetcher, owner, repo string, minApprovals int, author string, logf func(format string, args ...any)) *Dashboard {
	return &Dashboard{
		fetcher:      f,
		owner:        owner,
		repo:         repo,
		minApprovals: minApprovals,
		author:       author,
		logf:         logf,
		poke:         make(chan struct{}, 1),
	}
}

// TriggerRefresh asks the refresh loop to rebuild the dashboard now, without
// waiting for the next tick. It never blocks; a refresh already pending is kept.
func (d *Dashboard) TriggerRefresh() {
	select {
	case d.poke <- struct{}{}:
	default:
	}
}

// List returns a snapshot of the cached dashboard entries.
func (d *Dashboard) List() []Entry {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]Entry, len(d.entries))
	copy(out, d.entries)

	return out
}

// Loaded reports whether at least one refresh has completed, so the UI can tell
// "still loading" apart from "no open PRs".
func (d *Dashboard) Loaded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.loaded
}

// Refresh rebuilds the dashboard from GitHub. A PR whose review status cannot be
// read is skipped rather than failing the whole refresh.
func (d *Dashboard) Refresh(ctx context.Context) error {
	author, err := d.ensureAuthor(ctx)
	if err != nil {
		return err
	}

	prs, err := d.fetcher.ListOpenPRsByAuthor(ctx, d.owner, d.repo, author)
	if err != nil {
		return err
	}

	d.logf("dashboard: %d open PR(s) for %q in %s/%s", len(prs), author, d.owner, d.repo)

	// Fetch PRs in parallel (bounded) into a fixed-size slice, preserving the
	// search order; each index is written by exactly one goroutine.
	built := make([]*Entry, len(prs))
	sem := make(chan struct{}, dashboardConcurrency)
	var wg sync.WaitGroup

	for i, pr := range prs {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(i int, pr PR) {
			defer wg.Done()
			defer func() { <-sem }()

			if entry, ok := d.entryFor(ctx, pr); ok {
				built[i] = &entry
			}
		}(i, pr)
	}
	wg.Wait()

	entries := make([]Entry, 0, len(prs))
	for _, e := range built {
		if e != nil {
			entries = append(entries, *e)
		}
	}

	d.mu.Lock()
	d.entries = entries
	d.loaded = true
	d.mu.Unlock()

	return nil
}

// entryFor builds one dashboard entry, returning ok=false when the PR should be
// skipped (its state could not be read).
func (d *Dashboard) entryFor(ctx context.Context, pr PR) (Entry, bool) {
	state, err := d.fetcher.MergeableState(ctx, d.owner, d.repo, pr.Number)
	if err != nil {
		d.logf("dashboard mergeable PR #%d: %v", pr.Number, err)
		return Entry{}, false
	}

	// A conflicting PR needs a rebase, not approvals; skip the review lookup.
	if isConflict(state) {
		return Entry{Number: pr.Number, Title: pr.Title, Required: d.minApprovals, State: state}, true
	}

	status, err := d.fetcher.ReviewStatus(ctx, d.owner, d.repo, pr.Number)
	if err != nil {
		d.logf("dashboard review PR #%d: %v", pr.Number, err)
		return Entry{}, false
	}

	return Entry{
		Number:    pr.Number,
		Title:     pr.Title,
		Approvals: status.Approvals,
		Required:  d.minApprovals,
		State:     state,
	}, true
}

// RefreshLoop refreshes once immediately, then on every interval tick until ctx
// is cancelled. A non-positive interval falls back to five minutes.
func (d *Dashboard) RefreshLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}

	if err := d.Refresh(ctx); err != nil && ctx.Err() == nil {
		d.logf("dashboard refresh: %v", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.Refresh(ctx); err != nil && ctx.Err() == nil {
				d.logf("dashboard refresh: %v", err)
			}
		case <-d.poke:
			if err := d.Refresh(ctx); err != nil && ctx.Err() == nil {
				d.logf("dashboard refresh: %v", err)
			}
		}
	}
}

// ensureAuthor caches the token owner's login (it does not change between runs).
func (d *Dashboard) ensureAuthor(ctx context.Context) (string, error) {
	d.mu.Lock()
	author := d.author
	d.mu.Unlock()
	if author != "" {
		return author, nil
	}

	author, err := d.fetcher.CurrentUser(ctx)
	if err != nil {
		return "", err
	}

	d.mu.Lock()
	d.author = author
	d.mu.Unlock()

	return author, nil
}

// isConflict reports whether a mergeable_state means the PR has merge conflicts.
func isConflict(state string) bool {
	return state == "dirty"
}
