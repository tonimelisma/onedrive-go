# Learnings — Institutional Knowledge Base

Knowledge captured during implementation. Architecture-independent patterns and gotchas carried forward from Phases 1-3 archive.

---

## 1. QuickXorHash (`pkg/quickxorhash/`)

**Plan test vectors were wrong.** The test vectors specified in the plan were incorrect. Verified against rclone v1.73.1's `quickxorhash` package, which is verified against Microsoft's reference C# implementation. Lesson: always verify test vectors against a known-good reference, don't blindly trust specs.

---

## 2. Config TOML Gotchas

- **Chunk size default "10MiB" not "10MB".** 10 MB (decimal, 10,000,000 bytes) is NOT a multiple of 320 KiB (327,680 bytes). 10 MiB (10,485,760) IS a multiple (32 * 327,680). Default was changed to "10MiB" to maintain alignment validation.
- **`toml.MetaData` is 96 bytes** (triggers hugeParam). Must be passed by pointer. `toml.DecodeFile` returns it by value, so take its address when passing to helper functions.
- **misspell linter catches intentional typos in test TOML strings.** Test data for unknown-key-detection uses deliberate misspellings. Must use `//nolint:misspell` on those lines. Inline TOML strings (not raw string literals with backticks) make the nolint placement cleaner.

---

## 3. Agent Coordination

### Parallel agent execution
Four agents ran simultaneously without conflicts (true leaf packages). Scoped verification commands to own package to avoid cross-agent interference:
```bash
# Good: scoped to own package
go test ./internal/graph/...
# Bad: tests all packages, sees intermediate states from other agents
go test ./...
```

---

## 4. Graph Client (`internal/graph/`)

### Test-friendly time delays
Any code with `time.Sleep` or timer-based backoff must use an injectable sleep function. Pattern:
```go
type Client struct {
    sleepFunc func(ctx context.Context, d time.Duration) error // default: timeSleep
}
```
Tests override with `noopSleep` that returns immediately. Without this, retry tests took 70s instead of 1.4s.

### govet shadow checks are strict
This project enables `govet` with `enable-all: true`. Variable shadowing in nested scopes (e.g., `if err := ...` inside a block that already has `err`) triggers lint failures. Use distinct names like `sleepErr`, `readErr`.

### httptest is the right choice for Graph API tests
Decision: use `httptest.NewServer` for all Graph client tests. Real HTTP, no interfaces for mocking. Tests are realistic and simple. Confirmed this works well in 1.1 and 1.2.

### oauth2 fork for OnTokenChange
`golang.org/x/oauth2` has no persistence callback — when `ReuseTokenSource` silently refreshes a token, the new refresh token is only in memory. We use `github.com/tonimelisma/oauth2` fork (branch `on-token-change`) via `go.mod` replace directive. Adds `Config.OnTokenChange func(newToken *Token)` — fires after refresh, outside mutex, nil-safe. Tracks upstream proposal `golang/go#77502`.

### Public functions that depend on config paths need internal helpers for testability
Functions like `Login`, `Logout`, `TokenSourceFromProfile` call `config.ProfileTokenPath()` which resolves to real OS paths. Extract the actual logic into internal functions (`doLogin`, `logout`, `tokenSourceFromPath`) that accept explicit paths, and test those. The public wrappers are thin path-resolvers.

### oauth2 device code tests use real polling delays
Tests using `cfg.DeviceAccessToken()` incur real 1-second polling intervals (the minimum per RFC 8628). Set `"interval": 1` in mock device code responses to minimize delay, but tests still take ~1-3s each. Use `context.WithTimeout` for cancellation tests.

### Always check coverage before committing
Run `go test -coverprofile=/tmp/cover.out ./internal/graph/... && go tool cover -func=/tmp/cover.out | grep total` as part of the DOD check, not just build+test+lint. Coverage regressions are easy to miss when only running `go test` without `-coverprofile`.

