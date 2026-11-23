# A18 — The short tier's go test deadline scales with the cores

Written by the coordinator on 2026-11-14. Merged as #902 (`83fe1f82`), installed 05:00 PST.

## Why the row exists

A pre-PR gate on this 2-core host ran into the short tier's fixed 10-minute go test deadline while another build was running. The full tier had scaled its deadline with the cores since #888; the short tier had not.

## What changed

- `internal/devtool/verify.go`, `verify_quick.go`: the short tier's go test deadline is 10 minutes on 3 or more cores and 60 core-minutes divided by the cores below that. `--short-unit-timeout` overrides it.
- `cmd/devtool/main.go`: the flag.
- Tests in `internal/devtool/verify_quick_test.go`, `verify_ci_deadline_test.go` and `cmd/devtool/main_test.go`; the CI-deadline test covers the new deadline against the job timeouts.
- `spec/design/system.md`, `spec/requirements/non-functional.md`, the repository's `CLAUDE.md`.

9 files, +178/-44.

## Checks

Pre-PR gate passed at 04:48 on the commit that was pushed; PR CI and the post-merge CI on main passed. No live run: the change is in the developer tool only.

## Seen and not fixed

None.
