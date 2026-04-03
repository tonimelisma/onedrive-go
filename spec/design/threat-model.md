# Threat Model

Implements: R-6.10.7 [verified]

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

## Existing Mitigations

- Explicit filesystem trust boundaries are documented in [system.md](system.md) and enforced in code by `fsroot`, `synctree`, and `localpath`.
- Graph error bodies are size-capped and Graph quirks are normalized at the boundary; see [graph-client.md](graph-client.md).
- Token files are validated on read and write, and managed-state I/O stays rooted; see [graph-client.md](graph-client.md) and [config.md](config.md).
- Local observation filters invalid uploads before they become actions, while remote observation remains server-trusting to avoid silent data loss; see [sync-observation.md](sync-observation.md).
- Durable state is transactional and rebuildable; see [sync-store.md](sync-store.md) and [data-model.md](data-model.md).
- Retry, scope blocking, and graceful degradation are explicit runtime behaviors instead of ad hoc loops; see [retry.md](retry.md), [sync-engine.md](sync-engine.md), and [degraded-mode.md](degraded-mode.md).

## Residual Risks And Follow-Ups

- Resource-consumption guarantees per component remain a planned follow-up in [system.md](system.md).
- Full lock-ordering documentation is still incomplete; the current work documents ownership, not a finished global lock hierarchy.
- Secret-leak auditing remains ongoing for all log and error-message surfaces.
