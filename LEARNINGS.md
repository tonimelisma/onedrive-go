# Learnings — Institutional Knowledge Base

Knowledge captured during implementation. Organized by topic. Historical learnings from the batch-pipeline sync engine (Phases 4.1-4.11) archived in `docs/archive/learnings-v1.md`.

---

## 1. API and Graph Client

### Test-friendly time delays
Any code with `time.Sleep` or timer-based backoff must use an injectable sleep function:
```go
type Client struct {
    sleepFunc func(ctx context.Context, d time.Duration) error // default: timeSleep
}
```
Tests override with `noopSleep` that returns immediately. Without this, retry tests took 70s instead of 1.4s.

### govet shadow checks are strict
This project enables `govet` with `enable-all: true`. Variable shadowing in nested scopes (e.g., `if err := ...` inside a block that already has `err`) triggers lint failures. Use distinct names like `sleepErr`, `readErr`.

### httptest is the right choice for Graph API tests
Use `httptest.NewServer` for all Graph client tests. Real HTTP, no interfaces for mocking. Tests are realistic and simple.

### oauth2 fork for OnTokenChange
`golang.org/x/oauth2` has no persistence callback — when `ReuseTokenSource` silently refreshes a token, the new refresh token is only in memory. We use `github.com/tonimelisma/oauth2` fork (branch `on-token-change`) via `go.mod` replace directive. Adds `Config.OnTokenChange func(newToken *Token)` — fires after refresh, outside mutex, nil-safe. Tracks upstream proposal `golang/go#77502`.

### Public auth functions accept explicit token paths
Public auth functions (`Login`, `Logout`, `TokenSourceFromPath`) accept explicit `tokenPath` parameters — the caller computes the path via `config.DriveTokenPath(canonicalID)`. This decouples `graph/` from `config/` entirely.

### oauth2 device code tests use real polling delays
Tests using `cfg.DeviceAccessToken()` incur real 1-second polling intervals (the minimum per RFC 8628). Set `"interval": 1` in mock device code responses to minimize delay, but tests still take ~1-3s each.

### go.mod replace directive pseudo-version format
When using `replace` with a commit hash, the pseudo-version timestamp must match the commit's actual timestamp. Use `go mod download <module>@<commit>` first to discover the correct timestamp.

### Graph API quirks
- **Personal accounts have empty mail field.** Use `userPrincipalName` as fallback.
- **Returns 400 (not 404) for invalid item ID formats.** Use path-based addressing for proper 404.
- **JSON tag `@odata.nextLink` / `@microsoft.graph.conflictBehavior`** trigger `tagliatelle` linter — suppress with `//nolint:tagliatelle`.
- **Delta token is always a full URL.** Use `stripBaseURL()` to convert to a relative path for `Do()`.

### Pre-authenticated URLs bypass the Graph API
`@microsoft.graph.downloadUrl` and `uploadUrl` from CreateUploadSession are pre-authenticated URLs. Must NOT use `Do()` (no base URL prefix, no auth headers). Use `httpClient.Do(req)` directly. Never log these URLs.

