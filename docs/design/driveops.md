# `internal/driveops/` — Authenticated Drive Access

## Problem

The "resolved drive → authenticated Graph API clients" glue logic was duplicated:

- **`DriveSession`** in the CLI package — used by file ops (`ls`, `get`, `put`, etc.)
- **`Orchestrator.getOrCreateClient`** in `internal/sync/` — used by sync commands
- **`TransferManager`** and **`SessionStore`** in `internal/sync/` — transfer concerns used by both CLI and sync

All three duplicated the same 4 steps: token path resolution → TokenSource creation → graph.Client construction → DriveID validation.

## Design Decision

`internal/driveops/` is the single owner of authenticated drive access, token caching, and transfer operations.

### Dependency Direction

```
cmd/onedrive-go/  →  internal/driveops/  →  internal/graph/
                  →  internal/sync/      →  internal/driveops/
                                         →  internal/config/
                                         →  internal/driveid/
                                         →  pkg/quickxorhash/
```

- `driveops` imports `graph`, `config`, `driveid`, `quickxorhash`
- `sync` imports `driveops` (Orchestrator delegates to SessionProvider)
- CLI imports `driveops` (file ops use `cc.Provider.Session()`)
- `driveops` does NOT import `sync` — no cycles

### Package Contents

**`session.go`** — `SessionProvider` + `Session`:
```go
type SessionProvider struct {
    holder        *config.Holder           // shared config owner
    TokenSourceFn func(ctx, path, logger)  // exported for test injection
    mu            sync.Mutex               // protects tokenCache only
    tokenCache    map[string]graph.TokenSource
}

func (p *SessionProvider) Session(ctx, rd) (*Session, error)

type Session struct {
    Meta     *graph.Client  // 30s timeout
    Transfer *graph.Client  // no timeout
    DriveID  driveid.ID
    Resolved *config.ResolvedDrive
}

func (s *Session) ResolveItem(ctx, remotePath) (*graph.Item, error)
func (s *Session) ListChildren(ctx, remotePath) ([]graph.Item, error)
func CleanRemotePath(path) string
```

**`transfer_manager.go`** — `TransferManager` (download/upload with resume, hash verification)

**`session_store.go`** — `SessionStore` (file-based upload session persistence)

**`interfaces.go`** — `Downloader`, `Uploader`, `RangeDownloader`, `SessionUploader`

**`hash.go`** — `SelectHash`, `ComputeQuickXorHash`

### Token Caching

`SessionProvider` caches `TokenSource`s by token file path. Multiple drives sharing a token path (e.g., personal + SharePoint on same account) share one `TokenSource`, preventing OAuth2 refresh token rotation races (two independent refreshes can invalidate each other's refresh tokens).

Config is accessed via `p.holder.Config()` — the `config.Holder` provides thread-safe read access (RWMutex). SIGHUP reload updates config via `holder.Update(newCfg)` in one place.

### CLI Integration

```go
// root.go — CLIContext (created in PersistentPreRunE Phase 2)
type CLIContext struct {
    Provider *driveops.SessionProvider  // nil for auth/account commands
}

// files.go — file-op commands
session, err := cc.Provider.Session(ctx, cc.Cfg)
items, err := session.ListChildren(ctx, remotePath)
```

The `sync` command creates its own `SessionProvider` because it uses `skipConfigAnnotation` and handles its own config resolution.

### Orchestrator Integration

```go
// OrchestratorConfig
type OrchestratorConfig struct {
    Holder   *config.Holder             // shared with SessionProvider
    Provider *driveops.SessionProvider
    // ...
}

// prepareDriveWork
session, err := o.cfg.Provider.Session(ctx, rd)

// reload — single-point config update via shared Holder
o.cfg.Holder.Update(newCfg)
```

Both `OrchestratorConfig.Holder` and `SessionProvider.holder` point to the same `*config.Holder` instance. On SIGHUP reload, one `holder.Update(newCfg)` call updates config for all consumers.

Tests inject stubs via `cfg.Provider.TokenSourceFn = stubFn`.

### What Stayed in Root Package

- `newSyncEngine()` — creates `sync.EngineConfig` from `*driveops.Session`. Would create import cycle if moved to driveops (needs `sync.EngineConfig`). Only caller: `resolve.go`.
- `newGraphClient()` — used by auth commands (`login`, `logout`, `whoami`) which need graph.Client without a full Session.
