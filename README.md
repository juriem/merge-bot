# mergebot

A small tool that takes a pull request number, waits until GitHub reports the PR
is ready, and merges it — automatically running **Update branch** whenever the PR
falls behind its base. It removes the manual "update → wait for CI → merge → it
went stale again" loop that happens when many developers merge into `main`.

It comes in two modes:

- **One-shot CLI** — drive a single PR to merged and exit.
- **Daemon + web UI** — a sequential merge queue you feed PR numbers from the
  browser.

## How readiness is decided

mergebot relies on GitHub's `mergeable_state`, the same signal the green
**Merge** button uses. It already aggregates required approvals, required status
checks, conflicts and branch freshness:

| `mergeable_state` | Action                                                         |
|-------------------|----------------------------------------------------------------|
| `clean`           | merge (squash by default)                                      |
| `behind`          | run **Update branch**, wait for CI to re-run, re-check         |
| `blocked`         | wait — missing approvals or required checks not green yet      |
| `unstable`        | wait (or merge with `--allow-unstable`)                        |
| `dirty`           | stop — merge conflicts need a human                            |
| `draft` / closed  | stop                                                           |
| `unknown`         | GitHub is still computing; re-check                            |

An already-merged PR is detected and left untouched.

### Review gate

Before merging, mergebot independently checks (via GraphQL) two things that
GitHub may not enforce through `mergeable_state` unless branch protection is
configured for them:

- **Unresolved review threads** — merge is held while any conversation is open.
- **Review decision** — held on `CHANGES_REQUESTED` or `REVIEW_REQUIRED`.

The reason is shown in the log and in the queue/History message. The gate is on
by default; pass `--allow-unresolved` (or `MERGEBOT_ALLOW_UNRESOLVED=true`) to
skip it.

## Install

```bash
go build -o mergebot .
```

Requires Go 1.26+.

## Authentication

mergebot needs a GitHub token (repo scope) in `GITHUB_TOKEN`. The simplest source
is the GitHub CLI:

```bash
export GITHUB_TOKEN=$(gh auth token)
```

Or put it in a local `.env` file (auto-loaded, git-ignored). Copy the template:

```bash
cp .env.example .env
# then set GITHUB_TOKEN=...
```

Real environment variables take precedence over `.env`.

## One-shot mode

```bash
mergebot 6585                 # poll PR #6585 and merge when ready
mergebot --dry-run 6585       # report what it would do, then exit
mergebot --interval 20s --timeout 90m 6585
```

## Daemon + web UI

```bash
mergebot serve                              # http://127.0.0.1:8080
mergebot serve --addr 127.0.0.1:9000 --state ~/.mergebot.json
```

Open the address in a browser: enter a PR number to enqueue it, watch the queue
status, and remove items. The page polls the API every few seconds. Two tabs:
**Queue** (queued + active) and **History** (merged / failed / stopped, newest
first).

The queue is **sequential** — one PR is driven to merged before the next starts,
matching how a GitHub merge queue behaves. It is persisted to the state file, so
the queue survives a restart (an in-flight PR is re-queued).

The UI is served on `127.0.0.1` only and has no authentication; run it locally.

### HTTP API

| Method | Path                  | Body              | Purpose            |
|--------|-----------------------|-------------------|--------------------|
| GET    | `/api/items`          | —                 | list the queue     |
| POST   | `/api/items`          | `{"number": 123}` | enqueue a PR       |
| DELETE | `/api/items/{number}` | —                 | stop / remove a PR |
| GET    | `/api/config`         | —                 | repo shown in UI   |

## Configuration

Every flag has an environment-variable fallback. Precedence:
**CLI flag → environment variable → `.env` → default**.

| Flag              | Env var                   | Default               | Notes                         |
|-------------------|---------------------------|-----------------------|-------------------------------|
| `--repo`          | `MERGEBOT_REPO`           | `wallester/monorepo`  | `owner/name`                  |
| `--interval`      | `MERGEBOT_INTERVAL`       | `30s`                 | re-check frequency            |
| `--timeout`       | `MERGEBOT_TIMEOUT`        | `60m`                 | give up on a PR after this    |
| `--merge-method`  | `MERGEBOT_MERGE_METHOD`   | `squash`              | `squash`, `merge` or `rebase` |
| `--allow-unstable`| `MERGEBOT_ALLOW_UNSTABLE` | `false`               | merge despite non-required red checks |
| `--allow-unresolved`| `MERGEBOT_ALLOW_UNRESOLVED` | `false`           | merge despite unresolved threads / requested changes |
| `--dry-run`       | `MERGEBOT_DRY_RUN`        | `false`               | one-shot mode only            |
| `--addr`          | `MERGEBOT_ADDR`           | `127.0.0.1:8080`      | `serve` only                  |
| `--state`         | `MERGEBOT_STATE`          | `mergebot-queue.json` | `serve` only                  |

## Releasing

Pushing a tag that starts with `v` triggers the
[`release`](.github/workflows/release.yml) workflow, which builds a macOS arm64
binary (with the tag baked into `mergebot --version`) and publishes a **draft**
GitHub Release with a `mergebot_<tag>_darwin_arm64.tar.gz` archive containing the
binary, `.env.example` and this README. Review the draft and publish it manually.

```bash
git tag v1.0.0
git push origin v1.0.0
```

## Queue lifecycle

```
queued → active → merged
                ↘ failed   (conflicts, timeout, API error)
                ↘ stopped  (removed by user / shutdown)
```