### SimpleUpload needs custom content type
`Do()` always sets `Content-Type: application/json`. SimpleUpload needs `application/octet-stream`. Solution: a private `doRawUpload` helper. No retry for upload operations (can't safely replay a partially-consumed reader).

### Upload chunk responses have three shapes
- 202 Accepted: intermediate chunk (drain and discard body)
- 200/201: final chunk complete (body has driveItem JSON)
- Error: various HTTP error codes

### rewindBody is called before every attempt, not just retries
`doRetry` calls `rewindBody` at the top of the loop, which means the first attempt also rewinds. Testing "rewind fails on retry" requires a seeker that succeeds on the first Seek but fails on subsequent ones -- a simple always-failing seeker will fail before any HTTP call is made.

### Delta normalization pipeline
Four steps in order: (1) filter packages, (2) clear bogus hashes on deleted items, (3) deduplicate keeping last occurrence, (4) reorder deletions before creations. Each step is a separate unexported function. `slices.SortStableFunc` preserves relative order.

### URL encoding in Graph API paths
Path segments must be individually URL-encoded. `encodePathSegments()` splits on `/`, encodes each with `url.PathEscape`, and reassembles. In httptest, use `r.RequestURI` (not `r.URL.RawPath`) to verify encoding.

### Retry body consumption bug
The `doRetry` loop reused an `io.Reader` body across retries. Fix: seek the body to offset 0 before each attempt. The `rewindBody` helper was extracted to keep `doRetry` under complexity limits.

### JSON dot-notation tags never work in encoding/json
`json:"file.mimeType"` does not support dot notation — the field was never populated. Dead fields with misleading tags are a maintenance hazard.

### Upload session resume and fileSystemInfo
- `fileSystemInfo` in upload session requests preserves local timestamps (prevents double-versioning).
- 416 Range Not Satisfiable is recoverable — call `QueryUploadSession` to discover `nextExpectedRanges`.
- Upload session URLs are pre-authenticated (no `Authorization` header).

### httptest closure variable forward-reference
When an `httptest.NewServer` handler needs `srv.URL`, declare `var srv *httptest.Server` first, then assign. Direct assignment with a closure referencing `srv.URL` won't compile.

### Package-level var for test-overridable guards
`maxDeltaPages` uses `var` instead of `const` with `//nolint:gochecknoglobals` to allow test overrides. Tests save/restore the original value with `defer`.

### gosec G602 false positive on backwards iteration
`gosec` flags `items[i]` as "slice index out of range" when iterating backwards. Work around by copying, reversing with `slices.Reverse()`, and iterating forward.

---

## 2. Filesystem and Platform

### filepath.Ext(".bashrc") returns ".bashrc", not ""
Go's `filepath.Ext` returns the suffix from the LAST dot in the last path element. For dotfiles like `.bashrc`, the only dot is at position 0, so the entire name (`.bashrc`) is treated as the extension and the stem is empty. This causes conflict copy naming to produce `.conflict-TIMESTAMP.bashrc` instead of `.bashrc.conflict-TIMESTAMP`. Fix: detect when `base[0] == '.'` and `strings.Count(base, ".") == 1`, then treat extension as empty and use the full name as stem. Extract a `conflictStemExt` helper for this logic.

### NFC normalization requires separate filesystem and DB paths
On macOS (APFS), NFC and NFD lookups resolve to the same file. On Linux (ext4), filenames are stored as exact bytes. Thread two separate paths: `fsRelPath` (original bytes for I/O) and `dbRelPath` (NFC-normalized for database). Track visited DB paths during walks to avoid false positive orphan detection.

### DirEntry.Info() can fail independently of ReadDir
`os.ReadDir` returns `DirEntry` values that defer their `Info()` call. Handle as skip-and-warn rather than fatal error.

### os.ReadDir vs filepath.Walk for scanner control
Manual `os.ReadDir` + recursion gives full control over entry ordering, filter short-circuiting, and error handling per-directory. Enables skipping entire subtrees without walking into them.

### Deterministic large file data for corruption detection
`byte(i % 251)` (prime modulus) generates a repeating pattern covering 251 distinct byte values. Better than random (deterministic) and better than zeros (catches offset/truncation bugs).

### Platform-specific build tags for disk space
Darwin uses `syscall.Statfs` directly; Linux needs `golang.org/x/sys/unix.Statfs`. Both use `Bavail` (available to unprivileged users), NOT `Bfree`.

### APFS prevents creating filenames with invalid UTF-8
macOS APFS normalizes filenames and rejects invalid byte sequences. To test UTF-8 validation guards in scanner's `validateEntry`, use mock `os.DirEntry`/`os.FileInfo` types rather than real filesystem operations.

---

## 3. Linter Patterns

### mnd (magic number detector)
Every number needs a named constant; tests are exempt. The `.golangci.yml` `ignored-numbers` list includes `'2'`, so `if len(parts) < 2` doesn't need `//nolint:mnd`. The `nolintlint` linter catches unnecessary directives.

### funlen (100 lines / 50 statements)
Decompose into small helpers. Near-limit functions require extracting helpers for any future additions rather than adding code inline.

### dupl (duplicate detection)
Catches near-identical method pairs and structural patterns. Solutions: extract shared helpers parameterized by the varying part.

### gocritic
- **rangeValCopy**: Use `for i := range items` with `items[i]` when struct > ~128 bytes. `graph.Item` is 264 bytes, `Drive` is 144 bytes, `FilterConfig` is 112 bytes.
- **hugeParam**: `toml.MetaData` (96 bytes), `SafetyConfig` (88 bytes) — pass by pointer.
- **emptyStringTest**: Prefers `name != ""` over `len(name) > 0`.
- **sprintfQuotedString**: Use `%q` instead of `"\"%s\""`.

### staticcheck QF1008 (embedded field selectors)
With embedded structs, `cfg.FilterConfig.SkipDotfiles` and `cfg.SkipDotfiles` are equivalent. Use the short (promoted) form everywhere.

### unparam (unused parameters)
When a parameter always receives the same value, the linter suggests removing it. Correct for internal helpers.

### gocyclo (cyclomatic complexity limit 15)
Strict for decision matrices. Strategy: extract classification logic into a pure function returning an enum/struct, then dispatch with a simple switch.

### gosec G101 (potential hardcoded credentials)
SQL variable named `sqlGetDeltaToken` triggers false positive. Fix with `//nolint:gosec`.

### gosec G115 (integer overflow)
`uint64` to `int64` — cap at `math.MaxInt64` before conversion.

### nilnil return pattern
Returning `(nil, nil)` for "skip this entry" requires `//nolint:nilnil`. Idiomatic when both nil means "nothing to do, no error."

### errcheck is disabled in test files
`.golangci.yml` excludes `errcheck` from `_test.go` files. Do NOT add `//nolint:errcheck` in tests — the `nolintlint` linter will reject it as an unnecessary directive.

### goconst applies to test files and counts cross-file
Unlike `mnd`, `funlen`, `dupl` -- `goconst` flags repeated string literals even in tests. It counts across all files in a package, so adding `"file.txt"` in one test file can trigger goconst for an existing occurrence in another. Use unique filenames in each test file.

### handleChunkResponse can be tested with crafted *http.Response
For error paths that don't depend on the HTTP transport, construct an `*http.Response` directly with a custom `Body` instead of using httptest.

### gofumpt stricter than gofmt
Enforces stricter struct field alignment. Multi-byte characters in comments can cause differences. Always run `gofumpt -w` before committing.

---

## 4. Testing Patterns

### QuickXorHash test vectors
Plan test vectors were wrong. Always verify against a known-good reference (rclone v1.73.1), don't blindly trust specs.

### Scoped test verification
```bash
# Good: scoped to own package
go test ./internal/graph/...
# Bad: sees intermediate states from other agents
go test ./...
```

### E2E test pattern: build once, run as subprocess
Build the binary in `TestMain` to a temp dir, run via `os/exec` in each test. Use `t.Cleanup` for teardown.

### t.Fatalf cannot be called from non-test goroutines
`testing.T.Fatalf` calls `runtime.Goexit()` which panics from a non-test goroutine. Use `exec.Command` directly with error channels for concurrent tests.

### Build tag isolation
Files with `//go:build e2e` are completely excluded from `go build ./...`. Two-tier verification: compile without API, run with API.

### Always check coverage before committing
Run `go test -coverprofile` as part of DOD check, not just build+test+lint.

### E2E test decomposition
Even though test files are exempt from `funlen`, decompose into helper functions for readability.

### Recursive mkdir with 409 Conflict handling
Walk path segments, create each. If CreateFolder returns 409, resolve by path and continue with its ID as parent.

---

## 5. Config and TOML

### Chunk size default "10MiB" not "10MB"
10 MB (10,000,000 bytes) is NOT a multiple of 320 KiB. 10 MiB (10,485,760) IS a multiple (32 * 327,680).

### `toml.MetaData` is 96 bytes
Triggers hugeParam. Pass by pointer. `toml.DecodeFile` returns it by value, so take its address.

### misspell linter catches intentional typos in test TOML
Use `//nolint:misspell` on lines with deliberate misspellings for unknown-key-detection tests.

### BurntSushi/toml embedded struct field promotion
Works without tags. No `toml:",squash"` needed (that's mapstructure). Just embed directly.

### Two-pass TOML decode for mixed-key configs
Pass 1 decodes globals into embedded structs. Pass 2 decodes into `map[string]any`, extracts keys containing `:` as drive sections, converts via re-encode/decode through `mapToDrive()`.

### Drive key validation in two-pass decode
Unknown keys in drive sections can't be caught by `toml.MetaData.Undecoded()`. Must validate explicitly with `checkDriveUnknownKeys()`.

### errWriter pattern for multi-write formatting
Wrap `io.Writer`, capture first error, subsequent writes are no-ops. One `failWriter` test covers all error paths.

### cmd.Flags().Changed() for pflag default disambiguation
Use `cmd.Flags().Changed("flag")` to detect whether the user actually passed the flag vs. relying on default value.

### CLIOverrides pointer fields for nil-vs-zero-value
`*string` / `*bool` — `nil` means "not specified", `&false` means "user explicitly set to false".

### Synthetic default drive for zero-config UX
When no config file exists, `ResolveDrive()` creates a synthetic default drive with `SyncDir: "~/OneDrive"`.

### findSectionEnd must exclude next section's preamble
Blank lines and comments between the last key-value line and the next section header belong to the NEXT section. Walk backwards from the next header to skip them.

### Atomic writes: temp file in same directory, then rename
`os.Rename` is atomic on POSIX when source and target are on the same filesystem. `succeeded` flag with deferred cleanup handles all error paths.

### Cross-package impact of config rewrite
Key API changes: `Config.Profiles` -> `Config.Drives`, `Resolve()` -> `ResolveDrive()`, `ProfileTokenPath()` -> `DriveTokenPath()`, `ProfileDBPath()` -> `DriveStatePath()`. `config show` command removed entirely.

---

## 6. CI and Integration

### Azure OIDC + Key Vault for CI token management
GitHub secrets can't be updated from workflows, so we use Azure Key Vault. OIDC federation means no stored credentials — GitHub Actions presents a short-lived JWT scoped to `repo:tonimelisma/onedrive-go:ref:refs/heads/main`. Token files flow via `az keyvault secret download/set --file`, never through stdout/CI logs.

### Token and drive ID bootstrap
Tokens bootstrapped via `go run . login --drive personal:user@example.com`. Drive IDs discovered via `go run . whoami --json`. Integration tests require `ONEDRIVE_TEST_DRIVE_ID` env var.

### Integration test build tag pattern
`//go:build integration` excluded from `go test ./...`. The `newIntegrationClient(t)` helper skips (not fails) when no token is available.

### Nightly CI keeps refresh tokens alive
Microsoft rotates refresh tokens on use and they expire after 90 days of inactivity. Nightly schedule (3 AM UTC) keeps them active.

### Orchestrator manages Key Vault secrets directly
The AI orchestrator has `az` CLI access for creating/renaming secrets, downloading/uploading tokens, setting GitHub variables. The human only handles one-time Azure infrastructure and interactive browser-based flows.

### Local CI validation prevents push-and-pray
Mirror the workflow's token loading logic with `az keyvault secret download`, test `whoami --json --drive`, run E2E tests locally. See test-strategy.md §6.1.

### CI token path migration (profiles -> drives)
`ONEDRIVE_TEST_PROFILES` -> `ONEDRIVE_TEST_DRIVES`. Token file: `sed 's/:/_/'`. Key Vault secret: `sed 's/[:@.]/-/g'`. Data dir: `~/.config/onedrive-go/tokens/` -> `~/.local/share/onedrive-go/`.

### Auth command bootstrapping problem
Login must work before any config file exists. `PersistentPreRunE` skips `loadConfig()` for auth commands via `switch cmd.Name()`. Auth commands derive token path from `--drive` flag.

### OAuth app ID must be consistent across source, tokens, and Key Vault
Refresh tokens are bound to the OAuth client ID that obtained them. If the Azure AD app registration is deleted and re-created (new client ID), all three must be updated in lockstep: (1) `defaultClientID` in source code, (2) token files on disk (re-login required), (3) tokens in Key Vault.

### Token discovery enables zero-config CLI experience
`DiscoverTokens()` scans the data dir for `token_*.json` files and extracts canonical IDs from filenames. `matchDrive()` falls back to this when no config file exists and no `--drive` flag is given. One token -> auto-select. Multiple -> prompt with list.

---

## 7. Event-Driven Sync Engine (Option E)

### graph.Item is 264 bytes — always pass by pointer
The `gocritic` `hugeParam` lint catches this (already documented in §3 under `rangeValCopy`), but it's easy to forget when writing new code that accepts `graph.Item` as a parameter. In 4v2.2, all three methods (`processItem`, `classifyAndConvert`, `materializePath`) and standalone helpers (`classifyItemType`, `selectHash`, `resolveParentDriveID`) were initially written with value parameters and had to be refactored to pointer. Check LEARNINGS §3 gocritic section before writing any function that takes graph types.

### Run gofumpt before first lint check
Running `golangci-lint run` before `gofumpt -w` wastes a cycle fixing formatting issues that gofumpt would have caught. Always format before linting.

### DOD gates are not optional — complete ALL 15 before declaring done
In 4v2.3, gates 7 (logging review), 8 (comment review), 9 (docs update), 14 (retrospective), and 15 (re-envisioning) were initially skipped. The logging review caught two missing log lines (nosync guard Warn, deletion detection Debug summary) and the comment review caught a misleading "order matters" comment. These are exactly the kind of issues the gates exist to catch. Never shortcut the DOD checklist — the mechanical gates (build/test/lint) are necessary but not sufficient.

### alwaysExcludedSuffixes order: `.db-wal`/`.db-shm` before `.db`
The `strings.HasSuffix` loop checks suffixes in declaration order. `.db-wal` and `.db-shm` must appear before `.db` in the slice, otherwise `.db` would match first and the longer suffixes would never be reached. This is a correctness constraint, not just style.

### Planner decision matrix: localDeleted implies localChanged
When splitting the EF1-EF10 file decision matrix into switch cases, `localDeleted` (baseline exists but Local is nil) always implies `localChanged` (detectLocalChange returns true when Local is nil). This means a catch-all case like `localChanged && remoteChanged && hasRemote` will match both EF5 (edit-edit conflict, local present) and EF7 (local deleted, remote modified). The fix: add `!localDeleted` to EF4/EF5/EF9 cases, or evaluate `localDeleted` cases first. This is a subtle ordering bug that manifests as conflicts instead of downloads.

### Spec pseudocode ordering != implementation ordering
The sync-algorithm.md §7.3 pseudocode had EF3 (`localChanged && !remoteChanged`) before EF6 (`localDeleted && !remoteChanged`), but `localDeleted` implies `localChanged` (detectLocalChange returns true when Local is nil). In Go's `switch`, the first matching case wins, so EF3 would steal EF6's matches. The implementation correctly checks `localDeleted` cases first via sub-functions. Always update the spec when the implementation deviates — spec and code must agree on case ordering.

### RemoteState must carry DriveID for cross-drive correctness
Shared folder items from Drive A in Drive B's delta carry Drive A's DriveID. If RemoteState omits DriveID, new cross-drive items get empty DriveID in Actions, breaking executor API calls (404 or wrong-item operations). DriveID and ItemID are the two halves of Graph API item identity (`/drives/{driveID}/items/{itemID}`). The planner's DriveID propagation: Remote.DriveID wins (cross-drive), Baseline.DriveID is fallback (no remote observation), empty for new local items (executor fills from context).

### Worktree branch point matters for parallel agents
When a worktree is created from `main` via the EnterWorktree tool but the active development branch is `clean-slate`, the worktree will have old code (e.g., the deleted batch-pipeline sync engine). The fix is `git reset --hard origin/clean-slate` after worktree creation. Save any new files to /tmp first, reset, then restore. This cost one debugging cycle in 4v2.5.

### Move detection must preserve views for reused paths
When a remote move A→B happens and a new item appears at A in the same delta, `detectRemoteMoves` must not delete A from views. If the old path has a new item (Remote.IsDeleted=false from a ChangeCreate after the synthetic delete), clear Baseline and Local instead of deleting — so the new item classifies correctly (EF14 for files, ED3 for folders) rather than conflicting against the moved item's stale baseline.

### Delete ordering: files before folders at same depth
Depth-first ordering (deepest first for deletes, shallowest first for creates) is necessary but not sufficient. At the same depth level, files must be deleted before folders. A folder at depth N cannot be deleted until sibling files at depth N are removed first. Use `resolveItemType` as a tiebreaker in the sort comparator: `ItemTypeFile(0) < ItemTypeFolder(1)`.

### Split switch statements to keep gocyclo under 15
A Go `switch` with 10 case arms easily exceeds gocyclo's complexity threshold of 15. The pattern: split by a discriminating boolean (e.g., `localDeleted` vs not, `hasBaseline` vs not) into two smaller functions. Each sub-function handles half the cases with much lower complexity. Used successfully for both `classifyFileWithFlags` (28→split into 3 functions) and `classifyFolder` (32→split into 3 functions).

### mtime+size fast path is the industry standard for change detection
No production sync tool (rsync, rclone, Syncthing, abraunegg/onedrive, Unison, Git) hashes every file on every scan. They all use mtime+size as a fast path and only hash when metadata differs. For a 50K-file sync root, always-hashing reads ~5 GB per cycle vs sub-second stat-only checks. The racily-clean guard (force hash when mtime is within 1 second of scan start) handles the edge case where a file was modified in the same clock tick as the last sync — Git's well-documented "racily clean" problem.

---

## 8. Agent Coordination

### Scoped verification prevents cross-agent interference
Four agents ran simultaneously without conflicts (true leaf packages). Scope tests to own package.

### Test symbol collisions between same-package agents
Multiple agents in the same package can redeclare test helpers. Fix: (1) list existing test symbols in prompts, (2) assign unique prefixes, (3) reuse shared helpers.

### Plan merge order to minimize rebase churn
The last-to-merge PR bears all conflict burden. Merge agents defining shared infrastructure first.

### Export shared utilities in Wave 0
Agents working in parallel may need the same utility. Export shared utilities before launching agents.

### Plan NFC/NFD normalization upfront
Dual-path NFC should be specified from the start for any code touching filesystem paths AND a database.

### Parallel agent file conflicts
`git checkout` destroys untracked files. Files must be staged immediately after creation. Use worktrees for isolation.

### Agents must commit LEARNINGS.md updates
Agents modifying LEARNINGS.md must include it in their commits. Explicit checklist item in quality gates.

### Agent subagent_type must be `general-purpose` for code changes
`subagent_type: "Bash"` only has the Bash tool. Always use `general-purpose` for agents that need to read, edit, and write files.

### cmd.CommandPath() is safer than cmd.Name() for skip lists
`cmd.Name()` returns just the leaf name (e.g., `"add"`), risking collisions. `cmd.CommandPath()` returns full path (e.g., `"onedrive-go drive add"`).

### CLI output conventions
Status/error messages to stderr. Structured data (JSON, tables) to stdout. Allows piping.

### gochecknoinits forbids init() functions
Use constructor functions instead: `newRootCmd()`. Testable, no package-level mutable state.

### Cobra transitive dependency: mousetrap
Must be added to depguard allow list alongside cobra and pflag. Always check transitive deps.
