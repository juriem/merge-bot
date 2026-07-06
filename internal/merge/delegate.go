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

// LabelClient is the GitHub subset the delegate runner needs: enough to hand a
// PR to an external label-driven merge queue and watch the outcome.
type LabelClient interface {
	GetPullRequest(ctx context.Context, owner, repo string, number int) (*github.PullRequest, error)
	AddLabel(ctx context.Context, owner, repo string, number int, label string) error
	RemoveLabel(ctx context.Context, owner, repo string, number int, label string) error
	CheckRuns(ctx context.Context, owner, repo, ref string) ([]CheckRun, error)
	ListComments(ctx context.Context, owner, repo string, number int, since time.Time) ([]Comment, error)
}

// rejectionMarker is how the external queue bot reports that it refused to
// enqueue a PR (it replies with a comment and leaves the label in place).
const rejectionMarker = "Could not queue PR"

// DelegateRunner drives a pull request through an external merge queue that is
// triggered by a label (e.g. a team-wide "merge-queue" bot): it applies the
// label and then only watches — the external queue owns updating, validating
// and merging. There is deliberately no deadline: batch queues can hold a PR
// for a long time, and the external queue owns the pacing.
//
// The label alone is not trusted as queue state: the bot refuses PRs (wrong
// base, red checks) by commenting and leaves the label behind, so the runner
// gates the handover on green checks and watches comments for rejections.
type DelegateRunner struct {
	Client   LabelClient
	Owner    string
	Repo     string
	Number   int
	Label    string
	Interval time.Duration
	Logf     func(format string, args ...any)
}

// Run hands the PR to the external queue and polls until it is merged (nil),
// closed without merging, dequeued, rejected by the queue bot, or ctx is
// cancelled.
func (r DelegateRunner) Run(ctx context.Context) error {
	labeled := false
	var handedAt time.Time // rejections are only counted after this moment

	for {
		pr, err := r.Client.GetPullRequest(ctx, r.Owner, r.Repo, r.Number)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.Logf("get PR #%d: %v; will re-check", r.Number, err)

		case pr.GetMerged():
			r.Logf("PR #%d merged by the delegated queue", r.Number)
			return nil

		case pr.GetState() != "open":
			return fmt.Errorf("PR #%d is %s, not open", r.Number, pr.GetState())

		case hasLabel(pr, r.Label):
			if !labeled {
				// Labeled outside mergebot: watch from now on. A rejection that
				// predates us is invisible — remove + retry re-triggers the bot.
				r.Logf("PR #%d is in the %q queue; waiting for it to merge", r.Number, r.Label)
				labeled = true
				handedAt = time.Now()
			}
			if reason := r.rejection(ctx, handedAt); reason != "" {
				// Drop the label so a retry re-applies it and the bot re-evaluates
				// (it does not retry on its own and leaves the label behind).
				if err := r.Client.RemoveLabel(ctx, r.Owner, r.Repo, r.Number, r.Label); err != nil {
					r.Logf("remove %q label from PR #%d: %v", r.Label, r.Number, err)
				}
				return fmt.Errorf("PR #%d rejected by the %q queue: %s", r.Number, r.Label, reason)
			}

		case !labeled:
			// The queue bot refuses PRs whose checks are not green, so gate the
			// handover: wait out running checks, decline on a completed failure.
			runs, err := r.Client.CheckRuns(ctx, r.Owner, r.Repo, pr.GetHead().GetSHA())
			if err != nil {
				r.Logf("list checks for PR #%d: %v; will re-check", r.Number, err)
				break
			}

			pending, failed := CountChecks(runs)
			switch {
			case pending > 0:
				r.Logf("PR #%d: waiting for %d running check(s) before handover (%d failed so far)", r.Number, pending, failed)
			case failed > 0:
				return fmt.Errorf("PR #%d has %d failed check(s); the %q queue would reject it: %w", r.Number, failed, r.Label, ErrRequiredCheckFailed)
			default:
				if err := r.Client.AddLabel(ctx, r.Owner, r.Repo, r.Number, r.Label); err != nil {
					r.Logf("add %q label to PR #%d: %v; will retry", r.Label, r.Number, err)
				} else {
					labeled = true
					handedAt = time.Now()
					r.Logf("handed PR #%d to the %q queue", r.Number, r.Label)
				}
			}

		default:
			// We saw the label earlier and it is gone while the PR is still open:
			// someone (or the queue bot) dequeued it.
			return fmt.Errorf("PR #%d left the %q queue without merging (label removed)", r.Number, r.Label)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.Interval):
		}
	}
}

// rejection returns the queue bot's rejection message posted after since, or ""
// when there is none (or the comments cannot be read right now).
func (r DelegateRunner) rejection(ctx context.Context, since time.Time) string {
	comments, err := r.Client.ListComments(ctx, r.Owner, r.Repo, r.Number, since)
	if err != nil {
		r.Logf("list comments of PR #%d: %v; will re-check", r.Number, err)
		return ""
	}

	for _, c := range comments {
		if c.CreatedAt.After(since) && strings.Contains(c.Body, rejectionMarker) {
			return strings.TrimSpace(c.Body)
		}
	}

	return ""
}

func hasLabel(pr *github.PullRequest, name string) bool {
	for _, l := range pr.Labels {
		if l.GetName() == name {
			return true
		}
	}

	return false
}
