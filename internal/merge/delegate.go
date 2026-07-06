package merge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-github/v66/github"
)

// Comment is a pull-request conversation comment, reduced to what the delegate
// runner needs to spot queue-bot feedback.
type Comment struct {
	Author    string
	Body      string
	CreatedAt time.Time
}

// QueueClient is the GitHub subset the delegate runner needs: enough to hand a
// PR to a comment-driven team merge queue and watch the outcome.
type QueueClient interface {
	GetPullRequest(ctx context.Context, owner, repo string, number int) (*github.PullRequest, error)
	CheckRuns(ctx context.Context, owner, repo, ref string) ([]CheckRun, error)
	CreateComment(ctx context.Context, owner, repo string, number int, body string) error
	ListComments(ctx context.Context, owner, repo string, number int, since time.Time) ([]Comment, error)
}

// The team queue bot's comment protocol, as observed:
//   - "/queue" enqueues; the bot replies with a status block;
//   - a reply containing "waiting at queue position" confirms the PR is queued;
//   - "Could not queue PR" is a rejection (the bot does not retry on its own);
//   - "is not currently in the custom merge queue" reports a dequeued/unknown PR;
//   - "Validation batch ... started/failed" reports batch progress.
const (
	cmdQueue   = "/queue"
	cmdDequeue = "/dequeue"

	queuedMarker    = "waiting at queue position"
	rejectionMarker = "Could not queue PR"
	notQueuedMarker = "is not currently in the custom merge queue"
	batchMarker     = "Validation batch"
)

// DelegateRunner drives a pull request through an external comment-driven merge
// queue: it posts /queue and then only watches — the external queue owns
// validating and merging. There is deliberately no deadline: batch queues can
// hold a PR for a long time, and the external queue owns the pacing.
type DelegateRunner struct {
	Client   QueueClient
	Owner    string
	Repo     string
	Number   int
	Interval time.Duration
	Logf     func(format string, args ...any)
}

// Run enqueues the PR and polls until it is merged (nil), closed without
// merging, rejected by the queue bot, or ctx is cancelled.
func (r DelegateRunner) Run(ctx context.Context) error {
	// If the PR is already in the queue (enqueued via GitHub or a previous run),
	// don't re-ask — just watch. The lookback keeps old, superseded signals out.
	watchFrom := time.Now().Add(-7 * 24 * time.Hour)
	asked := false
	if sig := r.latestSignal(ctx, watchFrom); sig != nil && sig.kind == sigQueued {
		r.Logf("PR #%d is already in the team queue; watching", r.Number)
		asked = true
		watchFrom = sig.at
	}

	lastMsg := ""
	for {
		pr, err := r.Client.GetPullRequest(ctx, r.Owner, r.Repo, r.Number)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.Logf("get PR #%d: %v; will re-check", r.Number, err)

		case pr.GetMerged():
			r.Logf("PR #%d merged by the team queue", r.Number)
			return nil

		case pr.GetState() != "open":
			return fmt.Errorf("PR #%d is %s, not open", r.Number, pr.GetState())

		case !asked:
			// The bot rejects PRs whose checks are not green, so gate the /queue:
			// wait out running checks, decline on a completed failure.
			runs, err := r.Client.CheckRuns(ctx, r.Owner, r.Repo, pr.GetHead().GetSHA())
			if err != nil {
				r.Logf("list checks for PR #%d: %v; will re-check", r.Number, err)
				break
			}

			pending, failed := CountChecks(runs)
			switch {
			case pending > 0:
				r.Logf("PR #%d: waiting for %d running check(s) before queueing (%d failed so far)", r.Number, pending, failed)
			case failed > 0:
				return fmt.Errorf("PR #%d has %d failed check(s); the team queue would reject it: %w", r.Number, failed, ErrRequiredCheckFailed)
			default:
				if err := r.Client.CreateComment(ctx, r.Owner, r.Repo, r.Number, cmdQueue); err != nil {
					r.Logf("post %s on PR #%d: %v; will retry", cmdQueue, r.Number, err)
				} else {
					asked = true
					watchFrom = time.Now()
					r.Logf("posted %s on PR #%d; waiting for the team queue", cmdQueue, r.Number)
				}
			}

		default:
			sig := r.latestSignal(ctx, watchFrom)
			if sig != nil {
				switch sig.kind {
				case sigRejected:
					return fmt.Errorf("PR #%d rejected by the team queue: %s", r.Number, sig.detail)
				case sigNotQueued:
					return fmt.Errorf("PR #%d left the team queue without merging", r.Number)
				default:
					if sig.detail != lastMsg {
						lastMsg = sig.detail
						r.Logf("PR #%d: %s", r.Number, sig.detail)
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.Interval):
		}
	}
}

type signalKind int

const (
	sigQueued signalKind = iota
	sigRejected
	sigNotQueued
	sigBatch
)

type queueSignal struct {
	kind   signalKind
	detail string
	at     time.Time
}

// latestSignal returns the newest queue-bot signal after since, or nil.
func (r DelegateRunner) latestSignal(ctx context.Context, since time.Time) *queueSignal {
	comments, err := r.Client.ListComments(ctx, r.Owner, r.Repo, r.Number, since)
	if err != nil {
		r.Logf("list comments of PR #%d: %v; will re-check", r.Number, err)
		return nil
	}

	var latest *queueSignal
	for _, c := range comments {
		if !c.CreatedAt.After(since) {
			continue
		}
		if sig := parseQueueSignal(c); sig != nil {
			if latest == nil || sig.at.After(latest.at) {
				latest = sig
			}
		}
	}

	return latest
}

// QueuedFromComments reports whether the newest queue signal among the comments
// says the PR is in the team queue (a batch in progress counts as queued). Used
// by the review dashboard to flag externally-queued PRs.
func QueuedFromComments(comments []Comment) bool {
	var latest *queueSignal
	for _, c := range comments {
		if sig := parseQueueSignal(c); sig != nil {
			if latest == nil || sig.at.After(latest.at) {
				latest = sig
			}
		}
	}

	return latest != nil && (latest.kind == sigQueued || latest.kind == sigBatch)
}

// parseQueueSignal classifies one comment, or returns nil for unrelated ones.
func parseQueueSignal(c Comment) *queueSignal {
	switch {
	case strings.Contains(c.Body, rejectionMarker):
		return &queueSignal{kind: sigRejected, detail: strings.TrimSpace(c.Body), at: c.CreatedAt}
	case strings.Contains(c.Body, queuedMarker):
		return &queueSignal{kind: sigQueued, detail: firstLineWith(c.Body, queuedMarker), at: c.CreatedAt}
	case strings.Contains(c.Body, notQueuedMarker):
		return &queueSignal{kind: sigNotQueued, at: c.CreatedAt}
	case strings.Contains(c.Body, batchMarker):
		return &queueSignal{kind: sigBatch, detail: firstLineWith(c.Body, batchMarker), at: c.CreatedAt}
	default:
		return nil
	}
}

// firstLineWith returns the (markdown-stripped) line of body containing marker.
func firstLineWith(body, marker string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, marker) {
			return strings.TrimSpace(strings.ReplaceAll(line, "`", ""))
		}
	}

	return marker
}
