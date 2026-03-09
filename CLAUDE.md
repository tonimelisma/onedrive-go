# onedrive-go

Fast, safe OneDrive CLI and sync client in Go. Unix-style file ops (`ls`, `get`, `put`) plus bidirectional sync with conflict tracking. Linux and macOS. MIT licensed.

## Routing Table

| When modifying... | Read first | Also consult |
|-------------------|-----------|--------------|
| `internal/graph/` | `spec/design/graph-client.md` | `spec/reference/graph-api-quirks.md` |
| `internal/tokenfile/` | `spec/design/graph-client.md` | |
| `internal/config/` | `spec/design/config.md` | |
| `internal/driveid/`, `drive.go` | `spec/design/drive-identity.md` | |
| `internal/driveops/`, `get.go`, `put.go` | `spec/design/drive-transfers.md` | |
| `pkg/quickxorhash/` | `spec/design/drive-transfers.md` | |
| `internal/retry/` | `spec/design/retry.md` | |
| `internal/sync/observer_*.go`, `scanner.go`, `buffer.go` | `spec/design/sync-observation.md` | `spec/reference/onedrive-sync-behavior.md` |
| `internal/sync/planner.go`, `types.go` | `spec/design/sync-planning.md` | `spec/reference/onedrive-sync-behavior.md` |
| `internal/sync/executor*.go`, `worker.go`, `tracker.go`, `reconciler.go` | `spec/design/sync-execution.md` | |
| `internal/sync/engine*.go`, `orchestrator.go`, `drive_runner.go`, `sync.go` | `spec/design/sync-engine.md` | |
| `internal/sync/baseline.go`, `store_interfaces.go`, `migrations.go` | `spec/design/sync-store.md` | `spec/design/data-model.md` |
| Root package CLI files | `spec/design/cli.md` | |

| Working on capability... | Requirements | Design docs |
|--------------------------|-------------|-------------|
| R-1 File Operations | `spec/requirements/file-operations.md` | `spec/design/cli.md`, `spec/design/drive-transfers.md` |
| R-2 Sync | `spec/requirements/sync.md` | `spec/design/sync-*.md`, `spec/design/data-model.md` |
| R-3 Drive Management | `spec/requirements/drive-management.md` | `spec/design/drive-identity.md`, `spec/design/config.md` |
| R-4 Configuration | `spec/requirements/configuration.md` | `spec/design/config.md` |
| R-5 Transfers | `spec/requirements/transfers.md` | `spec/design/drive-transfers.md` |
| R-6 Non-Functional | `spec/requirements/non-functional.md` | `spec/design/system.md`, `spec/design/retry.md` |

Planned work: search `spec/` for `[planned]`. Reference docs: `spec/reference/`.

## Coding Rules

- Go 1.23+, `gofumpt` + `goimports -local github.com/tonimelisma/onedrive-go`
- US English (`canceled`, `marshaling`), comments explain **why**
- `log/slog` structured logging. Never log secrets. Levels: Debug (HTTP/tokens), Info (lifecycle), Warn (retries/fallbacks), Error (terminal failures)
- All assertions use `testify` (`assert` + `require`). Never `t.Fatal`/`t.Error` for assertions
- Table-driven tests, TDD (red/green/refactor), never pass nil context
- E2E tiered: `e2e` tag (fast, every push) vs `e2e_full` (slow, nightly). Requires `.env` with test accounts
- OAuth app ID: `d50ca740-7a5b-4916-88b6-e8152e0e5071` — same ID everywhere (code, scripts, CI)
- Sentinel errors with `%w`, functions do one thing, accept interfaces / return structs
- Three-group imports (stdlib / third-party / local), separated by blank lines

## Process

### 1. Claim work
Search `spec/` for `[planned]` items. Read the governing design doc and requirements file.

### 2. Branch
`<type>/<task-name>` (feat, fix, refactor, test, docs, chore). All changes go through PRs.

### 3. Develop (TDD)
Red (failing test) → Green (minimum code) → Refactor. No exceptions.

### 4. Update docs
- Design doc: update spec for changed behavior, new constraints
- Requirements: update status (`implemented` → `verified` when tests pass). Mirror in design doc `Implements:` line
- Reference: new API quirk → file it upstream. Don't update reference docs during implementation

### 5. Self-verify
Re-read governing design doc. Produce compliance report: each spec item → fully/partially/not implemented.

### 6. Quality gates

```bash
gofumpt -w . && goimports -local github.com/tonimelisma/onedrive-go -w . && go build ./... && go test -race -coverprofile=/tmp/cover.out ./... && golangci-lint run && go tool cover -func=/tmp/cover.out | grep total && go test -tags=e2e -race -v -parallel 5 -timeout=10m ./e2e/... && echo "ALL GATES PASS"
```

### 7. Ship
Push branch, `gh pr create`, `gh pr merge --auto --squash --delete-branch`. Branch protection: all 4 CI jobs must pass (`enforce_admins=true`).

### 8. Cleanup
```bash
cd /Users/tonimelisma/Development/onedrive-go && git fetch --prune origin && git checkout main && git pull --ff-only origin main
```
**Never delete other worktrees or branches.** Report them to the human.

## Multi-Agent

Multiple agents work concurrently. Check git status before committing. Investigate unexpected changes before overwriting.

## Test Credentials

Bootstrap: `./scripts/bootstrap-test-credentials.sh` (interactive, one-time). Migrate to CI: `./scripts/migrate-test-data-to-ci.sh`. CI auto-rotates tokens. Re-bootstrap if tokens expire (90 days idle).

## Engineering Philosophy

Always do the right thing. Engineering effort is free. Prefer ambitious long-term solutions over quick fixes. Every behavior change is TDD. You own this repo — a broken test is your responsibility. Fix it, don't work around it.
