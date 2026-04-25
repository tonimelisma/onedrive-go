# Live Recurring Families

This companion doc is the compact current-policy view for recurring live CI /
E2E / integration families.

Start every new live investigation in
[live-incidents.md](live-incidents.md) first so you can confirm whether the
same failure family already has chronology and evidence. After that first
lookup, use this document to jump directly to the current repo policy without
replaying the whole incident ledger.

## Mount-Root Children-Route Lag After Shared-Folder Bootstrap

Current repo policy:

- isolated shared-folder fixture setup must wait for the configured mount-root
  `ls /` boundary before handing the mount to later upload, sync, or watch
  coverage
- mount-root remote-read helpers (`stat`, `get`, parent `ls`, exact-path
  visibility waits) re-establish that same `ls /` boundary after the known
  `resolve item path ... list children for segment ... HTTP 404` family
- the recovery stays narrow to the documented mount-root route-lag shape; it
  does not absorb unrelated read failures

Promoted docs:

- [graph-api-quirks.md#fresh-parent-child-create-lag](graph-api-quirks.md#fresh-parent-child-create-lag)
- [cli.md](../design/cli.md)

Matching ledger incidents:

- [LI-20260405-09](live-incidents.md#li-20260405-09-recently-created-parent-folder-lagged-child-create-and-child-list-routes)
- [LI-20260422-02](live-incidents.md#li-20260422-02-shared-root-full-sync-commands-widened-or-destabilized-the-configured-subtree-after-nightly-harness-repair)

## Strict Auth Preflight Discovery / Projection Lag

Current repo policy:

- strict auth preflight still fails durable auth problems immediately
- the repo-owned preflight gives `/me` a bounded retry only for transient
  502/503/504 or transport-read failures
- `/me/drives` keeps its own bounded retry for transient `403 accessDenied`
  discovery/auth projection lag
- scheduled or manual `devtool verify e2e-full --classify-live-quirks` may
  rerun the strict auth preflight once for this exact known family and only
  downgrade it if the rerun passes

Promoted docs:

- [graph-api-quirks.md#strict-auth-preflight-quirks](graph-api-quirks.md#strict-auth-preflight-quirks)
- [system.md](../design/system.md)

Matching ledger incidents:

- [LI-20260405-06](live-incidents.md#li-20260405-06-strict-auth-preflight-treated-transient-me-or-medrives-glitches-as-durable-failure)

## Post-Mutation Destination-Path Visibility Lag

Current repo policy:

- successful mutation commands own a bounded follow-on visibility wait for the
  destination path
- generic fixture-readiness waits accept either exact `stat` success or visible
  parent listing, and they record this family as a softened recurrence when the
  exact path still lags
- tests that explicitly validate exact-route behavior must still poll the exact
  route instead of inheriting the looser fixture-seed contract

Promoted docs:

- [graph-api-quirks.md#post-mutation-visibility-lag](graph-api-quirks.md#post-mutation-visibility-lag)
- [cli.md](../design/cli.md)

Matching ledger incidents:

- [LI-20260405-07](live-incidents.md#li-20260405-07-destination-path-stayed-unreadable-after-successful-mutation)
- [LI-20260410-01](live-incidents.md#li-20260410-01-server-side-copy-rejected-a-freshly-visible-destination-folder)

## Immediate Post-Simple-Upload Mtime PATCH Transient Failures

Current repo policy:

- only the immediate post-simple-upload mtime PATCH gets the retry
- the retry remains narrow to HTTP 404 `itemNotFound` and transient
  502/503/504 server failures on the simple-upload finalization path
- direct metadata PATCH calls outside that finalization path stay strict

Promoted docs:

- [graph-api-quirks.md](graph-api-quirks.md)
- [drive-transfers.md](../design/drive-transfers.md)

Matching ledger incidents:

- [LI-20260408-01](live-incidents.md#li-20260408-01-immediate-post-simple-upload-mtime-patch-failed-transiently-after-successful-create)

## Shared-Folder Search Visibility In `drive list --json`

Current repo policy:

- exact shared-selector checks poll ordinary read-only propagation before
  asserting that a newly shared folder is visible through `drive list --json`
- repeated `drive list --json` comparisons such as `default` versus `--all`
  also poll for the default-visible shared entries instead of assuming one
  later search pass must expose the same selector set immediately
- if Graph never exposes the same selector on that run, the nightly test skips
  that shared-discovery assertion instead of treating one empty search pass as
  a product regression

Promoted docs:

- [system.md](../design/system.md)
- [developer-onboarding.md](developer-onboarding.md)

Matching ledger incidents:

- [LI-20260422-04](live-incidents.md#li-20260422-04-shared-folder-drive-list-exact-selector-check-assumed-one-pass-search-visibility)

## Upload-Only Live Sync Convergence Can Leave Retryable Work On First Pass

Current repo policy:

- nightly full-suite sync tests assert eventual sync convergence, not that the
  very first upload-only pass must clear every retryable transient
- the follow-up sync pass remains the contract-owned recovery path after live
  5xx or provider timing families leave `retry_work`

Promoted docs:

- [system.md](../design/system.md)
- [sync-engine.md](../design/sync-engine.md)

Matching ledger incidents:

- [LI-20260422-05](live-incidents.md#li-20260422-05-transfer-worker-e2e-assumed-one-upload-only-pass-could-not-leave-retryable-transient-work)
