# Degraded Mode

Implements: R-6.10.7 [verified]

## Overview

The system degrades by isolating the smallest safe scope: command, drive,
observer, item, or scope block. It does not silently keep going when the
current source of truth is missing or untrusted.

## Failure Matrix

| Failure Source | Detection Owner | One-Shot Behavior | Watch Behavior | Persistence | Logging | User Recovery |
|---------------|-----------------|-------------------|----------------|-------------|---------|---------------|
| Config load or validation failure | `config` / CLI bootstrap | Abort the command before any data-plane work starts. | Initial startup aborts. SIGHUP reload failure logs a warning and keeps the last good in-memory config/runners. | None beyond the existing config file. | Fatal load errors at command start; reload failures at WARN. | Fix the config file or bad override and rerun/reload. |
| Token load or refresh failure | `graph` auth boundary | Abort the current command as fatal auth failure. | The affected drive stops making progress until auth is repaired; other drives continue. | Token file remains authoritative; no retry row is invented for bad credentials. | Auth failure at ERROR or user-facing command error. | Re-login or repair the token/account. |
| Graph transient/service failure (`408`, `423`, `429`, `5xx`, documented transient quirks) | `graph` + `sync` classifier | The pass records transient failures and returns; the next run reuses durable retry state. | Items become retryable failures or activate scope blocks with trials; unaffected work keeps running. | `sync_failures` and, when needed, `scope_blocks`. | Retry detail at DEBUG, scope/service degradation at WARN/INFO. | Wait for recovery or clear the underlying service issue. |
| Store open/corruption/recovery failure | `syncstore` / engine startup | Abort the drive before observation or execution. | The affected drive does not start or reload; other drives continue. | Existing DB remains the failed authority; no shadow store is created. | ERROR on startup/open failure. | Repair or rebuild the state store, then rerun. |
| Local permission denied | `sync` permission handling | Record actionable failure or boundary scope and continue with unrelated work. | Activate `perm:dir`/`perm:remote` scopes, keep readable work flowing, and recheck later. | Actionable or scope-backed `sync_failures`, plus `scope_blocks` when boundary-scoped. | WARN summaries plus reason/action text in issues output. | Fix permissions or clear the issue after access is restored. |
| Disk full / below `min_free_space` | `driveops` + `sync` classifier | Fail the affected download or abort further downloads in that pass as classified by disk policy. | Activate `disk:local` scope for downloads while allowing non-download work to continue; retry via scope trials. | Transient failure rows and `disk:local` scope state. | WARN/INFO for degraded scope; per-item detail at DEBUG. | Free disk space or lower/disable the reserve threshold intentionally. |
| Local or remote observer failure | observer + engine | Abort the pass for that drive because fresh observation is unavailable. | Observer loops back off and retry; the watch engine stays alive and reuses the shared pipeline when the observer recovers. | No synthetic alternate source of truth; durable state stays in the store. | WARN/ERROR on observer failure with retry/backoff detail at DEBUG. | Wait for recovery or fix the local/network cause. |
| Full reconciliation failure | watch-mode engine | Not applicable; reconciliation is watch-only. | Log the failure and continue normal watch operation; the next reconciliation interval retries. | Existing durable state remains authoritative; no partial reconciliation result is applied. | WARN/ERROR for the failed reconcile run. | Wait for the next cycle or inspect the logged error. |
| Shutdown during in-flight work | CLI + engine lifecycle | Respect cancellation, stop admitting new work, drain what can finish within the shutdown path, then exit. | First signal requests graceful drain; second signal forces exit. Crash recovery repairs unfinished durable state on next startup. | Partially completed work is recovered from store + sync root on next start. | INFO for graceful shutdown, abrupt termination only when forced. | Restart the command; recovery and durable retry state resume unfinished work. |