### go.mod replace directive pseudo-version format
When using `replace` with a commit hash, the pseudo-version timestamp must match the commit's actual timestamp. Use `go mod download <module>@<commit>` first to discover the correct timestamp from error messages, then construct the pseudo-version as `v0.0.0-YYYYMMDDHHMMSS-<12-char-hash>`.

---

## 5. Integration Tests & CI

### Azure OIDC + Key Vault for CI token management
GitHub secrets can't be updated from within workflows, so we use Azure Key Vault as a writable secret store. OIDC federation means no stored credentials — GitHub Actions presents a short-lived JWT to Azure, scoped to `repo:tonimelisma/onedrive-go:ref:refs/heads/main`. Token files flow Key Vault <-> disk via `az keyvault secret download/set --file`, never through stdout/CI logs.

### Token and drive ID bootstrap
Tokens are bootstrapped via `go run . login --profile personal`. Drive IDs are discovered via `go run . whoami --json --profile personal | jq -r '.drives[0].id'`. Integration tests require `ONEDRIVE_TEST_DRIVE_ID` env var; CI discovers it via whoami. The old `cmd/integration-bootstrap` was deleted in 1.7 (B-025).

### POC code creates path dependency
When rewriting POC tests to use typed methods, audit for raw API patterns that survive by inertia. If a test helper uses raw `Do()` + `map[string]interface{}`, it biases all downstream tests toward that pattern. Prefer env vars or external tools for test prerequisites over inline raw API calls.

### Integration test build tag pattern
Integration tests use `//go:build integration` and are excluded from `go test ./...`. Run with `go test -tags=integration`. The `newIntegrationClient(t)` helper skips (not fails) when no token is available, so these tests degrade gracefully.

### Graph API returns 400 (not 404) for invalid item ID formats
Requesting `/me/drive/items/nonexistent-string` returns HTTP 400 ("invalidRequest"), not 404. The Graph API validates item ID format before lookup. Use path-based addressing (`/me/drive/root:/nonexistent-path`) to get proper 404 responses for nonexistent items.

### Nightly CI keeps refresh tokens alive
Microsoft rotates refresh tokens on use and they expire after 90 days of inactivity. The nightly schedule (3 AM UTC) ensures tokens stay active.

### Graph API JSON tag nolint patterns
Graph API uses non-standard JSON keys like `@odata.nextLink` and `@microsoft.graph.conflictBehavior`. These trigger the `tagliatelle` linter — suppress with `//nolint:tagliatelle` on the struct field.

### gofumpt stricter than gofmt on field alignment
`gofumpt` enforces stricter struct field alignment than `gofmt`. Multi-byte characters (em-dashes) in field comments can cause alignment differences. Always run `gofumpt -w` before committing, not just `gofmt`.

### httptest closure variable forward-reference
When an `httptest.NewServer` handler needs `srv.URL` (e.g., to build pagination URLs), declare `var srv *httptest.Server` first, then assign. Direct `srv := httptest.NewServer(...)` with a closure referencing `srv.URL` won't compile.

### Delta normalization pipeline design
The delta normalization pipeline applies four steps in order: (1) filter packages, (2) clear bogus hashes on deleted items, (3) deduplicate items keeping last occurrence, (4) reorder deletions before creations at the same parent. Each step is a separate unexported function for testability. The pipeline runs only on delta responses, not on single-item or list-children responses. `slices.SortStableFunc` is the right choice for deletion reordering because it preserves relative order of items at different parents.

### Delta token is always a full URL
The Graph API delta endpoint returns `@odata.deltaLink` and `@odata.nextLink` as full URLs (e.g., `https://graph.microsoft.com/v1.0/drives/{id}/root/delta?token=...`). The cleanest API design passes these opaque URLs back as tokens. Use `stripBaseURL()` to convert to a relative path for `Do()`. Empty token means initial sync, non-HTTP-prefixed strings are treated as initial sync too (defensive).

