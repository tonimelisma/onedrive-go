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

### Token bootstrap before CLI login exists
`cmd/integration-bootstrap/main.go` is a thin wrapper around `graph.Login()` for bootstrapping tokens before the real CLI `login` command (1.7). Upload the resulting token to Key Vault manually.

### Integration test build tag pattern
Integration tests use `//go:build integration` and are excluded from `go test ./...`. Run with `go test -tags=integration`. The `newIntegrationClient(t)` helper skips (not fails) when no token is available, so these tests degrade gracefully.

### Graph API returns 400 (not 404) for invalid item ID formats
Requesting `/me/drive/items/nonexistent-string` returns HTTP 400 ("invalidRequest"), not 404. The Graph API validates item ID format before lookup. Use path-based addressing (`/me/drive/root:/nonexistent-path`) to get proper 404 responses for nonexistent items.

### Nightly CI keeps refresh tokens alive
Microsoft rotates refresh tokens on use and they expire after 90 days of inactivity. The nightly schedule (3 AM UTC) ensures tokens stay active.

---

## 6. Tier 1 Research

16 research documents in `docs/tier1-research/` covering Graph API bugs, reference implementation analysis, and tool surveys. Consult these before implementing any API interaction — they contain critical gotchas (upload session resume, delta headers, hash fallbacks, etc.) tracked as B-015 through B-023 in BACKLOG.md.

