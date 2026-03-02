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
    cfg          *config.Config           // guarded by mu
    TokenSourceFn func(ctx, path, logger) // exported for test injection
    mu           sync.Mutex
    tokenCache   map[string]graph.TokenSource
}

func (p *SessionProvider) Session(ctx, rd) (*Session, error)
func (p *SessionProvider) UpdateConfig(cfg)  // SIGHUP reload

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

The config reference (`p.cfg`) is read under the lock to avoid a data race with `UpdateConfig()` during SIGHUP reload.

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
    Provider *driveops.SessionProvider
    // ...
}

// prepareDriveWork
session, err := o.cfg.Provider.Session(ctx, rd)

// reload — update both config references
o.cfg.Config = newCfg
o.cfg.Provider.UpdateConfig(newCfg)
```

Tests inject stubs via `cfg.Provider.TokenSourceFn = stubFn`.

### What Stayed in Root Package

- `newSyncEngine()` — creates `sync.EngineConfig` from `*driveops.Session`. Would create import cycle if moved to driveops (needs `sync.EngineConfig`). Only caller: `resolve.go`.
- `newGraphClient()` — used by auth commands (`login`, `logout`, `whoami`) which need graph.Client without a full Session.
