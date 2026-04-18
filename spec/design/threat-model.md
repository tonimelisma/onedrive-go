# Threat Model

Implements: R-6.10.7 [verified]

## Verified By

| Behavior | Evidence |
| --- | --- |
| Threat-boundary claims are backed by rooted filesystem, Graph redaction, and repo-consistency enforcement tests. | `internal/fsroot/fsroot_test.go`, `internal/localpath/localpath_test.go`, `internal/graph/client_test.go`, `internal/devtool/verify_repo_checks_test.go` |
| Durable-state and degraded-mode trust assumptions are exercised by sync-store, startup diagnosis, and reset-guidance tests. | `internal/sync/engine_run_once_test.go`, `internal/sync/schema_test.go`, `internal/cli/status_test.go`, `internal/cli/sync_runtime_test.go` |

## Assets

The main assets this codebase must protect are:

- OAuth tokens and token metadata
- pre-authenticated upload/download URLs
- sync-root file contents and metadata
- managed state files (config, state DB, session files, cached metadata)
- structured logs
- SQLite sync state

## Trust Boundaries

The architecture relies on explicit trust boundaries instead of ambient access:

- `graph`: all Microsoft Graph and pre-authenticated HTTP traffic
- `fsroot`: managed-state filesystem writes under repo-controlled roots
- `synctree`: runtime filesystem writes under one configured sync root
- `localpath`: arbitrary user-selected local paths outside rooted managed/sync domains
- CLI/config input: flags, environment, and TOML
- SQLite: durable sync state and failure persistence
- logs: structured operator-visible output that must never leak secrets

## Threat Actors And Non-Goals

Threat actors considered here:

- malformed or hostile remote API payloads
- hostile or corrupted local filesystem state
- accidental operator misconfiguration
- local users or software reading logs and managed-state files
- network failures or hostile intermediaries causing truncation, replay, or malformed responses

Explicit non-goals:

- defending against a fully compromised host OS
- confidentiality against a local attacker who already has read access to the user's files and process memory
- making arbitrary remote content trustworthy; remote content is always treated as untrusted input

## Attack Surfaces

| Surface | Primary Risk | Current Guard |
|--------|--------------|---------------|
| Path reconstruction and rename/delete targets | Path traversal or escape from sync root | `synctree`/`localpath`/`fsroot` rooted boundaries plus path validation |
| Graph payloads | Missing fields, malformed names, unexpected enum/status combinations | Graph normalization, bounded reads, nil/shape validation |
| Token persistence | Secret leakage or silent corruption | `tokenfile` validation plus managed-root writes |
| Pre-auth URLs | Secret leakage or SSRF-like misuse | redaction, host/scheme validation, one raw dispatch boundary |
| Local observation | Symlink escape, invalid names, watch exhaustion | rooted walk/watch helpers, configurable symlink following with cycle guard, inotify limit detection |
| Logs | Token or pre-auth URL disclosure | redaction types, structured logging policy, no secret logging |
| SQLite state | Corruption or split-brain state | WAL mode, single durable authority, transactional writes |
| Resource exhaustion | unbounded response bodies, runaway pagination, too many events | response-body caps, page/depth guards, debounced buffering, bounded workers |
| Privileged subprocess / signal / SQL entrypoints | hidden side effects or widened trust boundaries | verifier-enforced allowlist for `exec.CommandContext`, `signal.Notify`, `sql.Open`, and raw `http.Client.Do` |

## Existing Mitigations

- Explicit filesystem trust boundaries are documented in [system.md](system.md) and enforced in code by `fsroot`, `synctree`, and `localpath`.
- Graph error bodies are size-capped and Graph quirks are normalized at the boundary; see [graph-client.md](graph-client.md).
- Token files are validated on read and write, and managed-state I/O stays rooted; see [graph-client.md](graph-client.md) and [config.md](config.md).
- Local observation filters invalid uploads before they become actions, while remote observation remains server-trusting to avoid silent data loss; see [sync-observation.md](sync-observation.md).
- Durable state is transactional and rebuildable; see [sync-store.md](sync-store.md) and [data-model.md](data-model.md).
- Retry, scope blocking, and graceful degradation are explicit runtime behaviors instead of ad hoc loops; see [retry.md](retry.md), [sync-engine.md](sync-engine.md), and [degraded-mode.md](degraded-mode.md).

## Mitigation Evidence

| Mitigation | Evidence |
|------------|----------|
| Rooted filesystem boundaries and atomic replacement writes | `internal/fsroot/fsroot_test.go` (`TestRoot_AtomicWrite_WritesFileAtomically`, `TestRoot_AtomicWrite_RejectsRootEscape`), `internal/localpath/localpath_test.go` (`TestAtomicWrite`, `TestAtomicWrite_CleansTempOnRenameFailure`) |
| Graph normalization, redaction, and pre-auth boundary discipline | `internal/graph/client_test.go` (`TestDo_DebugLogsNeverExposeBearerToken`, `TestDoPreAuth_ErrorBodyCappedAt64KiB`, `TestDoOnce_401_RefreshSucceeds`), `internal/devtool/verify_repo_checks_test.go` (`TestRunRepoConsistencyChecksFailsOnHTTPClientDoOutsideApprovedBoundary`) |
| Managed token/config file validation and rooted writes | `internal/graph/auth_test.go` (`TestSaveToken_AtomicWrite`, `TestLoadToken_InvalidJSON`), `internal/config/write_test.go` (`TestAtomicWriteFile_WritesFile`) |
| Observer-side invalid-path and permission containment | `internal/sync/permission_handler_test.go` (`TestPermHandler_HandleLocalPermission_DirectoryLevel`, `TestPermHandler_Handle403_NoPermissionRoot`), `internal/sync/engine_watch_test.go` (`TestRunWatch_AllObserversDead_ReturnsError`) |
| Durable state authority and crash recovery | `internal/sync/engine_run_once_test.go` (`TestNewEngine_RequiresResetForNonSQLiteStateDB`, `TestNewEngine_RequiresResetForIncompatibleSchemaStateDB`, `TestNewEngine_RequiresResetForUnsupportedLegacyPersistedState`, `TestRunOnce_ReconcilesRemoteMirrorDownloadDriftWithoutFreshDelta`, `TestRunOnce_ReconcilesRemoteDeleteDriftWithoutFreshDelta`), `internal/sync/engine_phase0_test.go` (`TestBootstrapSync_ReconcilesRemoteDeleteDriftWithoutFreshDelta`), `internal/cli/drive_reset_sync_state_test.go` (`TestRunDriveResetSyncStateWithInput_ResetsAndRecreatesStateDB`) |
| Privileged API boundaries kept narrow by repo verification | `internal/devtool/verify_repo_checks_test.go` (`TestRunRepoConsistencyChecksFailsOnExecCommandContextOutsideApprovedBoundary`, `TestRunRepoConsistencyChecksFailsOnSQLOpenOutsideApprovedBoundary`, `TestRunRepoConsistencyChecksFailsOnSignalNotifyOutsideApprovedBoundary`) |

## Residual Risks And Follow-Ups

- Resource-consumption guarantees per component remain a planned follow-up in [system.md](system.md).
- Full lock-ordering documentation is still incomplete; the current work documents ownership, not a finished global lock hierarchy.
- Secret-leak auditing remains ongoing for all log and error-message surfaces.
