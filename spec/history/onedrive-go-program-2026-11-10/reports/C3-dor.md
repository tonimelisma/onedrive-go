# C3 definition of ready (R3b, on refactor/observation-leftovers at c00095a5): STOPPED

Not built. Two findings meet the stop conditions; no product code was edited.

## Stop 1 (brief (v), and the decision itself): blocked rows without a block_scopes row are written on purpose
- Read-permission scopes are not persisted in `block_scopes`: `scopeDescriptor.PersistsInBlockScopes` (scope_semantics.go) is false for `scopeFamilyPermDir`/`scopeFamilyPermRemote` with read access ("Read-denied subtree boundaries remain observation-owned facts carried on observation_issues via ScopeKey").
- Yet work is recorded blocked under such a scope: engine_runtime_permissions.go, the `permissionEvidenceBoundaryDenied` arms of both the completion path and the trial path, call `recordPermissionBlockedRetry(ctx, work, scopeKey)` (store `RecordBlockedRetryWork`, blocked = 1, scope_key = the read scope) with `scopeKeyForPermissionEvidence` mapping `issueLocalReadDenied` to `sKPermLocalRead(...)`; `applyBlockScope` follows only when `scopeKey.PersistsInBlockScopes()`, so for a local read denial no scope row is ever written. The trial path's `permissionEvidenceKnownActiveBoundary` arm does the same with the evidence's or the trial's scope.
- The start-up repair `ReleaseOrphanBlockedRetryWork` (store_retry_work.go) releases exactly these rows ("every blocked row whose scope has no block_scopes row").
- With the decided key (`blocked_scope_key` non-NULL exactly when blocked, referencing `block_scopes`), every such write is refused. Either the CHECK/key changes (blocked rows under read scopes carry no key) or the code changes (read scopes get block_scopes rows, or read denials are not recorded as blocked work). That is the owner's decision, per the brief.

## Stop 2 (caller's limit): the write-order rework reaches beyond the scope ledger
The blocked row is written before its scope row, in separate store calls, on five paths:
- engine_runtime_completion.go `persistFailureWithScopeEffects`: `persistAndHoldFailure` (applyResultPersistence -> retry_work) then `applyBlockScope`, in both the detected-block arm and the non-detection scope-blocking arm.
- engine_runtime_completion_trial.go `applyTrialRetryFallback`: `persistAndHoldFailure` then `transitionTrialScopeToPersistedBlock` (persistActiveScope).
- engine_runtime_completion_trial.go `applyTrialReclassification` (disk-local): `rehomeBlockedRetryWork` then `applyBlockScope`.
- engine_runtime_permissions.go, both `permissionEvidenceBoundaryDenied` arms: `recordPermissionBlockedRetry` then `applyBlockScope`.
One transaction for scope row and blocked row means threading the activation into the failure-persistence path (`applyResultPersistence`, the retry_work writers of completion, trial and permission handling), not only the ledger (`activateScope`, `persistActiveScope`, `applyBlockScope`, `transitionTrialScopeToPersistedBlock`). Swapping the order instead (scope first) trips `mustAssertInvariants("activate scope")` while the new scope has no blocked work yet.

## Facts gathered for whoever builds it
- (iii) Driver: the pooled store connections get `foreign_keys(ON)` through the DSN `_pragma` list (store.go), so every pooled connection has it; the read-only inspector opens `mode=ro` without the pragma (R1 report), harmless for a reader but it would not enforce.
- (iv) `UpsertBlockScope` (store_write_block_scopes.go) is `INSERT OR REPLACE INTO block_scopes`: with a key referencing the row, REPLACE deletes the parent first and would fail (no cascade) while blocked rows exist.
- (vi) `hash_panic` / `issueHashPanic` is named in 16 Go files under internal/sync (product: issue_types.go, scanner.go, condition_keys.go, observation_findings.go, truth_status.go, engine_runtime_completion.go, engine_runtime_lifecycle.go, engine_runtime_permissions.go; tests: 8) and in spec/design/sync-domain.md, sync-engine.md, sync-observation.md.
- (vii) Store generation is 28 (schema.go `currentSyncStoreGeneration`); a store of another generation is refused and reset with `onedrive-go drive reset-sync-state`.