### gosec G602 false positive on backwards iteration
`gosec` flags `items[i]` as "slice index out of range" (G602) when iterating backwards with `for i := len(items) - 1; i >= 0; i--`. This is a false positive. Work around by copying the slice, reversing it with `slices.Reverse()`, and iterating forward. Alternatively, use `//nolint:gosec` but the reverse approach is cleaner.

### Parallel agent file conflicts during concurrent branch work
When multiple agents work on different increments in parallel using different git branches, `git checkout` destroys untracked files that exist on the current branch but not the target. Files must be staged immediately after creation and committed as quickly as possible to survive branch switching. Use single-shell-session heredocs (`cat > file << 'EOF' ... EOF && git add file`) to atomically create and stage files.

---

## 6. Tier 1 Research

16 research documents in `docs/tier1-research/` covering Graph API bugs, reference implementation analysis, and tool surveys. Consult these before implementing any API interaction — they contain critical gotchas (upload session resume, delta headers, hash fallbacks, etc.) tracked as B-015 through B-023 in BACKLOG.md.

---

## 7. Increment 1.6 — Drives (Me, Drives, Drive)

### No pivots from plan
The implementation followed the plan exactly. The `Me()`, `Drives()`, and `Drive()` methods, JSON response types, and conversion functions were implemented as specified.

### Graph API quirk: Personal accounts have empty mail field
The Graph API often returns an empty `mail` field for Personal (consumer) accounts. The `userPrincipalName` (UPN) field is the reliable fallback for email. The `toUser()` conversion function handles this with a simple fallback: use `mail` if non-empty, otherwise use `userPrincipalName`.

