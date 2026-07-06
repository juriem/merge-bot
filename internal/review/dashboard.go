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

// Category buckets a dashboard PR into a UI tab.
const (
	CategoryMine     = "mine"     // ready or in-progress — shown in My PRs
	CategoryConflict = "conflict" // merge conflict — shown in Merge conflicts
	CategoryFailed   = "failed"   // failed check that can't recover — shown in Failed
)

// Entry is one dashboard row: one of my open PRs, triaged into a Category, with
// its approval ratio, GitHub mergeable_state and a display hint for the ones that
// are not merge-ready. Approval fields are not filled in for a conflicting PR.
type Entry struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Approvals int    `json:"approvals"`
	Required  int    `json:"required"`
	State     string `json:"state"`
	Category  string `json:"category"`
	Hint      string `json:"hint"`
	Queued    bool   `json:"queued"` // already in the team merge queue (merge-queue mode)
}

// Fetcher is the GitHub subset the dashboard needs.
type Fetcher interface {
	CurrentUser(ctx context.Context) (string, error)
	ListOpenPRsByAuthor(ctx context.Context, owner, repo, author string) ([]PR, error)
	PullState(ctx context.Context, owner, repo string, number int) (state, base, head string, err error)
	BehindBy(ctx context.Context, owner, repo, base, head string) (int, error)
	CheckRuns(ctx context.Context, owner, repo, ref string) ([]merge.CheckRun, error)
	ReviewStatus(ctx context.Context, owner, repo string, number int) (merge.ReviewStatus, error)
	ListComments(ctx context.Context, owner, repo string, number int, since time.Time) ([]merge.Comment, error)
}

// Dashboard periodically lists the token owner's ready-for-review PRs and caches
// the result for the HTTP layer to serve.
type Dashboard struct {
	fetcher      Fetcher
	owner, repo  string
	minApprovals int
	logf         func(format string, args ...any)

	poke chan struct{}

	// teamQueue enables the "already in the external queue" flag, detected from
	// the queue bot's comments. Set before RefreshLoop starts.
	teamQueue bool

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

// WithTeamQueue enables the "already in the external queue" flag on entries,
// detected from the queue bot's comments (the bot keeps no label state).
func (d *Dashboard) WithTeamQueue() *Dashboard {
	d.teamQueue = true
	return d
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

// entryFor builds one dashboard entry, triaging the PR into a Category. It
// returns ok=false when the PR should be skipped (state could not be read).
func (d *Dashboard) entryFor(ctx context.Context, pr PR) (Entry, bool) {
	state, base, head, err := d.fetcher.PullState(ctx, d.owner, d.repo, pr.Number)
	if err != nil {
		d.logf("dashboard state PR #%d: %v", pr.Number, err)
		return Entry{}, false
	}

	e := Entry{Number: pr.Number, Title: pr.Title, Required: d.minApprovals, State: state}
	if d.teamQueue {
		comments, err := d.fetcher.ListComments(ctx, d.owner, d.repo, pr.Number, time.Now().Add(-7*24*time.Hour))
		if err != nil {
			d.logf("dashboard comments PR #%d: %v", pr.Number, err)
		} else {
			e.Queued = merge.QueuedFromComments(comments)
		}
	}

	// A conflicting PR needs a rebase, not approvals — straight to the conflicts
	// lane, no further lookups.
	if state == "dirty" {
		e.Category = CategoryConflict
		return e, true
	}

	status, err := d.fetcher.ReviewStatus(ctx, d.owner, d.repo, pr.Number)
	if err != nil {
		d.logf("dashboard review PR #%d: %v", pr.Number, err)
		return Entry{}, false
	}
	e.Approvals = status.Approvals

	// clean / unstable are mergeable (required checks green + approved) → ready.
	if state == "clean" || state == "unstable" {
		e.Category = CategoryMine
		return e, true
	}

	// behind/unknown: not blocked by a failure — surface as an in-progress row.
	if state != "blocked" {
		e.Category = CategoryMine
		if state == "behind" {
			e.Hint = "behind base"
		} else {
			e.Hint = "checking…"
		}
		return e, true
	}

	// blocked with the review side unsatisfied: the block is explained by the
	// missing approvals, so it belongs in My PRs (collecting approvals) — a red
	// non-required check must not send it to Failed. The required-check list is
	// hidden behind rulesets, so this ordering is what keeps failed-check blame
	// honest: checks are only blamed once the PR is fully approved.
	if status.ReviewDecision == "CHANGES_REQUESTED" {
		e.Category, e.Hint = CategoryMine, "changes requested"
		return e, true
	}
	if e.Approvals < d.minApprovals || status.ReviewDecision == "REVIEW_REQUIRED" {
		e.Category, e.Hint = CategoryMine, "needs approvals"
		return e, true
	}

	// blocked while fully approved: distinguish a dead check failure (→ Failed)
	// from something still workable (pending, behind, or a re-approval gate →
	// stays in My PRs). If we can't determine behind or the checks, stay
	// conservative and keep it in My PRs rather than declaring it dead-failed.
	behind, err := d.fetcher.BehindBy(ctx, d.owner, d.repo, base, head)
	if err != nil {
		d.logf("dashboard compare PR #%d: %v", pr.Number, err)
		e.Category, e.Hint = CategoryMine, "blocked"
		return e, true
	}
	runs, err := d.fetcher.CheckRuns(ctx, d.owner, d.repo, head)
	if err != nil {
		d.logf("dashboard checks PR #%d: %v", pr.Number, err)
		e.Category, e.Hint = CategoryMine, "blocked"
		return e, true
	}

	pending, failed := merge.CountChecks(runs)

	switch {
	case pending > 0:
		e.Category, e.Hint = CategoryMine, "checking…"
	case behind > 0:
		e.Category, e.Hint = CategoryMine, "behind — update to re-run"
	case failed > 0:
		e.Category, e.Hint = CategoryFailed, "required check failed"
	default:
		e.Category, e.Hint = CategoryMine, "needs re-approval"
	}

	return e, true
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
