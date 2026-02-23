# Coding Conventions

Detailed conventions for the onedrive-go codebase. CLAUDE.md has the essentials; this file has the full reference.

---

## Comment Convention

Comments explain **why**, not **what**. Good: intent, constraints, architectural boundaries, gotcha warnings, external references. Bad: restating code, temporary project state, obvious descriptions.

---

## Logging Standard

All code uses `log/slog` with structured key-value fields. Logging is a first-class concern — not an afterthought. Every function that does I/O, state changes, or non-trivial processing must log enough to debug a CI failure or user bug report without adding instrumentation later.

**Log levels**:
- **Debug**: Every HTTP request/response, token acquisition, file read/write. Off by default for users.
- **Info**: Lifecycle events — login/logout, token load/refresh/save, sync start/complete, config load.
- **Warn**: Degraded but recoverable — retries, expired tokens, failed persistence with fallback.
- **Error**: Terminal failures — request failed after all retries, unrecoverable auth failure.

**Minimum logging per code path**: public function entry with key parameters, every state transition, every error path, every external call (method, URL, status, request-id), every security event (token acquire/refresh/save/delete). Never log token values or secrets (architecture.md §9.2).

**Testing**: Integration tests use a Debug-level `testLogger(t)` writing to `t.Log`, so all activity appears in CI output.

---

## Linter Patterns

Common golangci-lint rules that require specific patterns:

- **mnd**: Every number needs a named constant; tests are exempt
- **funlen**: Max 100 lines / 50 statements — decompose into small helpers
- **depguard**: Update `.golangci.yml` when adding new external dependencies. Check transitive deps too (e.g. Cobra pulls in `mousetrap`)
- **gochecknoinits**: No `init()` functions allowed. Use constructor functions instead (e.g. `newRootCmd()`)
- **gocritic:rangeValCopy**: Use `for i := range items` with `items[i]` instead of `for _, item := range items` when struct > ~128 bytes
- **go.mod pseudo-versions**: Never use placeholder timestamps. Always run `go mod download <module>@<commit>` first to discover the correct timestamp, then construct `v0.0.0-YYYYMMDDHHMMSS-<12-char-hash>`

---

## Test Patterns

- Never pass nil context — runtime panics, not caught by compiler/linter
- Scope test verification to own package: `go test ./internal/graph/...` not `go test ./...`
- See LEARNINGS.md §4 for additional patterns (test vectors, E2E, build tags, coverage)
