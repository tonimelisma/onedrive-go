# internal/graph review routing

This file is additive with the repository-root `AGENTS.md`. All root instructions still apply.

For Codex PR review under `internal/graph/`:

- Treat any plausible pagination gap, timeout omission, nil or enum trust bug, retry of a non-idempotent operation without protection, streaming or hash validation regression, or documented Graph quirk violation as `P1`.
- When behavior changes, require alignment with `spec/design/graph-client.md` and `spec/reference/graph-api-quirks.md`.
- If code changes request classification, paging, download or upload streaming, response normalization, shared-item enrichment, or auth handling without matching tests, raise a `P1`.

