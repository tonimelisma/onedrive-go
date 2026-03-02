# `internal/driveops/` — Authenticated Drive Access

## Problem

The "resolved drive → authenticated Graph API clients" glue logic is duplicated:

- **`DriveSession`** in the CLI package (`drive_session.go`) — used by file ops (`ls`, `get`, `put`, `rm`, `mkdir`, `stat`) and `resolve`
- **`Orchestrator.getOrCreateClient`** in `internal/sync/` — used by sync commands

Both do the same 4 steps:

1. `config.DriveTokenPath(cid, cfg)` → token file path
2. `graph.TokenSourceFromPath(ctx, path, logger)` → auto-refreshing auth
3. `graph.NewClient(...)` × 2 → meta (30s timeout) + transfer (no timeout)
4. Validate DriveID is non-zero

`DriveSession` lives in the CLI layer, which is architecturally wrong — it's application infrastructure, not presentation logic. File-op commands like `ls` should not need to construct their own authenticated clients.

## Design Decision

Extract `internal/driveops/` as the single owner of "authenticated access to a drive."

### Dependency Direction

```
cmd/onedrive-go/  →  internal/sync/     →  internal/driveops/  →  internal/graph/
                  →  internal/driveops/                         →  internal/config/
                                                                →  internal/driveid/
```

- `driveops` imports `graph` (creates clients), `config` (resolves token paths), `driveid` (identity types)
- `sync` imports `driveops` (Orchestrator wraps Session with client caching)
- CLI imports `driveops` (file ops use Session directly)

### Package Contents

```go
// internal/driveops/session.go

// Session is an authenticated handle to a single drive. It bundles the
// metadata client (30s timeout), transfer client (no timeout), and the
// drive's resolved identity. Constructed once per command invocation.
type Session struct {
    Meta     *graph.Client
    Transfer *graph.Client
    DriveID  driveid.ID
    Resolved *config.ResolvedDrive
}

func NewSession(ctx context.Context, rd *config.ResolvedDrive, cfg *config.Config,
    metaHTTP, transferHTTP *http.Client, userAgent string, logger *slog.Logger,
) (*Session, error) {
    tokenPath := config.DriveTokenPath(rd.CanonicalID, cfg)
    if tokenPath == "" {
        return nil, fmt.Errorf("cannot determine token path for %s", rd.CanonicalID)
    }

    ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
    if err != nil {
        return nil, fmt.Errorf("token error for %s: %w", rd.CanonicalID, err)
    }

    if rd.DriveID.IsZero() {
        return nil, fmt.Errorf("drive ID not resolved for %s — re-run 'onedrive-go login'", rd.CanonicalID)
    }

    meta := graph.NewClient(graph.DefaultBaseURL, metaHTTP, ts, userAgent, logger)
    transfer := graph.NewClient(graph.DefaultBaseURL, transferHTTP, ts, userAgent, logger)

    return &Session{
        Meta:     meta,
        Transfer: transfer,
        DriveID:  rd.DriveID,
        Resolved: rd,
    }, nil
}
```

### Orchestrator Integration

The Orchestrator wraps `driveops.Session` with client caching by token path:

```go
// internal/sync/orchestrator.go

func (o *Orchestrator) sessionForDrive(ctx context.Context, rd *config.ResolvedDrive) (*driveops.Session, error) {
    // Cache check by token path (multiple drives may share a token)
    // On miss: driveops.NewSession(...)
    // Cache the Session's clients for reuse
}
```

### Migration

| Before | After |
|--------|-------|
| `drive_session.go` (CLI) | Deleted → `internal/driveops/session.go` |
| `drive_session_test.go` (CLI) | Deleted → `internal/driveops/session_test.go` |
| `sync_helpers.go` (`newSyncEngine`) | Stays for `resolve.go` until resolve is refactored |
| `files.go`: `NewDriveSession(ctx, cc.Cfg, cc.RawConfig, cc.Logger)` | `driveops.NewSession(ctx, cc.Cfg, cc.RawConfig, metaHTTP, transferHTTP, ua, cc.Logger)` |
| `resolve.go`: `NewDriveSession(...)` | `driveops.NewSession(...)` |
| `sync.go` watch bridge: `NewDriveSession(...)` | Deleted — watch routes through Orchestrator |
| Orchestrator: `getOrCreateClient` + `prepareDriveWork` | Refactored to use `driveops.NewSession` with caching |

### Future Growth

Natural candidates to migrate into `driveops/`:

- `TransferManager` construction (currently duplicated between `files.go` and `engine.go`)
- Item/path resolution helpers used by both file ops and sync
- Common download/upload wrappers

### Why Not Alternatives

- **Keep in CLI**: Semantically wrong — `DriveSession` is application infrastructure, not presentation
- **Put in `internal/sync/`**: `ls` importing sync is semantically wrong — listing files has nothing to do with syncing
- **Put in `internal/graph/`**: `graph` doesn't know about config or token paths — it's pure transport
- **Put in `internal/config/`**: Config reads settings, it doesn't create API clients — wrong responsibility
- **Orchestrator as universal factory**: Makes `ls` depend on sync orchestration — god object
