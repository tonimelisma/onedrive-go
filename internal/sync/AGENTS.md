# internal/sync review routing

This file is additive with the repository-root `AGENTS.md`. All root instructions still apply.

For Codex PR review under `internal/sync/`:

- Treat any plausible risk of silent overwrite, dropped delta events, illegal or implicit state transitions, conflict-tracking gaps, big-delete safety regressions, cancellation leaks, or partial-failure masking as `P1`.
- When behavior changes, require alignment with the sync-engine design family in `spec/design/sync-engine.md`, `spec/design/sync-execution.md`, `spec/design/sync-planning.md`, `spec/design/sync-observation.md`, and `spec/design/data-model.md` as applicable to the touched behavior.
- If code changes the state machine, retry routing, baseline durability, deletion safety, observation semantics, or dispatch/execution ordering without matching test updates, raise a `P1`.

