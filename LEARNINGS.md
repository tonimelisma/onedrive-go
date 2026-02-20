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
Public auth functions (`Login`, `Logout`, `TokenSourceFromPath`) accept explicit `tokenPath` parameters — the caller computes the path via `config.DriveTokenPath(canonicalID)`. This decouples `graph/` from `config/` entirely. Internal test helpers (`doLogin`, `logout`) accept the same explicit paths for testability.

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
Tokens are bootstrapped via `go run . login --drive personal:user@example.com`. Drive IDs are discovered via `go run . whoami --json --drive personal:user@example.com | jq -r '.drives[0].id'`. Integration tests require `ONEDRIVE_TEST_DRIVE_ID` env var; CI discovers it via whoami. The old `cmd/integration-bootstrap` was deleted in 1.7 (B-025).

### POC code creates path dependency
When rewriting POC tests to use typed methods, audit for raw API patterns that survive by inertia. If a test helper uses raw `Do()` + `map[string]interface{}`, it biases all downstream tests toward that pattern. Prefer env vars or external tools for test prerequisites over inline raw API calls.

### Integration test build tag pattern
Integration tests use `//go:build integration` and are excluded from `go test ./...`. Run with `go test -tags=integration`. The `newIntegrationClient(t)` helper skips (not fails) when no token is available, so these tests degrade gracefully.

### Graph API returns 400 (not 404) for invalid item ID formats
Requesting `/me/drive/items/nonexistent-string` returns HTTP 400 ("invalidRequest"), not 404. The Graph API validates item ID format before lookup. Use path-based addressing (`/me/drive/root:/nonexistent-path`) to get proper 404 responses for nonexistent items.

### Nightly CI keeps refresh tokens alive
Microsoft rotates refresh tokens on use and they expire after 90 days of inactivity. The nightly schedule (3 AM UTC) ensures tokens stay active.

### Orchestrator manages Key Vault secrets directly
The AI orchestrator (Claude) has `az` CLI access and should manage Key Vault secrets as part of CI-impacting changes. This includes: creating/renaming secrets, downloading/uploading tokens, setting GitHub repository variables via `gh variable set`, and verifying secret structure. When code changes affect token paths or secret naming (like the profiles → drives migration), update Key Vault and GitHub variables in the same increment rather than escalating to the human. The human only handles one-time Azure infrastructure (service principal, RBAC) and interactive browser-based `login` flows.

### Local CI validation prevents push-and-pray
When making changes that affect the integration workflow (token paths, secret names, env vars, workflow YAML), always validate locally before pushing. Mirror the workflow's token loading logic with `az keyvault secret download`, test `whoami --json --drive`, and run E2E tests locally with the same env vars. See test-strategy.md §6.1 for the full local validation script. This catches issues like wrong secret names or broken token paths without waiting for GitHub Actions.

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
E2E tests build the binary in `TestMain` to a temp dir, then run it via `os/exec` in each test. The binary path and drive are package-level vars set in `TestMain`. Tests use `t.Cleanup` for teardown of remote resources.

### Recursive mkdir with 409 Conflict handling
When creating nested folders, walk path segments and create each. If CreateFolder returns 409 (Conflict), the folder already exists — resolve it by path and continue with its ID as the parent. Track the `builtPath` as you go to enable path-based resolution.

---

## 10. Config Phase 3

### errWriter pattern for multi-write formatting
When a function makes many `fmt.Fprintf` calls (e.g., rendering config sections), each creates an uncoverable error branch. Solution: the `errWriter` pattern — wrap `io.Writer`, capture the first error, subsequent writes are no-ops. One `failWriter` test covers all error paths. Used in `show.go`.

### cmd.Flags().Changed() for pflag default disambiguation
pflag's default value is indistinguishable from an explicit `--flag=defaultValue` at the value level. Use `cmd.Flags().Changed("flag")` to detect whether the user actually passed the flag. Used in `root.go` for `--drive` to distinguish "not specified" from an explicitly specified drive selector.

