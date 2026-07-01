package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"mergebot/internal/ghclient"
	"mergebot/internal/merge"
	"mergebot/internal/queue"
	"mergebot/internal/review"
	"mergebot/internal/server"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-version":
			fmt.Println("mergebot", version)
			return nil
		}
	}

	// Load a local .env file if present. Variables already set in the real
	// environment take precedence over the file.
	_ = godotenv.Load()

	if len(os.Args) > 1 && os.Args[1] == "serve" {
		return runServe(os.Args[2:])
	}

	return runOnce(os.Args[1:])
}

// commonConfig holds the merge settings shared by both modes.
type commonConfig struct {
	repo             *string
	interval         *time.Duration
	timeout          *time.Duration
	mergeMethod      *string
	minApprovals     *int
	rateLimitWait    *time.Duration
	rateLimitRetries *int
	allowUnstable    *bool
	allowUnresolved  *bool
}

func registerCommon(fs *flag.FlagSet) commonConfig {
	return commonConfig{
		repo:             fs.String("repo", envOr("MERGEBOT_REPO", ""), "target repository in owner/name form (required)"),
		interval:         fs.Duration("interval", envDurationOr("MERGEBOT_INTERVAL", 30*time.Second), "how often to re-check a PR"),
		timeout:          fs.Duration("timeout", envDurationOr("MERGEBOT_TIMEOUT", 60*time.Minute), "give up on a PR after this duration"),
		mergeMethod:      fs.String("merge-method", envOr("MERGEBOT_MERGE_METHOD", "squash"), "merge method: squash, merge or rebase"),
		minApprovals:     fs.Int("min-approvals", envIntOr("MERGEBOT_MIN_APPROVALS", 2), "required number of approving reviews (0 disables the check)"),
		rateLimitWait:    fs.Duration("rate-limit-wait", envDurationOr("MERGEBOT_RATE_LIMIT_WAIT", 15*time.Minute), "max time to wait out a GitHub rate limit before treating it as transient (0 disables waiting)"),
		rateLimitRetries: fs.Int("rate-limit-retries", envIntOr("MERGEBOT_RATE_LIMIT_RETRIES", 3), "max retries while waiting out a rate limit"),
		allowUnstable:    fs.Bool("allow-unstable", envBoolOr("MERGEBOT_ALLOW_UNSTABLE", false), "merge even when non-required checks are failing"),
		allowUnresolved:  fs.Bool("allow-unresolved", envBoolOr("MERGEBOT_ALLOW_UNRESOLVED", false), "merge even with unresolved review threads or requested changes"),
	}
}

// validate checks the flags shared by both modes.
func (c commonConfig) validate() error {
	if *c.interval <= 0 {
		return fmt.Errorf("--interval must be positive, got %s", *c.interval)
	}
	if *c.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive, got %s", *c.timeout)
	}
	switch *c.mergeMethod {
	case "squash", "merge", "rebase":
	default:
		return fmt.Errorf("--merge-method must be squash, merge or rebase, got %q", *c.mergeMethod)
	}
	if *c.minApprovals < 0 {
		return fmt.Errorf("--min-approvals cannot be negative, got %d", *c.minApprovals)
	}
	if *c.rateLimitWait < 0 {
		return fmt.Errorf("--rate-limit-wait cannot be negative, got %s", *c.rateLimitWait)
	}
	if *c.rateLimitRetries < 0 {
		return fmt.Errorf("--rate-limit-retries cannot be negative, got %d", *c.rateLimitRetries)
	}

	return nil
}

// newClient builds a GitHub client from the shared rate-limit settings.
func (c commonConfig) newClient(token string) *ghclient.Client {
	return ghclient.New(token,
		ghclient.WithRateLimitWait(*c.rateLimitWait),
		ghclient.WithRateLimitRetries(*c.rateLimitRetries),
	)
}

func splitRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return "", "", fmt.Errorf("invalid --repo %q, want owner/name", repo)
	}

	return owner, name, nil
}

func token() (string, error) {
	t := os.Getenv("GITHUB_TOKEN")
	if t == "" {
		return "", fmt.Errorf("GITHUB_TOKEN is not set (try: export GITHUB_TOKEN=$(gh auth token))")
	}

	return t, nil
}

