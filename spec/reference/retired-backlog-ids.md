# Retired Backlog IDs

`B-NNN` identifiers appear in code comments throughout this repository. They
are **historical provenance markers** from a backlog document (`BACKLOG.md`)
that was dissolved when planned work moved into `spec/` as `[planned]` items.

The backlog itself no longer exists. These IDs are retained deliberately, and
only as historical labels: they let you recover the original discussion by
searching git history (`git log -S B-207`). They are **not** live references,
and nothing in `spec/` resolves them.

## Rules

- Do not add new `B-NNN` identifiers to code. New work is tracked in `spec/`.
- A comment must carry its own meaning. Never write a comment whose only
  content is a pointer to one of these IDs.
- Every `B-NNN` cited in Go code must be listed below. `go run ./cmd/devtool
  verify default` enforces this, so a newly introduced ID fails the build.

## Registry

| ID | Cited in |
| --- | --- |
| `B-007` | `internal/sync/observer_remote_test.go` |
| `B-021` | `internal/driveops/hash.go`, `internal/driveops/transfer_manager.go`, `internal/driveops/transfer_manager_test.go`, +3 more |
| `B-036` | `internal/cli/status_test.go` |
| `B-068` | `internal/sync/executor.go` |
| `B-074` | `internal/sync/engine.go` |
| `B-076` | `internal/sync/executor_test.go` |
| `B-081` | `internal/sync/executor_test.go` |
| `B-085` | `internal/driveops/interfaces.go`, `internal/graph/download.go` |
| `B-090` | `internal/sync/worker_test.go` |
| `B-099` | `internal/sync/observer_local.go` |
| `B-100` | `internal/sync/observer_local_create_test.go` |
| `B-101` | `internal/sync/observer_local_handlers.go` |
| `B-102` | `internal/sync/observer_local_create_test.go`, `internal/sync/observer_local_test.go`, `internal/sync/observer_local_write_test.go` |
| `B-106` | `internal/sync/executor_test.go` |
| `B-107` | `internal/sync/observer_local.go`, `internal/sync/observer_local_delete_test.go`, `internal/sync/observer_local_handlers.go`, +1 more |
| `B-108` | `internal/sync/observer_local_handlers_test.go` |
| `B-112` | `internal/sync/observer_local_delete_test.go`, `internal/sync/observer_local_handlers.go` |
| `B-113` | `internal/sync/observer_local_handlers.go`, `internal/sync/observer_local_test.go` |
| `B-114` | `internal/sync/observer_local.go` |
| `B-116` | `internal/sync/observer_local_handlers.go` |
| `B-117` | `internal/sync/observer_local_handlers_test.go` |
| `B-118` | `internal/sync/observer_local_handlers_test.go` |
| `B-119` | `internal/sync/errors.go`, `internal/sync/scanner.go` |
| `B-125` | `internal/sync/observer_local.go`, `internal/sync/observer_remote.go`, `internal/sync/observer_remote_test.go` |
| `B-127` | `internal/sync/observer_remote.go`, `internal/sync/observer_remote_test.go` |
| `B-132` | `internal/sync/executor_test.go`, `internal/sync/executor_transfer.go` |
| `B-146` | `internal/graph/auth_browser.go` |
| `B-154` | `internal/sync/planner.go` |
| `B-156` | `internal/cli/rm.go` |
| `B-158` | `internal/graph/types.go`, `internal/graph/types_test.go` |
| `B-159` | `internal/graph/client_auth.go` |
| `B-189` | `internal/sync/observer_local_handlers_test.go` |
| `B-190` | `internal/sync/observer_local_trysend_test.go` |
| `B-198` | `internal/sync/baseline_test.go`, `internal/sync/store_write_baseline.go` |
| `B-203` | `internal/sync/observer_local_handlers.go`, `internal/sync/scanner.go` |
| `B-207` | `internal/driveops/stale_partials.go`, `internal/driveops/transfer_manager.go` |
| `B-208` | `internal/driveops/transfer_manager.go`, `internal/driveops/transfer_manager_test.go` |
| `B-211` | `internal/driveops/transfer_manager.go`, `internal/driveops/transfer_manager_test.go` |
| `B-214` | `internal/driveops/transfer_manager_test.go` |
| `B-215` | `internal/driveops/transfer_manager_test.go` |
| `B-216` | `internal/driveops/transfer_manager_test.go` |
| `B-217` | `internal/driveops/transfer_manager_test.go` |
| `B-221` | `internal/driveops/transfer_manager.go` |
| `B-222` | `internal/driveops/transfer_manager.go`, `internal/sync/executor.go` |
| `B-232` | `internal/cli/root_test.go` |
| `B-271` | `internal/graph/types.go`, `internal/sync/item_converter.go`, `internal/sync/observer_remote_test.go` |
| `B-272` | `internal/driveid/shared_test.go` |
| `B-273` | `internal/driveid/canonical_test.go`, `internal/driveid/edge_test.go`, `internal/driveid/shared_test.go` |
| `B-279` | `internal/graph/types.go` |
| `B-280` | `internal/graph/drives.go`, `internal/graph/types.go` |
| `B-281` | `internal/sync/item_converter.go`, `internal/sync/observer_remote.go`, `internal/sync/observer_remote_test.go` |
| `B-282` | `internal/sync/observer_remote.go`, `internal/sync/observer_remote_test.go` |
| `B-283` | `internal/graph/drives_test.go` |
| `B-284` | `internal/config/toml_lines_test.go`, `internal/config/write_test.go` |
| `B-287` | `internal/config/validate_drive.go` |
| `B-296` | `internal/cli/root_test.go` |
| `B-300` | `internal/driveops/session_store.go`, `internal/driveops/session_store_test.go` |
| `B-310` | `internal/sync/observer_local_create_test.go` |
| `B-312` | `internal/sync/executor_test.go`, `internal/sync/observer_local_handlers.go` |
| `B-313` | `internal/sync/errors.go`, `internal/sync/planner.go` |
| `B-314` | `internal/graph/client.go`, `internal/graph/client_test.go` |
| `B-315` | `internal/graph/types.go`, `internal/graph/types_test.go` |
| `B-318` | `internal/sync/fault_injection_test.go` |
| `B-323` | `internal/sync/executor_test.go` |
