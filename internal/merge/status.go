package merge

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// StatusClient is the GitHub subset the status prober needs.
type StatusClient interface {
	CreateComment(ctx context.Context, owner, repo string, number int, body string) error
	ListComments(ctx context.Context, owner, repo string, number int, since time.Time) ([]Comment, error)
}

// statusReplyMarker identifies the queue bot's reply to a /status command.
const statusReplyMarker = "merge queue status"

// StatusProber asks the external queue bot for a PR's queue status by posting a
// /status comment and reading the bot's reply. On-demand only — every probe
// adds two comments to the PR conversation.
type StatusProber struct {
	Client StatusClient
	Owner  string
	Repo   string

	// PollEvery / WaitFor bound how the reply is awaited; zero values default
	// to 2s / 20s.
	PollEvery time.Duration
	WaitFor   time.Duration
}

// Probe posts /status and returns a compact summary of the bot's reply.
func (p StatusProber) Probe(ctx context.Context, number int) (string, error) {
	poll, wait := p.PollEvery, p.WaitFor
	if poll <= 0 {
		poll = 2 * time.Second
	}
	if wait <= 0 {
		wait = 20 * time.Second
	}

	asked := time.Now()
	if err := p.Client.CreateComment(ctx, p.Owner, p.Repo, number, "/status"); err != nil {
		return "", fmt.Errorf("post /status: %w", err)
	}

	deadline := time.Now().Add(wait)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}

		comments, err := p.Client.ListComments(ctx, p.Owner, p.Repo, number, asked)
		if err != nil {
			continue // transient; keep waiting for the reply
		}

		for _, c := range comments {
			if c.CreatedAt.After(asked) && strings.Contains(strings.ToLower(c.Body), statusReplyMarker) {
				return summarizeStatus(c.Body), nil
			}
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("no /status reply from the queue bot within %s", wait)
		}
	}
}

// summarizeStatus reduces the bot's markdown reply to the informative lines,
// stripping markdown noise. Unknown formats fall back to the trimmed body.
func summarizeStatus(body string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.ReplaceAll(line, "`", ""))
		switch {
		case line == "":
		case strings.HasPrefix(line, "Queue length:"),
			strings.HasPrefix(line, "Batch start threshold:"),
			strings.HasPrefix(line, "Next validation batch:"),
			strings.HasPrefix(line, "Batch wait window:"),
			strings.HasPrefix(line, "Validation batches:"),
			strings.HasPrefix(line, "This PR:"),
			strings.Contains(line, "currently in the"):
			out = append(out, line)
		}
	}

	if len(out) == 0 {
		return strings.TrimSpace(body)
	}

	return strings.Join(out, "\n")
}
