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
	"github.com/wallester/mergebot/internal/ghclient"
	"github.com/wallester/mergebot/internal/merge"
	"github.com/wallester/mergebot/internal/queue"
	"github.com/wallester/mergebot/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
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
	repo            *string
	interval        *time.Duration
	timeout         *time.Duration
	mergeMethod     *string
	allowUnstable   *bool
	allowUnresolved *bool
}

func registerCommon(fs *flag.FlagSet) commonConfig {
	return commonConfig{
		repo:            fs.String("repo", envOr("MERGEBOT_REPO", "wallester/monorepo"), "target repository in owner/name form"),
		interval:        fs.Duration("interval", envDurationOr("MERGEBOT_INTERVAL", 30*time.Second), "how often to re-check a PR"),
		timeout:         fs.Duration("timeout", envDurationOr("MERGEBOT_TIMEOUT", 60*time.Minute), "give up on a PR after this duration"),
		mergeMethod:     fs.String("merge-method", envOr("MERGEBOT_MERGE_METHOD", "squash"), "merge method: squash, merge or rebase"),
		allowUnstable:   fs.Bool("allow-unstable", envBoolOr("MERGEBOT_ALLOW_UNSTABLE", false), "merge even when non-required checks are failing"),
		allowUnresolved: fs.Bool("allow-unresolved", envBoolOr("MERGEBOT_ALLOW_UNRESOLVED", false), "merge even with unresolved review threads or requested changes"),
	}
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
		Client:          ghclient.New(tok),
		Owner:           owner,
		Repo:            name,
		Number:          number,
		Interval:        *cfg.interval,
		Timeout:         *cfg.timeout,
		MergeMethod:     *cfg.mergeMethod,
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
	if err := fs.Parse(args); err != nil {
		return err
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

	mgr := queue.New(ghclient.New(tok), queue.Config{
		Owner:           owner,
		Repo:            name,
		Interval:        *cfg.interval,
		Timeout:         *cfg.timeout,
		MergeMethod:     *cfg.mergeMethod,
		AllowUnstable:   *cfg.allowUnstable,
		AllowUnresolved: *cfg.allowUnresolved,
	}, *statePath, logf)

	if err := mgr.Load(); err != nil {
		return fmt.Errorf("load queue state: %w", err)
	}

	go mgr.Run(ctx)

	srv := &http.Server{
		Addr:    *addr,
		Handler: server.New(mgr, *cfg.repo).Handler(),
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