func runOnce(args []string) error {
	fs := flag.NewFlagSet("mergebot", flag.ContinueOnError)
	cfg := registerCommon(fs)
	dryRun := fs.Bool("dry-run", envBoolOr("MERGEBOT_DRY_RUN", false), "report the decision without updating or merging anything")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if err := cfg.validate(); err != nil {
		return err
	}

	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("expected exactly one PR number argument")
	}

	number, err := strconv.Atoi(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("invalid PR number %q: %w", fs.Arg(0), err)
	}

	owner, name, err := splitRepo(*cfg.repo)
	if err != nil {
		return err
	}

	tok, err := token()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runner := merge.Runner{
		Client:          cfg.newClient(tok),
		Owner:           owner,
		Repo:            name,
		Number:          number,
		Interval:        *cfg.interval,
		Timeout:         *cfg.timeout,
		MergeMethod:     *cfg.mergeMethod,
		MinApprovals:    *cfg.minApprovals,
		AllowUnstable:   *cfg.allowUnstable,
		AllowUnresolved: *cfg.allowUnresolved,
		DryRun:          *dryRun,
		Logf:            logf,
	}

	return runner.Run(ctx)
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("mergebot serve", flag.ContinueOnError)
	cfg := registerCommon(fs)
	addr := fs.String("addr", envOr("MERGEBOT_ADDR", "127.0.0.1:8080"), "address to listen on")
	statePath := fs.String("state", envOr("MERGEBOT_STATE", "mergebot-queue.json"), "path to the queue state file")
	recheck := fs.Duration("recheck-interval", envDurationOr("MERGEBOT_RECHECK_INTERVAL", 5*time.Minute), "how often to re-check parked (needs-approvals) PRs; 0 disables")
	concurrency := fs.Int("concurrency", envIntOr("MERGEBOT_CONCURRENCY", 1), "how many PRs to drive in parallel")
	reviewAuthor := fs.String("review-author", envOr("MERGEBOT_REVIEW_AUTHOR", ""), "GitHub login for the My Open PR dashboard (default: token owner)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if err := cfg.validate(); err != nil {
		return err
	}
	if *concurrency < 1 {
		return fmt.Errorf("--concurrency must be at least 1, got %d", *concurrency)
	}
	if *recheck < 0 {
		return fmt.Errorf("--recheck-interval cannot be negative, got %s", *recheck)
	}

	owner, name, err := splitRepo(*cfg.repo)
	if err != nil {
		return err
	}

	tok, err := token()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := cfg.newClient(tok)
	mgr := queue.New(client, queue.Config{
		Owner:           owner,
		Repo:            name,
		Interval:        *cfg.interval,
		Timeout:         *cfg.timeout,
		RecheckInterval: *recheck,
		MergeMethod:     *cfg.mergeMethod,
		MinApprovals:    *cfg.minApprovals,
		Concurrency:     *concurrency,
		AllowUnstable:   *cfg.allowUnstable,
		AllowUnresolved: *cfg.allowUnresolved,
	}, *statePath, logf)

	if err := mgr.Load(); err != nil {
		return fmt.Errorf("load queue state: %w", err)
	}

	go mgr.Run(ctx)

	dashboard := review.NewDashboard(client, owner, name, *cfg.minApprovals, *reviewAuthor, logf)
	go dashboard.RefreshLoop(ctx, *recheck)

	srv := &http.Server{
		Addr:    *addr,
		Handler: server.New(mgr, *cfg.repo, dashboard).Handler(),
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logf("mergebot serving %s/%s on http://%s", owner, name, *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

func logf(format string, args ...any) {
	fmt.Printf(time.Now().Format("15:04:05")+" "+format+"\n", args...)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}

func envDurationOr(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}

	d, err := time.ParseDuration(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: invalid %s=%q, using default %s\n", key, v, def)
		return def
	}

	return d
}

func envIntOr(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}

	n, err := strconv.Atoi(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: invalid %s=%q, using default %d\n", key, v, def)
		return def
	}

	return n
}

func envBoolOr(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}

	b, err := strconv.ParseBool(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: invalid %s=%q, using default %t\n", key, v, def)
		return def
	}

	return b
}