### Parallel agent file conflicts are severe
When multiple Claude Code agents work on the same package directory simultaneously, untracked files from one agent appear in another agent's working directory. This causes build failures, lint failures, and pre-commit hook failures. Key issues:
- `rm` + `git commit` is a race condition: files can reappear between delete and commit
- Branch switches carry untracked files across branches (they aren't branch-specific)
- Parallel agents can stage files into each other's git index
- **Mitigation**: Use separate branches (as documented) but coordinate to avoid parallel work in the same directory. Alternatively, use `.gitignore` for work-in-progress files from other increments.

### B-024 cleanup completed
The `cmd/integration-bootstrap/main.go` `printDrive()` function was updated to use the typed `Drives()` method instead of raw `Do()` + manual JSON parsing. This eliminated the `encoding/json` and `io` imports.

### Cross-package concern: Drives() vs /me/drive
The bootstrap tool previously used `GET /me/drive` (single default drive), but the new typed API uses `GET /me/drives` (all drives) and takes the first. This is functionally equivalent for Personal accounts (which have one drive) but may differ for Business accounts with multiple drives. The first drive in the list should be the user's primary drive, but this is not explicitly documented by Microsoft. Worth monitoring in integration tests.

---

## 8. Transfers (Increment 1.5)

### Pre-authenticated URLs bypass the Graph API
The `@microsoft.graph.downloadUrl` from GetItem and the `uploadUrl` from CreateUploadSession are pre-authenticated URLs that go directly to SharePoint/OneDrive storage. They must NOT use `Do()` (no base URL prefix, no auth headers). Use `httpClient.Do(req)` directly. These URLs contain embedded auth tokens and must NEVER be logged.

### SimpleUpload needs custom content type
`Do()` in client.go always sets `Content-Type: application/json` when body is non-nil. SimpleUpload needs `application/octet-stream`. Solution: a private `doRawUpload` helper that takes a contentType parameter. It also provides auth (unlike pre-authenticated URL calls) but does not retry (can't safely replay a partially-consumed reader).

### Upload chunk responses have three shapes
- 202 Accepted: intermediate chunk, body has `nextExpectedRanges` (drain and discard)
- 200 OK or 201 Created: final chunk complete, body has driveItem JSON
- Error: various HTTP error codes with error body

Use a `switch resp.StatusCode` to handle all three cases cleanly.

### URL encoding in file paths
When building upload paths like `/drives/{driveID}/items/{parentID}:/{name}:/content`, be careful with filenames containing URL-special characters. `%s` as a filename causes `net/http` to reject the URL as an invalid escape sequence. Test with valid filenames only.

### No retry for upload operations
Retrying a `SimpleUpload` or `UploadChunk` with a partially-consumed `io.Reader` would silently send incomplete data. The `doRawUpload` helper deliberately does not implement retry. For resumable uploads, the caller should use `UploadChunk` with fresh readers for each chunk.

---

## 9. CLI (Increments 1.7-1.8 + E2E 2.2)

### gochecknoinits forbids init() functions
The golangci-lint config enables `gochecknoinits`. Cobra CLI patterns that use `init()` to register commands and flags won't pass lint. Use constructor functions instead: `newRootCmd()` builds the root, calls `newLoginCmd()` etc. This is actually better — testable, no package-level mutable state.

### Cobra transitive dependency: mousetrap
Cobra depends on `github.com/inconshreveable/mousetrap` (Windows-only, detects "launched from Explorer"). Must be added to the depguard allow list alongside cobra and pflag. Always check transitive deps when adding new dependencies.

### graph.Item is 264 bytes — avoid range value copies
`gocritic:rangeValCopy` flags `for _, item := range items` when the struct is large. Use `for i := range items` with `items[i]` instead. This applies to any struct over ~128 bytes.

### dupl linter catches near-identical method pairs
`GetItem`/`GetItemByPath` and `ListChildren`/`ListChildrenByPath` had identical fetch+decode logic differing only in URL construction. The `dupl` linter flagged this. Solution: extract shared helpers (`fetchItem`, `fetchAllChildren`) that take the URL as a parameter.

### CLI output conventions
Status/error messages go to stderr (`fmt.Fprintf(os.Stderr, ...)`). Structured data output (JSON, tables) goes to stdout. This allows piping `onedrive-go ls --json / | jq ...` while still seeing status messages.

### E2E test pattern: build once, run as subprocess
E2E tests build the binary in `TestMain` to a temp dir, then run it via `os/exec` in each test. The binary path and profile are package-level vars set in `TestMain`. Tests use `t.Cleanup` for teardown of remote resources.

### Recursive mkdir with 409 Conflict handling
When creating nested folders, walk path segments and create each. If CreateFolder returns 409 (Conflict), the folder already exists — resolve it by path and continue with its ID as the parent. Track the `builtPath` as you go to enable path-based resolution.

---

## 10. Config Phase 3

### errWriter pattern for multi-write formatting
When a function makes many `fmt.Fprintf` calls (e.g., rendering config sections), each creates an uncoverable error branch. Solution: the `errWriter` pattern — wrap `io.Writer`, capture the first error, subsequent writes are no-ops. One `failWriter` test covers all error paths. Used in `show.go`.

### cmd.Flags().Changed() for pflag default disambiguation
pflag's default value is indistinguishable from an explicit `--flag=defaultValue` at the value level. Use `cmd.Flags().Changed("flag")` to detect whether the user actually passed the flag. Used in `root.go` for `--profile` to distinguish "not specified" from `--profile=default`.

### CLIOverrides pointer fields for nil-vs-zero-value
`CLIOverrides` uses `*string` / `*bool` for optional flags. `nil` means "not specified by user" (use config/env value), while `&false` means "user explicitly set to false" (override config). Without pointers, `--dry-run=false` would be indistinguishable from not passing `--dry-run`.

### Synthetic default profile for zero-config UX
When no config file exists, `Resolve()` creates a synthetic default profile (`AccountType: "personal"`, `SyncDir: "~/OneDrive"`). This means CLI commands work out-of-the-box without creating a config file first.

### File extraction is zero-risk refactoring
Extracting functions from oversized files (unknown.go from load.go, size.go from validate.go) is purely mechanical — move functions + their tests to new files, no logic changes. If tests pass before and after, the refactor is correct. Good way to reduce file size without introducing bugs.
