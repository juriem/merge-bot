package merge

import (
	"context"
	"strings"
	"testing"
	"time"
)

// botReply is the real reply format observed from the team queue bot.
const botReply = "### Custom merge queue status\n\n" +
	"Repository: `wallester/monorepo`\n" +
	"Base branch: `main`\n" +
	"Queue length: `3` PRs currently waiting\n" +
	"Batch start threshold: `5` PRs, or any queued PRs after the wait window elapses\n" +
	"Next validation batch: ready to start with `3` queued PRs because the wait window elapsed\n" +
	"Batch wait window: `16m` elapsed / `15m` total, elapsed; smaller batch can start on the next scheduler run\n" +
	"Validation batches: `0`\n\n" +
	"`wallester/monorepo#7416` is not currently in the custom merge queue.\n"

type fakeStatusClient struct {
	posted  []string
	replies []Comment
}

func (f *fakeStatusClient) CreateComment(_ context.Context, _, _ string, _ int, body string) error {
	f.posted = append(f.posted, body)
	return nil
}

func (f *fakeStatusClient) ListComments(context.Context, string, string, int, time.Time) ([]Comment, error) {
	return f.replies, nil
}

func Test_Probe_PostsStatusAndSummarizesReply(t *testing.T) {
	// Arrange
	f := &fakeStatusClient{replies: []Comment{{
		Author:    "wallester-releases",
		Body:      botReply,
		CreatedAt: time.Now().Add(time.Hour),
	}}}
	p := StatusProber{Client: f, Owner: "o", Repo: "r", PollEvery: time.Millisecond, WaitFor: time.Second}

	// Act
	got, err := p.Probe(context.Background(), 7416)

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.posted) != 1 || f.posted[0] != "/status" {
		t.Fatalf("posted = %v, want [/status]", f.posted)
	}
	for _, want := range []string{"Queue length: 3", "Batch start threshold: 5", "is not currently in the"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "`") {
		t.Fatalf("summary should be stripped of markdown backticks:\n%s", got)
	}
}

func Test_Probe_TimesOutWithoutReply(t *testing.T) {
	// Arrange
	f := &fakeStatusClient{}
	p := StatusProber{Client: f, Owner: "o", Repo: "r", PollEvery: time.Millisecond, WaitFor: 10 * time.Millisecond}

	// Act
	_, err := p.Probe(context.Background(), 1)

	// Assert
	if err == nil {
		t.Fatal("expected a timeout error when the bot never replies")
	}
}
