# A19 — The linter's deadline scales with the cores

Written by the coordinator on 2026-11-14. Merged as #904 (`7d7bc06b`), installed 07:00 PST.

## Why the row exists

A whole-repository lint on this 2-core host ran into golangci-lint's fixed 15-minute deadline (exit status 4) whenever another build shared the cores, and the pre-PR gate failed for no fault of the change under it. The two go test tiers already scaled their deadlines with the cores; the linter did not.

## What changed

- `internal/devtool/verify.go`: devtool passes the linter `--timeout`. It is 15 minutes on 3 or more cores and 90 core-minutes divided by the cores below that (45 minutes on 2 cores, 90 on one). The two go test tiers and the linter share one function for the rule. There is no override flag.
- `.golangci.yml`: `run.timeout` stays 15 minutes, and a test holds it equal to devtool's base deadline.
- `.github/workflows/ci.yml`: the lint job's `timeout-minutes` goes from 15 to 20, and a test holds it above the linter deadline plus a 5-minute setup allowance.
- `internal/devtool/gh_retry.go`: gh's own "error connecting to <host>" message counts as a request that never reached GitHub, so the call is repeated. The post-merge stage of #903 failed on exactly that message after its merge had happened.
- Tests in `internal/devtool` (`gh_retry_test.go`, `lint_config_contract_test.go`, `verify_ci_deadline_test.go`, `verify_every_step_test.go`, `verify_quick_test.go`, `verify_runner_test.go`).
- `spec/design/system.md`, `spec/requirements/non-functional.md`, the repository's `CLAUDE.md`.

13 files, +171/-22.

## Checks

Pre-PR gate passed at 06:51 on the commit that was pushed, its whole-repository lint (5 min 27 s) included; PR CI and the post-merge CI on main passed. No live run: the change is in the developer tool and CI only.

## Seen and not fixed

None.
