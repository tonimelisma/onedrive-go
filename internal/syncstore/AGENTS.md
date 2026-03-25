# internal/syncstore review routing

This file is additive with the repository-root `AGENTS.md`. All root instructions still apply.

For Codex PR review under `internal/syncstore/`:

- Treat any plausible non-atomic write, transactional gap, migration safety issue, WAL or close durability bug, retry-queue corruption, or unrecoverable state transition as `P1`.
- When behavior changes, require alignment with `spec/design/sync-store.md` and `spec/design/data-model.md`.
- If code changes migrations, commit paths, failure persistence, baseline durability, or admin recovery behavior without matching tests, raise a `P1`.

