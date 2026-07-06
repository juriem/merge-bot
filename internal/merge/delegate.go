package merge

import (
	"context"
	"fmt"
	"time"

	"github.com/google/go-github/v66/github"
)

// LabelClient is the GitHub subset the delegate runner needs: enough to hand a
// PR to an external label-driven merge queue and watch the outcome.
type LabelClient interface {
	GetPullRequest(ctx context.Context, owner, repo string, number int) (*github.PullRequest, error)
	AddLabel(ctx context.Context, owner, repo string, number int, label string) error
	RemoveLabel(ctx context.Context, owner, repo string, number int, label string) error
}

// DelegateRunner drives a pull request through an external merge queue that is
// triggered by a label (e.g. a team-wide "merge-queue" bot): it applies the
// label and then only watches — the external queue owns updating, validating
// and merging. There is deliberately no deadline: batch queues can hold a PR
// for a long time, and the external queue owns the pacing.
type DelegateRunner struct {
	Client   LabelClient
	Owner    string
	Repo     string
	Number   int
	Label    string
	Interval time.Duration
	Logf     func(format string, args ...any)
}

// Run labels the PR and polls until it is merged (nil), closed without merging,
// dequeued by someone removing the label, or ctx is cancelled.
func (r DelegateRunner) Run(ctx context.Context) error {
	labeled := false

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
				r.Logf("PR #%d is in the %q queue; waiting for it to merge", r.Number, r.Label)
			}
			labeled = true

		case !labeled:
			if err := r.Client.AddLabel(ctx, r.Owner, r.Repo, r.Number, r.Label); err != nil {
				r.Logf("add %q label to PR #%d: %v; will retry", r.Label, r.Number, err)
			} else {
				labeled = true
				r.Logf("handed PR #%d to the %q queue", r.Number, r.Label)
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

func hasLabel(pr *github.PullRequest, name string) bool {
	for _, l := range pr.Labels {
		if l.GetName() == name {
			return true
		}
	}

	return false
}