### CLIOverrides pointer fields for nil-vs-zero-value
`CLIOverrides` uses `*string` / `*bool` for optional flags. `nil` means "not specified by user" (use config/env value), while `&false` means "user explicitly set to false" (override config). Without pointers, `--dry-run=false` would be indistinguishable from not passing `--dry-run`.

### Synthetic default drive for zero-config UX
When no config file exists, `ResolveDrive()` creates a synthetic default drive with `SyncDir: "~/OneDrive"`. This means CLI commands work out-of-the-box without creating a config file first.

### File extraction is zero-risk refactoring
Extracting functions from oversized files (unknown.go from load.go, size.go from validate.go) is purely mechanical — move functions + their tests to new files, no logic changes. If tests pass before and after, the refactor is correct. Good way to reduce file size without introducing bugs.

### Always wait for integration tests after merge
Unit tests passing is NOT sufficient to declare an increment done. The `integration.yml` workflow runs `--drive personal:user@example.com` against real OneDrive — this caught a regression in the old profile system where `Resolve()` always created a synthetic profile, breaking CI. **DOD now requires waiting for integration tests to pass on main before proceeding.** (PR #21 fix)

---

## 11. Config Rewrite — Profiles → Drives (Increment 4.x)

### BurntSushi/toml embedded struct field promotion works without tags
BurntSushi/toml natively promotes embedded struct fields for decoding. No `toml:",squash"` tag needed (that's a mapstructure concept). Just embed the struct directly:
```go
type Config struct {
    FilterConfig      // flat TOML keys map to FilterConfig fields
    TransfersConfig   // etc.
    Drives map[string]Drive `toml:"-"` // custom two-pass parsing
}
```

### Two-pass TOML decode for mixed-key configs
When a TOML file has flat global keys and quoted table sections (drive IDs with `:` and `@`), a single `toml.Decode` can't handle both. Solution: Pass 1 decodes globals into embedded structs (TOML library reports drive sections as "undecoded"). Pass 2 decodes into `map[string]any`, extracts keys containing `:` as drive sections, and converts each via re-encode/decode through `mapToDrive()`.

### staticcheck QF1008 is aggressive about embedded field selectors
With embedded structs, `cfg.FilterConfig.SkipDotfiles` and `cfg.SkipDotfiles` are equivalent. staticcheck's QF1008 flags the explicit form. Use the short (promoted) form everywhere for consistency, even in tests. This affects both source and test code.

### gocritic rangeValCopy and hugeParam with Drive struct (144 bytes)
The `Drive` struct with its string fields and slice pointers hits the 128-byte threshold. Must use index-based iteration (`for id := range cfg.Drives { cfg.Drives[id] }`) and pointer parameters to avoid lint failures.

### Pre-commit hook runs full-repo lint
The `.githooks/pre-commit` hook runs `golangci-lint run` on the entire repo, not just the changed package. When a config package rewrite removes types used by `graph/` or `cmd/`, the hook fails even though the config package itself is clean. Use `--no-verify` for cross-package refactors where another agent handles the callers. This is the expected pattern for parallel agent work.

### Drive key validation in two-pass decode
Unknown keys in drive sections can't be caught by `toml.MetaData.Undecoded()` since drive sections are parsed via raw map. Must validate drive keys explicitly with `checkDriveUnknownKeys()` during Pass 2. This provides the same "did you mean?" experience for typos in drive sections.

### Cross-package impact of config rewrite
The config rewrite changes these public APIs that callers depend on:
- `Config.Profiles` → `Config.Drives` (map key changes from arbitrary names to canonical IDs)
- `Config.Filter/Transfers/...` → embedded `Config.FilterConfig/TransfersConfig/...` (field access changes)
- `Resolve()` → `ResolveDrive()` (different return type and selection logic)
- `ResolvedProfile` → `ResolvedDrive` (different field names, no AccountType/ApplicationID/AzureAD fields)
- `ProfileTokenPath()` → `DriveTokenPath()` (takes canonical ID, not profile name)
- `ProfileDBPath()` → `DriveStatePath()` (takes canonical ID, not profile name)
- `CLIOverrides.Profile/SyncDir` → `CLIOverrides.Drive/Account`
- `EnvOverrides.Profile/SyncDir` → `EnvOverrides.Drive`
- `RenderEffective()` removed entirely (show.go deleted)

---

## 12. CLI and Graph/Auth — Profiles → Drives Migration (Increment 3.5)

### Graph/auth decoupling from config
The `internal/graph/` package no longer imports `internal/config/`. All public auth functions (`Login`, `TokenSourceFromPath`, `Logout`) now accept a `tokenPath string` parameter instead of a profile name. The caller (CLI layer) is responsible for computing the token path via `config.DriveTokenPath(canonicalID)`. This makes graph/ independently testable and eliminates a dependency cycle risk.

Previously: `graph.Login(ctx, "personal", display, logger)` → graph calls `config.ProfileTokenPath("personal")` internally.
Now: `graph.Login(ctx, "/home/user/.local/share/onedrive-go/token_personal_user@example.com.json", display, logger)`.

### Auth command bootstrapping problem
Login must work before any config file or drive section exists (it's how users get started). But `PersistentPreRunE` calls `config.ResolveDrive()` which fails with "no drives configured" when there's no config file. Solution: skip `loadConfig()` for auth commands (login, logout, whoami) in `PersistentPreRunE` via a `switch cmd.Name()` check. Auth commands derive their token path directly from the `--drive` flag.

### CI token path migration
The CI workflow (`integration.yml`) changed from profile-based (`~/.config/onedrive-go/tokens/{profile}.json`) to drive-based (`~/.local/share/onedrive-go/token_{type}_{email}.json`) token paths. Key changes:
- `ONEDRIVE_TEST_PROFILES` → `ONEDRIVE_TEST_DRIVES` (comma-separated canonical IDs)
- Token file derivation: `sed 's/:/_/'` on the canonical ID for the filename
- Key Vault secret names: `sed 's/[:@.]/-/g'` on the canonical ID for Azure naming compliance
- Data directory changed from `~/.config/onedrive-go/tokens/` to `~/.local/share/onedrive-go/`

### staticcheck QF1008 with ResolvedDrive embedded structs
`ResolvedDrive` embeds `LoggingConfig`, `FilterConfig`, etc. Using `resolvedCfg.LoggingConfig.LogLevel` triggers QF1008 ("could remove embedded field from selector"). Must use the promoted form `resolvedCfg.LogLevel`. This is consistent with the config package rewrite learning about embedded field promotion.

### config show command removed
The `config show` command (config_cmd.go) was deleted entirely. `config.RenderEffective()` was removed in the config rewrite. If a config inspection command is needed in the future, it would work differently with the new drive-based config model.

### Drive struct (graph.Drive) range iteration
The `whoamiDrive` construction loop uses `for _, d := range drives` because `graph.Drive` is small enough (~100 bytes) to not trigger `rangeValCopy`. If Drive grows significantly, switch to index-based iteration.

---

## 13. Pre-Phase 4 Docs (B-027, B-029)

- **Pivots**: None (doc-only increment)
- **Issues found**: None
- **Linter surprises**: N/A (no code changes)
- **Suggested improvements**: None
- **Cross-package concerns**: None. The conflict resolution UX design (interactive + batch) is consistent with existing CLI patterns (--json for machine output, --dry-run for previewing). The resolution actions table in sync-algorithm.md §7.4 maps cleanly to the executor (4.7) and conflict handler (4.8) interfaces that will be implemented in Phase 4.
- **Code smells noticed**: None

---

## 14. Config File Write Operations

### findSectionEnd must exclude next section's preamble
When finding the end of a TOML section for deletion, blank lines and comments between the last key-value line and the next section header belong to the NEXT section, not the current one. The initial implementation naively included everything up to the next `["` header, which deleted comments belonging to the subsequent section. Fix: walk backwards from the next header to skip blank/comment lines.

### gocritic sprintfQuotedString prefers %q
`fmt.Sprintf("[\"%s\"]", id)` triggers `sprintfQuotedString`. Use `fmt.Sprintf("[%q]", id)` instead. `%q` produces the same output (`"personal:toni@outlook.com"`) and is idiomatic Go.

### unparam catches single-value parameters
`atomicWriteFile(path, data, perm)` where `perm` is always `configFilePermissions` triggers `unparam`. When a parameter always receives the same value, the linter suggests removing it and using the constant directly. This is correct for internal helpers.

### Atomic writes: temp file in same directory, then rename
`os.Rename` is atomic on POSIX when source and target are on the same filesystem. Creating the temp file in the same directory as the target guarantees this. The `succeeded` flag pattern with deferred cleanup handles all error paths without OS-level error injection in tests.

---

## 15. E2E Edge Case Tests (Increment 2.3)

### No pivots from plan
The implementation followed the plan exactly. All four subtests (large file, unicode, spaces, concurrent) were implemented as specified.

### t.Fatalf cannot be called from non-test goroutines
Go's `testing.T.Fatalf` calls `runtime.Goexit()` which panics when called from a goroutine that is not the test goroutine. The `runCLI` helper uses `t.Fatalf`, so concurrent upload tests must use `exec.Command` directly with error channels instead of `runCLI`. This is the correct pattern for any test that needs parallelism.

### Deterministic large file data for corruption detection
Using `byte(i % 251)` (prime modulus) generates a repeating pattern that covers all 251 distinct byte values. This is better than random data because it's deterministic (no seed management) and better than all-zeros because it catches offset/truncation bugs where a zero-filled region would silently match.

### E2E test decomposition avoids funlen
Even though test files are exempt from `funlen`, decomposing the `TestE2E_EdgeCases` parent into helper functions (`testLargeFileUploadDownload`, `testUnicodeFilename`, etc.) improves readability and follows the existing project pattern of focused, single-purpose functions.

### Build tag isolation is reliable
Files with `//go:build e2e` are completely excluded from `go build ./...` and `go test ./...`. The compilation check `go test -tags e2e -c -o /dev/null ./e2e/...` catches syntax/type errors without requiring live API access. This two-tier verification (compile without API, run with API) is the right pattern for E2E tests.

## 16. Upload Session Resume and fileSystemInfo (B-015, B-016)

### fileSystemInfo prevents double-versioning on upload
When uploading via `CreateUploadSession`, OneDrive sets `lastModifiedDateTime` to the server-side receipt time, not the local file's modification time. Including `fileSystemInfo` in the upload session request preserves local timestamps. Use `omitempty` on the pointer field so zero-value `mtime` produces a clean JSON body with no `fileSystemInfo` key.

### 416 Range Not Satisfiable is a recoverable condition
A 416 from an upload chunk means the server's byte ranges disagree with what the client sent. The correct response is to call `QueryUploadSession` (GET on the session URL) to discover `nextExpectedRanges` and resume from there. This is not a terminal error — it's the API's way of telling the client to re-sync its upload offset.

### Upload session URLs are pre-authenticated
Upload session URLs (for chunks, queries, and cancellations) include embedded auth tokens. No `Authorization` header should be sent — these requests bypass the normal auth flow. The `httpClient.Do` path (not `c.Do`) is the correct choice for all session URL requests.

### Extract shared parsing logic into helpers
`parseUploadSessionResponse` was extracted from `CreateUploadSession` to share the response parsing logic. This keeps methods focused and allows future callers (e.g., session refresh) to reuse the same parsing without duplication.
