# B5 report: one token source per sign-in file, on a bounded sign-in client

Commit `83c16cb1` on `fix/auth-bootstrap-client` (base `90c60a8f`), 31 files.

## What changed
- `internal/graphtransport/profiles.go`: `AuthClient()` (30 s headers, 60 s request, `buildTransport`, no retry/perf wrapper); `doc.go` mentions it.
- `internal/graph/auth.go`: `withAuthClient` installs `oauth2.HTTPClient`, nil is an error; `Login`/`LoginWithBrowser` take `authHTTP`.
- `internal/graph/auth_token.go`: `TokenSourceFromPath(ctx, path, authHTTP, logger)`; the source refreshes through `authHTTP`.
- `internal/graph/client_construction.go`: nil HTTP client is an error (no `http.DefaultClient`).
- `internal/driveops/session.go`: `BootstrapClient`, `graphBaseURL()`, runtime-owned `authHTTP`, default `TokenSourceFn` = `tokenSourceFromPath` (signature unchanged).
- `internal/cli/{email_reconcile,shared_target,shared_discovery,status_live,drive_search,drive_catalog,drive_add}.go`: the seven sites use `BootstrapClient`; base-URL params and `cc.graphBaseURL()` removed; `drive_add` drops an unused logger param.
- `internal/cli/root.go`: `cc.runtime()` is the one place `cc.GraphBaseURL` reaches the runtime (duplicates in `Session` and `sharedTargetClients` removed); dead `newGraphClient` removed. `account_view_snapshot.go` call-site change. `auth_flow.go` passes `graphtransport.AuthClient()` to both login flows.
- Test-only compile fixes: `graph/{auth,client,quirks,integration}_test.go`, `e2e/auth_preflight_e2e_test.go`, `cli/root_test.go`.
- Docs: R-6.8.27 in `non-functional.md`; `graph-client.md` (Implements, profiles, Authentication, NewClient, Runtime Ownership, Design Constraints, two Verified By rows); `sync-control-plane.md`; `drive-transfers.md`; `cli.md` (Verified By row).

## Tests added (all `// Validates: R-6.8.27`)
- `TestAuthClient_BoundsEveryRequest`: 60 s client timeout, 30 s header timeout, bare `*http.Transport`.
- `TestTokenSourceFromPath_RefreshUsesTheInjectedClient`: an expired sign-in refreshes through the injected client (one request, token endpoint, rotated token saved).
- `TestTokenSourceFromPath_StalledTokenEndpointTimesOut`: handler blocks on a channel closed at cleanup; two concurrent callers both return `context.DeadlineExceeded` at the injected 50 ms deadline (no sleep).
- `TestTokenSourceFromPath_NilAuthClientIsAnError`, `TestLoginFlows_NilAuthClientIsAnError`: nil auth client fails before any I/O (browser not opened, no token file).
- `TestLogin_UsesTheInjectedAuthClient`, `TestLoginWithBrowser_UsesTheInjectedAuthClient`: device code and its poll, and the code exchange, go through the injected client.
- `TestNewClient_NilHTTPClientIsAnError` (and `TestNewClient_Defaults` updated to pass a client).
- `TestSessionRuntime_BootstrapClientReusesOneTokenSourcePerPath`: 8 concurrent `BootstrapClient` calls plus an `InteractiveSession` build one source.
- `TestSessionRuntime_BootstrapClientKeepsNotLoggedIn`: `ErrNotLoggedIn` survives; failed loads are not cached.
- `TestSessionRuntime_BootstrapClientNeedsATokenPath`, `TestSessionRuntime_BootstrapClientUsesTheRuntimeBaseURL`, `TestSessionRuntime_DefaultTokenSourceHasTheAuthClient`.
- `TestCLIContextSession_ProbeAndSessionShareOneTokenSource`: `cc.Session`'s probe and session load the file once (would have failed before: the probe read the file itself).
- `TestCLIBootstrapClientsUseTheSessionRuntime`: source scan; failed with exactly seven files before the change.

## Checks run
- `go test -race -count=1 -timeout=20m` on `./internal/graph/...`, `./internal/graphtransport/...`, `./internal/driveops/...`, `./internal/cli/...`: all pass. New timing tests also passed `-count=30` (graph) and `-count=20` (driveops).
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full integration' ./e2e/... ./internal/graph/...`: clean.
- `devtool verify lint`: first run 3 issues (goconst, gosec G101 on a test constant name, G120 `ParseForm` without `MaxBytesReader` in a test handler), fixed; second run 0 issues (`B5-lint.log`).

## Deviations
- R-6.8.27 says "within one session runtime" rather than "within a process": `sync` keeps two runtimes (the command runtime for its sync-bootstrap probe, and the engine's from `sync_flow.go:186` / `sync_runtime.go:187`), recorded as `[planned]` in `sync-control-plane.md`. Unifying them changes the watch daemon's runtime logger (console timestamps) and eight `runSyncDaemonWithFactory` test calls, so it was left out.
- A `graph.NewClient` failure inside `BootstrapClient` now reaches the callers' sign-in classification (drive list/search, shared discovery, status overlay report "invalid saved login"); before, drive list/search skipped it silently and shared discovery marked the account degraded. It can only happen with an invalid test base URL.
- The cli call sites' error wording changed to "load sign-in for ...: ..." (no test asserted the old text).

## Recommendations
1. Give `sync` one session runtime for its probe and its engine (`runSyncOnce` already has `cc`; `runSyncDaemon` needs the runtime passed, with the timestamped logger).
2. `sharedBootstrapMetaClient` and `sharedTargetClients` do not flush the token cache after a probe renames the account, unlike `cc.Session`; harmless (the old path's entry is orphaned) but inconsistent. Consider flushing inside `probeAccountIdentity` on change.
3. `graph-client.md:22-25` still lists the profile builders without their `notices` parameter.

## Unfinished
- Nothing in scope.

## Packages the reviewer should test
- `./internal/multisync/...` (injects `TokenSourceFn`; signature unchanged, compiles, not run here) and the full verify; `e2e` compiles only.
