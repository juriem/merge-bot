package ghclient

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestReviewStatus_Live exercises the real GraphQL endpoint. It is skipped
// unless MERGEBOT_LIVE=1, a GITHUB_TOKEN is present, MERGEBOT_LIVE_REPO names an
// owner/name repository and MERGEBOT_LIVE_PR names a pull request in it.
func TestReviewStatus_Live(t *testing.T) {
	if os.Getenv("MERGEBOT_LIVE") != "1" {
		t.Skip("set MERGEBOT_LIVE=1 to run the live GitHub integration test")
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		t.Skip("GITHUB_TOKEN not set")
	}

	owner, repo, ok := strings.Cut(os.Getenv("MERGEBOT_LIVE_REPO"), "/")
	if !ok || owner == "" || repo == "" {
		t.Skip("set MERGEBOT_LIVE_REPO to owner/name")
	}

	number, err := strconv.Atoi(os.Getenv("MERGEBOT_LIVE_PR"))
	if err != nil || number == 0 {
		t.Skip("set MERGEBOT_LIVE_PR to a pull request number")
	}

	status, err := New(token).ReviewStatus(context.Background(), owner, repo, number)
	if err != nil {
		t.Fatalf("ReviewStatus: %v", err)
	}

	t.Logf("decision=%q unresolved=%d more=%v", status.ReviewDecision, status.UnresolvedThreads, status.MoreThreads)
}
