# Live Incidents

This ledger records every investigated live CI / E2E / integration issue.

Use it as the exhaustive history. When the same live issue recurs, update the
existing entry instead of creating a duplicate. Behavior-shaping recurring
issues may also be summarized in curated reference docs such as
[graph-api-quirks.md](graph-api-quirks.md), but the ledger remains the source
of truth for what was seen, when it was seen, and how it was handled.

Promotion contract:
- this ledger owns chronology and investigation history
- promoted design/reference docs own the current repo policy
- recurring incidents should point to a canonical promoted-doc section so the
  current policy can be opened directly without replaying the whole incident
  history in your head

## Index

| Incident | Title | Status | Classification | Last seen | Recurring |
| --- | --- | --- | --- | --- | --- |
| LI-20260412-03 | Directional conflict E2Es still expected one-way overwrite semantics | fixed | test bug | 2026-04-12 | no |
| LI-20260412-02 | Queued conflict resolution could re-detect the same path before follow-up convergence | fixed | product bug | 2026-04-12 | no |
| LI-20260412-01 | Fast E2E smoke lane silently reran demoted direct file-op tests | fixed | test bug | 2026-04-12 | no |
| LI-20260410-01 | Server-side copy rejected a freshly visible destination folder | fixed | graph quirk | 2026-04-10 | no |
| LI-20260408-03 | Serialized `e2e_full` package exceeded the old 30-minute harness timeout | fixed | test harness | 2026-04-08 | no |
| LI-20260408-02 | `CreateFolder` returned success status with an empty body | mitigated | graph quirk | 2026-04-08 | no |
| LI-20260408-01 | Immediate post-simple-upload mtime PATCH returned `404 itemNotFound` | mitigated | graph quirk | 2026-04-08 | no |
| LI-20260405-06 | Strict auth preflight treated transient `/me` or `/me/drives` glitches as durable failure | mitigated | graph quirk | 2026-04-10 | yes |
| LI-20260405-09 | Recently created parent folder lagged child create routes | mitigated | graph quirk | 2026-04-12 | yes |
| LI-20260405-08 | Delete-by-ID returned `404 itemNotFound` after successful path lookup | mitigated | graph quirk | 2026-04-07 | yes |
| LI-20260405-07 | Destination path stayed unreadable after successful mutation | mitigated | graph quirk | 2026-04-10 | yes |
| LI-20260407-04 | Shared-file preflight assumed only one configured recipient could open the raw link | fixed | test bug | 2026-04-07 | no |
| LI-20260407-03 | Exact delete-target path lookup lagged parent listing during repeated sibling deletes | fixed | graph quirk | 2026-04-07 | no |
| LI-20260407-02 | Keep-local conflict resolution used parent-route upload despite known item identity | fixed | product bug | 2026-04-07 | no |
| LI-20260407-01 | Follow-on `put` lost a freshly visible parent path | fixed | graph quirk | 2026-04-07 | no |
| LI-20260406-01 | Personal scoped delta not ready after path resolution | fixed | graph quirk | 2026-04-06 | no |
| LI-20260405-05 | One-shot crash recovery left durable work unreplayed | fixed | product bug | 2026-04-05 | no |
| LI-20260405-04 | Fast E2E download-only assumed delta visibility too early | closed as test | graph quirk | 2026-04-10 | yes |
| LI-20260405-03 | Websocket watch tests timed websocket assertions before the steady-state subtree was ready | fixed | test bug | 2026-04-08 | yes |
| LI-20260405-02 | Stale root-level E2E artifacts inflated bootstrap and polluted live drives | fixed | test bug | 2026-04-05 | yes |
| LI-20260403-01 | Live Graph metadata requests stalled before response headers | mitigated | graph quirk | 2026-04-05 | yes |

## LI-20260412-03: Directional conflict E2Es still expected one-way overwrite semantics

First seen: 2026-04-12
Last seen: 2026-04-12
Area: `e2e_full`, directional sync semantics, conflict coverage
Suite / test: local `go run ./cmd/devtool verify e2e-full`, `TestE2E_Sync_DownloadOnlyIgnoresLocal`
Classification: test bug
Status: fixed
Recurring: no
Summary: The product already classified true two-sided drift as conflict even
under `--download-only` and `--upload-only`, but the full-suite directional
tests still expected those modes to behave like one-way overwrite switches.
After the directional sync refactor, the planner and executor continued to do
the safer thing: surface EF5 / EF9 / EF12 as conflicts and preserve both sides
instead of silently destroying one winner. The stale full E2Es therefore
started failing even though product behavior matched the intended conflict
model.
Evidence:
- Local `go run ./cmd/devtool verify e2e-full` on April 12, 2026 failed in
  `TestE2E_Sync_DownloadOnlyIgnoresLocal` with an internal baseline mismatch
  on `shared.txt`, because the test expected download-only to overwrite the
  divergent local edit and end clean.
- Repository inspection showed
  [`spec/design/sync-planning.md`](../design/sync-planning.md),
  [`internal/sync/planner.go`](../../internal/sync/planner.go), and
  [`internal/sync/executor_conflict.go`](../../internal/sync/executor_conflict.go)
  already treating EF5 edit/edit, EF9 edit/delete, and EF12 create/create as
  conflict flows regardless of directional mode.
- The stale tests were
  `TestE2E_Sync_DownloadOnlyIgnoresLocal` and
  `TestE2E_Sync_UploadOnlyIgnoresRemote`, which encoded overwrite semantics
  instead of ordinary one-sided deferral plus explicit conflict preservation.
Resolution / mitigation: the requirements and planning docs now state
explicitly that directional modes defer ordinary one-way reconciliation but do
not downgrade real conflicts into overwrites. The stale full-suite tests were
replaced with one-sided deferral coverage
(`TestE2E_Sync_DownloadOnlyDefersLocalOnlyChanges`,
`TestE2E_Sync_UploadOnlyDefersRemoteOnlyChanges`) plus explicit directional
conflict regressions for edit/edit, edit/delete, and create/create. Planner
regression coverage now also asserts that those conflict classes remain
`ActionConflict` in bidirectional, download-only, and upload-only modes.
Promoted docs: [sync.md](../requirements/sync.md), [sync-planning.md](../design/sync-planning.md), [sync-execution.md](../design/sync-execution.md)

## LI-20260412-02: Queued conflict resolution could re-detect the same path before follow-up convergence

First seen: 2026-04-12
Last seen: 2026-04-12
Area: `e2e_full`, manual conflict resolution, one-shot follow-up planning
Suite / test: local `go run ./cmd/devtool verify e2e-full`, `TestE2E_ConflictDetectionAndResolution`
Classification: product bug
Status: fixed
Recurring: no
Summary: A queued `resolve local` or `resolve remote` request could establish
the chosen file layout correctly and still let the immediately following
one-shot sync pass plan a brand-new conflict for the same canonical path. The
engine resolved the conflict row and changed the local layout, but the next
delta page could still report the losing side of the original edit race. With
no engine-owned follow-up change view for that just-resolved path, the planner
treated the stale remote delta plus the freshly chosen local layout as another
edit/edit conflict instead of converging the winner.
Evidence:
- Local `go run ./cmd/devtool verify e2e-full` on April 12, 2026 failed in
  `TestE2E_ConflictDetectionAndResolution` after the test queued
  `resolve local` and ran a normal `sync`; the conflict remained visible
  instead of clearing.
- Targeted unit reproduction in
  [`internal/sync/engine_watch_test.go`](../../internal/sync/engine_watch_test.go)
  showed `TestResolveConflict_KeepLocal_FollowUpSyncDoesNotRedetectConflictWhenRemoteDeltaStillShowsLoser`
  re-planning a new `edit_edit` conflict when the next remote delta still
  advertised the losing remote hash, even though the engine had already
  restored the chosen local winner.
- The same targeted suite showed the symmetric keep-remote case needed the
  same follow-up treatment to avoid unnecessary re-download or conflict
  re-detection when the next delta still showed the winner that was already on
  disk.
Resolution / mitigation: queued conflict resolutions now return an
engine-owned follow-up `PathChanges` batch that `RunOnce` merges before normal
planning. The canonical conflicted path gets a forced ordinary action
(`upload` for keep-local, `update_synced` for keep-remote / keep-both) plus a
synthetic remote view for the chosen outcome, so the same sync pass converges
instead of re-conflicting. Regression coverage lives in
[`internal/sync/engine_watch_test.go`](../../internal/sync/engine_watch_test.go).
Promoted docs: [sync-engine.md](../design/sync-engine.md)

## LI-20260412-01: Fast E2E smoke lane silently reran demoted direct file-op tests

First seen: 2026-04-12
Last seen: 2026-04-12
Area: fast-e2e verifier composition
Suite / test: PR `e2e` CI lane, `go run ./cmd/devtool verify e2e`
Classification: test bug
Status: fixed
Recurring: no
Summary: The documented fast-E2E contract from `#472` stayed correct, but the
direct file-op live tests that had been demoted to `e2e_full` were later
reintroduced into the fast-tagged [`e2e/e2e_test.go`](../../e2e/e2e_test.go).
That left the full-only owner file intact while also rerunning the legacy
file-op battery in the PR lane, so the “minimal smoke lane” silently grew back
to roughly three minutes even though the verifier, docs, and full-bucket
manifests still described the smaller suite.
Evidence:
- PR `e2e` run `24320726002` on April 12, 2026 finished `Run E2E tests` in
  `2m54s`, and the verifier summary inside that step still reported
  `fast e2e: pass (2m2.966s)` after separate auth (`14.165s`) and fixture
  (`8.841s`) preflights.
- PR `e2e` run `24320308641` on April 12, 2026 finished `Run E2E tests` in
  `3m04s`, and PR `e2e` run `24320303571` finished it in `2m52s`, confirming
  the PR lane had drifted back into the old ~3-minute band instead of the
  intended sub-2-minute envelope.
- Local `go test -tags=e2e ./e2e/... -list '^TestE2E_'` listed
  `TestE2E_FileOps_Whoami`, `TestE2E_FileOps_LsRoot`, `TestE2E_FileOps_Mkdir`,
  `TestE2E_FileOps_Put`, `TestE2E_FileOps_LsFolder`, `TestE2E_FileOps_Stat`,
  `TestE2E_FileOps_Get`, `TestE2E_FileOps_RmFile`,
  `TestE2E_FileOps_RmSubfolder`, `TestE2E_FileOps_RmPermanent`,
  `TestE2E_FileOps_Status`, `TestE2E_ErrorCases`, `TestE2E_JSONOutput`, and
  `TestE2E_QuietFlag` alongside `TestE2E_FileOpsSmokeCRUD`, proving the fast
  tag set contained both the old battery and the new smoke test.
- `git blame` traced the reintroduced block in
  [`e2e/e2e_test.go`](../../e2e/e2e_test.go) back to merged commit `f6af69a`
  (`refactor: finalize simplified directional sync architecture (#473)`).
Resolution / mitigation: The fast-tagged file no longer owns those demoted live
tests; [`e2e/fileops_full_e2e_test.go`](../../e2e/fileops_full_e2e_test.go)
remains the sole owner of the extended direct file-op coverage, and
[`internal/devtool/verify_test.go`](../../internal/devtool/verify_test.go)
now uses tagged-test discovery to regression-test that the `//go:build e2e`
set contains only the intended smoke live tests plus helper tests.
Promoted docs: [system.md](../design/system.md)

## LI-20260410-01: Server-side copy rejected a freshly visible destination folder

First seen: 2026-04-10
Last seen: 2026-04-10
Area: `e2e_full`, CLI `cp`, Graph copy start
Suite / test: local `go run ./cmd/devtool verify e2e-full`, `TestE2E_Cp_IntoFolder`
Classification: graph quirk
Status: fixed
Recurring: no
Summary: A full-suite copy flow proved that Graph's server-side copy verifier
can lag behind normal path reads for a freshly created destination folder.
During the live run, the destination folder was already readable by path, but
the immediate `POST .../copy` still returned `404` with `Failed to verify the
existence of destination location`. The product already had narrow retries for
other fresh-parent create routes, but copy start still treated this false
negative as terminal.
Evidence:
- Local `go run ./cmd/devtool verify e2e-full` on April 10, 2026 failed in
  `TestE2E_Cp_IntoFolder` while copying
  `/e2e-cp-folder-1775879036248706000/src.txt` into
  `/e2e-cp-folder-1775879036248706000/dest`.
- The child CLI log first showed
  `GET /drives/bd50cf43646e28e6/root:/e2e-cp-folder-1775879036248706000/dest: = 200`,
  proving the destination folder was already path-visible.
- The immediate follow-on
  `POST /drives/bd50cf43646e28e6/items/BD50CF43646E28E6!s75b8ff1dfb2846adbb1003d999d52233/copy`
  then returned HTTP 404 with request ID
  `f7f79148-fe82-4350-8bc7-a620c0598a1b` and message
  `Failed to verify the existence of destination location`.
Resolution / mitigation: `graph.Client.CopyItem()` now owns a narrow bounded
retry for that exact 404 family, and the full-suite `cp` assertions poll for
the copied child to appear instead of assuming post-copy listing is strongly
consistent on the first read.
Promoted docs: [graph-client.md](../design/graph-client.md), [graph-api-quirks.md](graph-api-quirks.md)

## LI-20260408-03: Serialized `e2e_full` package exceeded the old 30-minute harness timeout

First seen: 2026-04-08
Last seen: 2026-04-08
Area: nightly/manual full E2E verifier
Suite / test: local `go run ./cmd/devtool verify e2e-full --classify-live-quirks`
Classification: test harness
Status: fixed
Recurring: no
Summary: After the verifier intentionally serialized the `e2e_full` package
with `go test -parallel 1`, the old package-level `-timeout=30m` budget was
no longer large enough for the now-non-overlapping live suite. The resulting
panic was a harness/runtime-budget bug, not a product regression in the test
that happened to be running when the package timer expired.
Evidence:
- Local `go run ./cmd/devtool verify e2e-full --classify-live-quirks` on
  April 8, 2026 reached `panic: test timed out after 30m0s`.
- The panic caught `TestE2E_Cp_IntoFolder` only 7 seconds into its own body,
  which showed the package-level timer, not that individual scenario, had
  become the limiting factor after serializing the suite.
- The same run had already cleared the earlier full-suite regressions
  (`TestE2E_SyncWatch_WebsocketRemoteWakeAndRestart` and
  `TestE2E_Sync_CreateCreateConflict_ResolveKeepLocal`) before hitting the
  harness cap.
Resolution / mitigation: `devtool verify e2e-full` no longer runs the whole
`e2e_full` package as one serial block. It now keeps the same `60m` timeout
budget but splits the suite into three verifier-owned buckets so only the
already-vetted misc lane regains `-parallel 5`, while sync/watch/shared lanes
stay serial. The full preflight still owns the one-time remote scrub, bucketed
runs skip repeat scrubs with `ONEDRIVE_E2E_SKIP_SUITE_SCRUB=1`, and scheduled
/ manual CI now uploads verifier and timing-summary artifacts so long-lane
runtime and classified reruns are visible instead of opaque.
Promoted docs: [system.md](../design/system.md)

## LI-20260408-02: `CreateFolder` returned success status with an empty body

First seen: 2026-04-08
Last seen: 2026-04-08
Area: `e2e_full`, CLI `mkdir`, Graph item mutation boundary
Suite / test: local `go run ./cmd/devtool verify e2e-full --classify-live-quirks`, `TestE2E_Cp_File`
Classification: graph quirk
Status: mitigated
Recurring: no
Summary: Graph returned success for `POST .../children` during `mkdir`, but the
body was empty enough that `CreateFolder()` failed on `EOF` before it could
normalize the created item. Retrying the non-idempotent create would risk
turning a committed success into a false `nameAlreadyExists` conflict, so the
client needed a read-back confirmation path instead of a replay.
Evidence:
- Local `go run ./cmd/devtool verify e2e-full --classify-live-quirks` on April
  8, 2026 failed in `TestE2E_Cp_File` while creating
  `/e2e-cp-file-1775671921484525000`.
- `POST /drives/bd50cf43646e28e6/items/root/children` returned HTTP 200 with
  request ID `4b1bc4e6-0a58-4d5c-bda6-95c14326203f`.
- The body was empty enough that `CreateFolder()` surfaced
  `graph: decoding create folder response: EOF`.
Resolution / mitigation: `graph.Client.CreateFolder()` now treats an empty
success body as ambiguous success, then confirms the created folder by
re-listing the parent collection under the bounded post-mutation visibility
budget instead of replaying the create request.
Promoted docs: [graph-api-quirks.md](graph-api-quirks.md), [graph-client.md](../design/graph-client.md)

## LI-20260408-01: Immediate post-simple-upload mtime PATCH returned `404 itemNotFound`

First seen: 2026-04-08
Last seen: 2026-04-10
Area: fast E2E, CLI `put`, simple-upload finalization
Suite / test: local `go run ./cmd/devtool verify default`, `TestE2E_RoundTrip/rm_permanent` setup `put`
Classification: graph quirk
Status: mitigated
Recurring: yes
Summary: Graph can return a concrete item ID from a successful small-file
simple upload and then immediately reject the follow-on
`UpdateFileSystemInfo` PATCH for that same item with `404 itemNotFound`. The
file creation itself succeeded; the failure is a false negative in the mtime
preservation step.
Evidence:
- Local `go run ./cmd/devtool verify default` on April 8, 2026 uploaded
  `/onedrive-go-e2e-1775667044557991000/perm-test.txt` successfully via
  simple upload during `TestE2E_RoundTrip/rm_permanent` setup.
- The upload response returned item ID
  `BD50CF43646E28E6!s0db1ece8e28d4085845e623128c01e29`.
- The immediate follow-on
  `PATCH /drives/bd50cf43646e28e6/items/BD50CF43646E28E6!s0db1ece8e28d4085845e623128c01e29`
  still returned HTTP 404 `itemNotFound` with request ID
  `c597aa4d-e4c5-4c89-8888-fa9a07ab36db`.
- The later `rm_permanent` failure in that same fast-suite run was secondary:
  the fixture path had never been created cleanly because the preceding `put`
  had already surfaced this false negative.
Resolution / mitigation: simple-upload finalization now owns a narrow bounded
retry for the exact follow-on `UpdateFileSystemInfo` `404 itemNotFound` case.
Direct `UpdateFileSystemInfo()` calls remain strict; only the immediate
post-simple-upload patch gets this normalization. A later April 10, 2026 fast
E2E verifier run hit the same family again during `TestE2E_Sync_Conflicts`
while overwriting a remote file via CLI `put`, so the retry budget was widened
to 6 total attempts with a 250ms base, 2x multiplier, no jitter, and 4s max.
Promoted docs: [graph-api-quirks.md](graph-api-quirks.md), [graph-client.md](../design/graph-client.md), [drive-transfers.md](../design/drive-transfers.md), [transfers.md](../requirements/transfers.md)

## LI-20260405-06: Strict auth preflight treated transient `/me` or `/me/drives` glitches as durable failure

First seen: 2026-04-05
Last seen: 2026-04-10
Area: scheduled/full live verification, auth preflight, drive catalog
Suite / test: scheduled `e2e_full` `whoami`, local `verify default` `TestE2E_AuthPreflight_Fast`
Classification: graph quirk
Status: mitigated
Recurring: yes
Summary: Microsoft Graph can expose short-lived auth-preflight inconsistencies
on either endpoint the repo uses as an early live-account check. Earlier
failures showed `/me` succeeding while `/me/drives` stayed on transient
`403 accessDenied`; later verifier evidence broadened the family when `/me`
itself returned a one-off `504 GatewayTimeout` / `ProfileException` before
recovering. The production CLI already degrades or retries at the correct
runtime boundaries, but the repo-owned strict auth preflight still needed its
own endpoint-specific transient handling so live suites do not fail on a
single recoverable `/me` glitch.
Evidence:
- Scheduled `e2e_full` run `23999446320` on April 5, 2026 got `GET /me = 200`,
  then 3 consecutive `/me/drives = 403 accessDenied` failures over about 3.1
  seconds under the old production retry budget, before a later `whoami`
  command in the same run succeeded again.
- Local `go run ./cmd/devtool verify default` on April 8, 2026 failed
  `TestE2E_AuthPreflight_Fast/personal_kikkelimies123@outlook.com` after 10
  consecutive `/me/drives = 403 accessDenied` responses over 29.942 seconds.
- Request IDs from that April 8 strict-preflight failure:
  `312b53d7-f478-48c4-9f36-363067a2c1b6`,
  `b58ff5b7-6764-4223-9c83-ceb6f1ca3222`,
  `b0cec41d-7b15-4c81-8391-312edf2239d0`,
  `1cf97115-e2ab-4330-9706-c39df8294dcb`,
  `03c81db2-eb2f-4d77-b142-1a1b481f0cd9`,
  `0db09ec7-3a42-4ae7-aba0-fbf45beeb6fc`,
  `9c3ff205-3eaf-4dc8-83fa-8b80afdd9d02`,
  `69c9fbf9-ffa8-49e8-94f0-558cea646da9`,
  `24529051-0dd1-49a9-a908-f0c1b53930e4`,
  `a7fe6793-d731-4c0d-b6f5-a424e9de2d45`
- Local `go run ./cmd/devtool verify default` on April 9, 2026 failed
  `TestE2E_AuthPreflight_Fast/personal_testitesti18@outlook.com` when
  `GET /me` returned HTTP 504 `GatewayTimeout` with message
  `ProfileException` and request ID `446dc036-f752-4805-9c33-d637eb70975d`.
- Local `go run ./cmd/devtool verify default` on April 10, 2026 failed
  `TestE2E_AuthPreflight_Fast/personal_testitesti18@outlook.com` after 10
  consecutive `/me/drives = 403 accessDenied` responses over about 30.2
  seconds with request IDs
  `33133131-635a-459d-ae94-27ad662726fc`,
  `1735118a-3af3-4c10-9ca9-42341dc34d3c`,
  `02c6479c-b1f1-48a2-9df7-59839e9aeb13`,
  `c8c28fc2-602d-4353-93dc-8bc922e322cc`,
  `2609dbc8-b7cd-4c21-a4e2-a9fea4b97685`,
  `6b8f17e3-0afc-4449-9018-0b75a7ec9464`,
  `db8fbae0-2a00-47fe-95c8-bd922e5b1daf`,
  `635af068-cb2c-4854-b2a9-5419966d6a69`,
  `003f4e1e-0183-4168-8d9d-6c392e6406d0`, and
  `79db09a2-6d40-4a44-ba9a-73d1119690ac`.
- An immediate isolated rerun of the same preflight later passed for that
  account, confirming both the `/me/drives` and `/me` failures were transient.
Resolution / mitigation: `graph.Client.Drives()` now owns a narrow 5-attempt
retry for `/me/drives` `403 accessDenied`, and caller behavior above it treats
retry exhaustion as authenticated degraded discovery rather than auth failure.
The repo-owned auth preflight now keeps separate bounded endpoint windows:
`/me` retries only transient gateway/service or transport-read failures,
while `/me/drives` keeps polling through the already documented projection lag.
That keeps required lanes strict for durable auth failures while no longer
failing on a single recoverable `/me` glitch. Scheduled/manual
`devtool verify e2e-full --classify-live-quirks` still reruns that exact strict
preflight once and only downgrades it when the rerun passes; the verifier
summary records that classified rerun explicitly so nightly/manual CI can
distinguish a clean pass from a green-after-rerun pass.
Promoted docs: [graph-api-quirks.md#strict-auth-preflight-quirks](graph-api-quirks.md#strict-auth-preflight-quirks), [graph-client.md](../design/graph-client.md), [degraded-mode.md](../design/degraded-mode.md), [cli.md](../design/cli.md)

## LI-20260405-09: Recently created parent folder lagged child create routes

First seen: 2026-04-05
Last seen: 2026-04-12
Area: `e2e_full` and fast `e2e`, child create after recently created parent visibility
Suite / test: `verify e2e-full`, `TestE2E_SyncWatch_WebsocketStartupSmoke`; later `TestE2E_Sync_CreateCreateConflict_ResolveKeepLocal`; later `TestE2E_Sync_DriveRemoveAndReAdd`; later PR `e2e` CI `TestE2E_Sync_IgnoreMarkerRemovalReconcilesBlockedRemoteDownload` fixture setup
Classification: graph quirk
Status: mitigated
Recurring: yes
Summary: A recently created folder can become readable by path lookup before
Graph accepts follow-on child creates against that same parent. The live
failures first showed up on the upload-session route and later recurred even
after the session-route permission oracle had exhausted and the final simple
create still returned `404 itemNotFound`.
Evidence:
- Local `go run ./cmd/devtool verify e2e-full` on April 5, 2026 created
  `/e2e-watch-websocket-1775447250013596000`, then confirmed the folder was
  already readable by path lookup with request ID
  `c19f75f0-9a85-43e0-8144-0b4be7387226`.
- The immediate follow-on `POST ...:/createUploadSession` for `first.txt`
  still returned HTTP 404 `itemNotFound` with request ID
  `d02b9317-d3d5-44ad-a30c-327df8c859d3`.
- The same Graph family recurred on April 7, 2026 in
  `TestE2E_Sync_CreateCreateConflict_ResolveKeepLocal`, where repeated
  parent-route `createUploadSession` failures for `collision.txt` ended with
  request ID `9dce082f-97ae-4f6c-9cc2-69b650dcf4c1` before the product fix
  stopped using that route when authoritative item identity was already known.
- The same family recurred again on April 8, 2026 during
  `go run ./cmd/devtool verify e2e-full --classify-live-quirks`, where
  `put /e2e-sync-cc-1775669439198173000/collision.txt` first got simple-upload
  `404 itemNotFound`, then exhausted the bounded `createUploadSession` retry
  budget with final request ID `29f1df3d-8ec4-422a-8065-27336de29f00`.
- The same family recurred again on April 8, 2026 in
  `TestE2E_Sync_DriveRemoveAndReAdd`: after the first `sync --upload-only
  --force` created `/e2e-sync-readd-1775676089553365000/file1.txt`, the second
  sync still exhausted `createUploadSession` on `file2.txt` for parent
  `BD50CF43646E28E6!sa7cb589636134fe4b1bf296e555fb410`, then exhausted the old
  final simple-upload replay budget with request ID
  `a9ad7aa8-ba79-424d-9ead-9b718939ddca`.
- The same family recurred again on April 10, 2026 during local
  `go run ./cmd/devtool verify default` in `TestE2E_RoundTrip/put`: the fresh
  parent `/onedrive-go-e2e-1775869538090298000` was initially readable by path
  lookup (`request-id: 789200c7-e924-42d6-b7b6-5e4c4b9d2dad`), but the child
  create for `test.txt` still hit the documented fresh-parent path lag. The
  fallback `POST ...:/createUploadSession` retried through request IDs
  `53caf9d0-a53a-4a36-91ae-80f4edaa50e0`,
  `a54221af-4bef-4591-ab24-e94b5a1d8e2e`,
  `45bc7457-377d-434a-b00c-d4c702a89bbb`,
  `121a4763-1951-4bf3-8020-e1e815b166c2`,
  `9916ac49-9af8-4acd-98bd-28a64eced354`, and
  `46d04eb1-07df-4141-adf8-7e31c20d48e8` before the replayed simple-upload path
  also exhausted with final request ID `1f05a8a1-16e7-4e98-baad-72edfbe95218`.
  The rest of `TestE2E_RoundTrip` then failed only because the original child
  create never converged, leaving the parent path intermittently unreadable by
  exact route while root listing still showed unrelated siblings.
- An immediate isolated rerun on April 10, 2026 passed:
  `go test -tags=e2e ./e2e -run TestE2E_RoundTrip -count=1`
  completed successfully in about 66 seconds, confirming the same transient
  fresh-parent create family rather than a deterministic product regression.
- GitHub Actions PR `e2e` run `24275068041` on April 11, 2026 hit the same
  family in `TestE2E_Sync_IgnoreMarkerRemovalReconcilesBlockedRemoteDownload`
  before the test reached its actual ignore-marker assertion. After
  `mkdir /e2e-sync-marker-1775883434742317229/blocked`, fixture setup
  `put /e2e-sync-marker-1775883434742317229/blocked/secret.txt` exhausted the
  bounded `simple-upload-create-transient-404` retry family through request
  IDs `0cca0a1f-1ddb-4be4-8455-8fd1f06dbfc2`,
  `b9d5a1df-6fec-411c-9111-bb42feae1179`,
  `915727ce-0845-413f-a498-28b88c29ea48`,
  `229237c5-f2c1-438d-bb0c-d7b416482345`,
  `e5cd276f-fb21-4117-bc12-4cd738b1a81b`,
  `46a997a3-1e97-4ce3-b2b4-a27244a5adf1`, and final request ID
  `2ffe00d6-8b13-4f83-b4e0-9de51a8691ad`.
- The same family recurred again on April 10, 2026 during a local rerun of
  `go run ./cmd/devtool verify default` in `TestE2E_Sync_Conflicts`. The
  earlier local `sync --upload-only` had already returned success for
  `/e2e-fast-conflict-1775883850469638000/conflict.txt`, but the later fixture
  `put` used to seed the remote edit still spent its full bounded parent-path
  convergence window getting `GET ...root:/e2e-fast-conflict-1775883850469638000:`
  `404 itemNotFound` responses, ending with request ID
  `2017317f-6a94-454d-9bbb-6476355e92f1`, while the companion root listing also
  still omitted that folder on its final pass (`request-id:
  2fc18cd3-4efb-470b-ba09-5df096fe3a91`). The helper then surfaced
  `resolving parent "...": remote path not yet visible` before the test ever
  reached its actual conflict assertion.
- The same family recurred again on April 12, 2026 while fixing
  `TestE2E_Sync_BidirectionalMerge`. After a separate planner regression was
  fixed, the remaining isolated repro still recreated
  `/e2e-sync-bidi-1776045531019802000/local-only`, then immediately tried to
  upload `stuff.txt` through that fresh parent item's ID. The child upload
  exhausted the bounded `simple-upload-create-transient-404` family and the
  follow-on `ls /e2e-sync-bidi-1776045531019802000/local-only` still returned
  `404`, showing the already-documented fresh-parent create lag rather than a
  new Graph behavior.
Resolution / mitigation: `graph.Client.CreateUploadSession()` now owns a
bounded retry for the exact fresh-parent `404 itemNotFound` case, and flows
that already know the authoritative remote `itemID` avoid parent-route create
paths entirely by overwriting via item ID instead. When a small-file create has
already seen the initial simple-upload `404`, the graph boundary now replays
that original simple upload under a second, slightly longer bounded
create-convergence policy after the session path still exhausts on exact
`itemNotFound`. Fast E2E fixture helpers that are only seeding unrelated tests
now retry the whole fixture `put` when that command hits either the documented
fresh-parent create exhaustion signatures or the bounded
`resolving parent ... remote path not yet visible` variant, so the suite does
not tie unrelated assertions to one specific command window being the moment
Graph finally converges. Sync execution now also waits for the already-known
parent path to become readable before it spends dependent remote folder-create
or new-file upload routes under that parent, so one sync pass does not race a
freshly created parent item ID into the same documented Graph lag family.
Promoted docs: [graph-api-quirks.md#fresh-parent-child-create-lag](graph-api-quirks.md#fresh-parent-child-create-lag), [graph-client.md](../design/graph-client.md), [drive-transfers.md](../design/drive-transfers.md), [sync-execution.md](../design/sync-execution.md)

## LI-20260405-08: Delete-by-ID returned `404 itemNotFound` after successful path lookup

First seen: 2026-04-05
Last seen: 2026-04-12
Area: `e2e_full`, CLI `rm`, forced-overwrite cleanup
Suite / test: `go test -tags='e2e e2e_full' ./e2e -run '^TestE2E_EdgeCases$|^TestE2E_Sync_BidirectionalMerge$'`; later isolated `TestE2E_Sync_DeleteSafetyThreshold`
Classification: graph quirk
Status: mitigated
Recurring: yes
Summary: Graph can agree that a path exists and then immediately reject the
delete for the resolved item ID with `404 itemNotFound`. Later live coverage
showed the same delete-convergence family can also hit the initial exact path
lookup during repeated sibling deletes, so a one-shot resolve-plus-delete flow
was not trustworthy enough for path-authoritative CLI delete intent.
Evidence:
- Local `go test -tags='e2e e2e_full' ./e2e -run '^TestE2E_EdgeCases$|^TestE2E_Sync_BidirectionalMerge$'`
  on April 5, 2026 resolved
  `/onedrive-go-e2e-edge-1775450932112095000/concurrent-1.txt` successfully by
  path during cleanup.
- The immediate `DELETE /drives/bd50cf43646e28e6/items/BD50CF43646E28E6!s5fd8d410d21d4234a567f45e01fc46e2`
  still returned HTTP 404 `itemNotFound` with request ID
  `335ea56d-e3a9-4d2f-8b4c-742da9088eec`.
- On April 7, 2026, the isolated repro for `TestE2E_Sync_DeleteSafetyThreshold`
  extended the same family: after repeated sibling deletes, the initial exact
  path lookup for `file-10.txt` returned `404 itemNotFound` even though the
  file had been created successfully earlier in the same run.
- On April 9, 2026 local `go run ./cmd/devtool verify default` hit the same
  delete-intent family in `TestE2E_RoundTrip/rm_subfolder`: the exact target
  path `/onedrive-go-e2e-1775721721283528000/subfolder` returned `404
  itemNotFound`, and the first fallback `GET
  /root:/onedrive-go-e2e-1775721721283528000:/children` also returned `404`
  even though the parent subtree had already been used successfully earlier in
  the same round-trip. The failing request IDs were
  `b98e08f8-d6ee-43ee-9fc5-29229235a489` for the exact target path and
  `88168b5c-0f40-482e-8512-de77dc1c24e7` for the parent-children route.
Resolution / mitigation: `driveops.Session` now owns delete-specific recovery.
`ResolveDeleteTarget()` falls back from exact path lookup to the parent
collection before declaring a path missing. When that parent-path collection
route is itself in a transient `itemNotFound` gap, delete intent now resolves
the parent folder recursively through ancestor collections and then lists the
parent's children by item ID instead of failing on the first path-shaped
false negative. `DeleteResolvedPath()` / `PermanentDeleteResolvedPath()` still
retry delete intent against the currently resolved target while treating a
now-missing path as success.
Promoted docs: [graph-api-quirks.md#post-mutation-visibility-lag](graph-api-quirks.md#post-mutation-visibility-lag), [drive-transfers.md](../design/drive-transfers.md)

## LI-20260405-07: Destination path stayed unreadable after successful mutation

First seen: 2026-04-05
Last seen: 2026-04-10
Area: `e2e_full`, CLI mutation follow-on path reads
Suite / test: `verify e2e-full`, `TestE2E_Sync_EditEditConflict_ResolveKeepRemote`; later `TestE2E_Sync_BidirectionalMerge`, `TestE2E_Resolve_Both_PreservesConflictCopy`, local `verify default` scoped-sync fixture setup, and `TestE2E_RoundTrip/rm_permanent`
Classification: graph quirk
Status: mitigated
Recurring: yes
Summary: Graph can acknowledge a successful metadata-changing mutation and
still return `404 itemNotFound` on the immediate follow-on path read for the
destination. The same convergence family later also affected parent-path reads
that `put` depended on before uploading a child.
Evidence:
- Local `go run ./cmd/devtool verify e2e-full` on April 5, 2026 completed
  `put /e2e-sync-ee-1775448127403708000/conflict-file.txt` during
  `TestE2E_Sync_EditEditConflict_ResolveKeepRemote`.
- The immediate follow-on
  `stat /e2e-sync-ee-1775448127403708000/conflict-file.txt` kept returning HTTP
  404 `itemNotFound` with request ID
  `0d76a7d9-c2b4-4eec-acd2-de29e5aec5c7` until the test's 30-second poll window
  expired.
- The same day, `TestE2E_Sync_BidirectionalMerge` exhausted the older
  six-step visibility schedule with 7 consecutive `GET by path = 404
  itemNotFound` responses after a successful `put
  /e2e-sync-bidi-1775450238168612000/data/info.txt`, which led to the added
  final `16s` visibility step.
- Local `go run ./cmd/devtool verify default` on April 8, 2026 failed
  `TestE2E_Sync_DriveRemoveAndReAdd` after `sync --upload-only --force`
  completed successfully and its own executor visibility probe read
  `file1.txt` via path, but follow-on CLI `stat
  /e2e-sync-readd-1775668702253491000/file1.txt` still returned repeated
  `404 itemNotFound` for more than 30 seconds. Request IDs from that run
  included `f8bcdbf0-4d3a-4308-8641-2eae31d32728`,
  `001ccaad-0943-41d4-bc46-c2f33cea05c7`,
  `393649c4-9a53-45e9-9dcd-bd9d4689adbe`, and
  `f91e7f3b-24ea-4f62-b985-3a513002e2e8`.
- Later the same day, another local `go run ./cmd/devtool verify default`
  failed the same test one step later in the scenario: after drive remove and
  re-add, `sync --upload-only --force` completed but follow-on CLI `stat
  /e2e-sync-readd-1775673996862622000/file2.txt` still returned repeated
  `404 itemNotFound` for more than 2 minutes, and the cleanup `rm -r
  /e2e-sync-readd-1775673996862622000` also observed the parent path as
  missing. Request IDs from that run included
  `718c9dc3-5bd6-41c3-a6ac-f8ef4dee49f5`,
  `71f924c5-2675-4d03-b207-70426adcb3c2`,
  `9b7a6bc5-213d-4c26-bb49-951571a0b809`, and
  `b2f572f0-161d-4363-a45a-4550ba093a28`. An immediate isolated rerun of
  `TestE2E_Sync_DriveRemoveAndReAdd` passed in 13.49s, which points to the
  same transient Graph propagation family rather than a deterministic product
  regression in the refactor under test.
- A later April 8, 2026 `go run ./cmd/devtool verify default` hit the same
  broader visibility family in `TestE2E_RoundTrip/rm_subfolder`: `rm -r
  /onedrive-go-e2e-1775674310493677000/subfolder` deleted the child folder, but
  the follow-on parent visibility confirmation kept reading the parent path
  `/onedrive-go-e2e-1775674310493677000` as `404 itemNotFound` for more than
  30 seconds and eventually failed with `remote path not yet visible`. The run
  also saw the initial child DELETE return `404` before the by-path read proved
  the child was gone. Representative request IDs from that run included
  `b7fd7590-1fa2-440c-93ff-6e437e84f1ef`,
  `6006d2b2-c23e-451f-867a-0548d72e93d1`,
  `d3f4aee6-6ccc-4b87-8c44-2cddcefccdca`, and
  `333e6384-ec68-4812-b48d-44da956a029d`. An immediate isolated rerun of
  `TestE2E_RoundTrip` passed in 30.35s, again pointing to transient Graph
  visibility lag rather than a deterministic regression in the branch under
  test.
- Another April 8, 2026 `go run ./cmd/devtool verify default` reproduced the
  same family one increment later in `TestE2E_RoundTrip`: after `rm
  /onedrive-go-e2e-1775674751903651000/test.txt` returned success, every
  follow-on by-path read of the still-existing parent
  `/onedrive-go-e2e-1775674751903651000` returned `404 itemNotFound` for more
  than 30 seconds, which then cascaded into `rm_subfolder` and `rm_permanent`
  failures because the root folder never reconverged for subsequent commands.
  Representative request IDs from that run included
  `71dd8282-dc1f-4270-b87b-4da16e664cdc`,
  `cff44bae-7863-4a8b-b345-6d85ace95cd8`,
  `73104aa2-ef31-4543-945d-d05506521ed2`, and
  `9eee346a-951e-48b9-8484-da811a59c4dd`.
- On April 7, 2026, `TestE2E_Resolve_Both_PreservesConflictCopy` hit the same broader
  family earlier in the flow when a freshly visible parent path still returned
  `404 itemNotFound` to the next `put`, which is tracked separately in
  `LI-20260407-01`.
- On April 8, 2026 local `go run ./cmd/devtool verify e2e-full --classify-live-quirks`
  failed `TestE2E_Sync_DriveRemoveAndReAdd` after a successful
  `sync --upload-only --force` because the test still used the generic
  30-second stat poll for `/e2e-sync-readd-1775675752503071000/file1.txt`.
  Repeated `stat` reads kept returning HTTP 404 `itemNotFound` with request IDs
  `0b005d4f-c866-45b4-be1e-acb6d28b6cb2`,
  `6b60ec38-202b-4108-8487-e877fa6794d3`, and
  `d7c1d501-09df-4629-ae2c-03c12d1276a0` before the short harness timeout
  expired.
- On April 9, 2026 local `go run ./cmd/devtool verify default` hit the same
  family again in `TestE2E_Sync_DriveRemoveAndReAdd`, this time after the
  harness had already been widened to the shared 2-minute write-visibility
  helper. The underlying product claim for that test was durable state reuse
  across drive removal and re-add, but the harness was still asserting a
  stronger cross-command remote path readability guarantee by polling `stat
  /e2e-sync-readd-1775720725721617000/file1.txt` until timeout. The final
  request ID on the last failing `stat` was `c61afc1d-20ce-488c-8d3e-8b4bc037cb6f`.
- The same April 9, 2026 `go run ./cmd/devtool verify default` run then hit
  the same family one test earlier in `TestE2E_Sync_UploadOnly`: the sync pass
  itself succeeded, but the harness still treated `stat
  /e2e-sync-up-1775721401287802000/upload-test.txt` as the proof of success
  and timed out waiting for a follow-on by-path read that the product does not
  promise. The final request ID on the last failing `stat` was
  `fe8a7c70-ab69-44ed-9be3-c2ff83b05684`.
- On April 10, 2026 local `go run ./cmd/devtool verify default` reproduced the
  same family again in `TestE2E_RoundTrip/rm_permanent`, but this time the log
  proved the `put` command had already crossed its own visibility boundary:
  `put /onedrive-go-e2e-1775871160938777000/perm-test.txt` ended with a
  successful exact-path `GET .../root:/onedrive-go-e2e-1775871160938777000/perm-test.txt: = 200`
  and then the harness's separate follow-on `stat` on the same path kept
  returning `404 itemNotFound` for more than two minutes. Representative
  request IDs from that recurrence included `105db850-9f4d-41a6-8a60-cad58732c009`
  for the product-owned success read, followed by repeated harness failures
  such as `75777728-d7e4-4172-91ef-5a183c209999`,
  `a172eda8-3091-43e3-a20c-d355f14e93c8`, and
  `9ad0757a-d809-46cb-ae4f-cbff943f1bcf`.
- The same family recurred again later on April 10, 2026 during another local
  `go run ./cmd/devtool verify default`, this time one substep earlier in
  `TestE2E_RoundTrip/put`. The `put /onedrive-go-e2e-1775884436088088000/test.txt`
  command had already succeeded, but the harness's extra follow-on exact-path
  `stat /onedrive-go-e2e-1775884436088088000/test.txt` still timed out under
  the shared two-minute write-visibility window with final request ID
  `b5a70314-3791-4118-bc31-697c6af18177`, before later round-trip substeps even
  got a chance to run. That proved the `put` subtest was still asserting a
  stronger second read-after-write guarantee than the product command itself.
- On April 9, 2026 local `go run ./cmd/devtool verify default` reproduced the
  same family twice in sync-scope fixture setup. First,
  `TestE2E_Sync_SyncPathsExactFileDownloadsOnlySelectedRemoteFile` uploaded
  `/e2e-sync-scope-file-1775720244168811000/docs/other.txt`, but the
  follow-on exact-path `stat` still timed out under the 2-minute
  remote-write visibility window. Then
  `TestE2E_Sync_IgnoreMarkerRemovalReconcilesBlockedRemoteDownload` hit the
  inverse read-model lag: exact-path lookup for
  `/e2e-sync-marker-1775721144409607000` had already succeeded after `mkdir`,
  but repeated `ls /e2e-sync-marker-1775721144409607000` still returned 404
  while the child-folder setup was trying to prove `blocked` visibility.
- The same April 9, 2026 `go run ./cmd/devtool verify default` run also hit
  the product boundary directly in fast E2E `TestE2E_RoundTrip/rm_permanent`.
  Earlier substeps in that same round-trip had already created, listed, and
  partially cleaned up `/onedrive-go-e2e-1775753944282690000`, but the later
  `put /onedrive-go-e2e-1775753944282690000/perm-test.txt` still exhausted the
  older `WaitPathVisible()` budget resolving parent
  `/onedrive-go-e2e-1775753944282690000`. Representative late request IDs from
  that failing run were exact-path `54cd8b32-1efd-4927-8583-faa972d45e7a` and
  root-children `38ebde5b-9ff5-487a-89db-45b85f704e4b`.
- GitHub Actions PR `#456` `e2e` run on April 10, 2026 hit the same broader
  family in `TestE2E_Sync_IgnoreMarkerRemovalReconcilesBlockedRemoteDownload`:
  after `mkdir /e2e-sync-marker-1775870853588831314` had already produced a
  readable parent item ID
  `F1DA660E69BDEC82!s1905001a0a384fdeb1e03964b22f430d`, the immediate child
  create `POST /drives/f1da660e69bdec82/items/F1DA660E69BDEC82!s1905001a0a384fdeb1e03964b22f430d/children`
  still returned `404 itemNotFound` with request ID
  `c98813fc-6726-4d1f-ad89-a8a7da8522b3`. Cleanup immediately afterward also
  saw `/e2e-sync-marker-1775870853588831314` as missing by exact path with
  request ID `aa7dda79-1cad-4129-b6a6-6c1f37ad75bd`, even though the earlier
  setup had already used the same parent successfully. That extends the same
  post-mutation visibility family from follow-on read lag to immediate
  follow-on child-create instability.
- On April 12, 2026 local `go run ./cmd/devtool verify default` hit the same
  family earlier in `TestE2E_RoundTrip`: `mkdir
  /onedrive-go-e2e-1776012180531584000/subfolder` and the follow-on
  `put /onedrive-go-e2e-1776012180531584000/test.txt` both reported success,
  but the later round-trip assertions never regained a readable parent path.
  Repeated `ls /onedrive-go-e2e-1776012180531584000` and
  `stat /onedrive-go-e2e-1776012180531584000/test.txt` calls kept returning
  `404 itemNotFound` for more than two minutes, and later cleanup still saw
  the same parent as missing while `GET /items/root/children` returned an
  otherwise healthy root listing. Representative request IDs from that run
  included folder-list/read failures `a25ff48e-c887-4568-8a15-d7ba89e989f9`,
  `db5d96ce-646a-434e-b457-ad290b70675b`, and
  `820427af-adb4-4134-83e7-22d87e0c037a`.
- Later on April 12, 2026 another local `go run ./cmd/devtool verify default`
  hit the same family in `TestE2E_FileOps_RmFile`: `put
  /onedrive-go-e2e-rm-file-1776033974888448000/test.txt` had already reported
  success through the shared fixture-seed helper, but the immediate
  follow-on `rm /onedrive-go-e2e-rm-file-1776033974888448000/test.txt` still
  saw `GET .../root:/onedrive-go-e2e-rm-file-1776033974888448000/test.txt: = 404
  itemNotFound`, then `GET .../root:/onedrive-go-e2e-rm-file-1776033974888448000:/children = 404`,
  and finally a healthy root listing that still omitted the freshly written
  folder. Representative request IDs from that recurrence included
  `dedfd276-c419-44a8-b589-8f0b79ba10da`,
  `90d90008-00f9-47f7-bec4-aff991088c22`,
  `aa642558-d6d9-40c6-b50b-d61301f0cf37`, and
  `dd6863d6-e576-42dd-84ba-901a9808143f`.
Resolution / mitigation: CLI mutation flows now treat destination visibility as
a bounded driveops-owned convergence concern. `mkdir`, single-file `put`, and
`mv` wait for the destination path to become readable before reporting success,
and `put` also routes already-expected parent-path reads through the same
bounded visibility gate. Repo-owned E2E sync-upload visibility checks now use
the shared `waitForRemoteWriteVisible()` helper with
`remoteWritePropagationTimeout` instead of the older generic 30-second poll
when they are asserting follow-on remote readability after a successful write.
E2E fixture setup now accepts either exact-path `stat` or parent `ls`
visibility, so setup helpers stop depending on one specific Graph read model
being the first one to converge after a mutation.
The fast file-operation battery no longer chains unrelated assertions through
one long-lived `TestE2E_RoundTrip` folder. It now uses isolated
`TestE2E_FileOps_*` cases with fresh remote folders, so one transient
destination-visibility wobble cannot cascade into later `rm`, `get`, or
`status` assertions that are trying to prove something else.
`rm` now keeps the same bounded parent-read check for
non-root deletes, but once delete intent has already proved the target path is
gone it downgrades a pure `PathNotVisibleError` on that follow-on parent read
to a warning instead of reporting a false delete failure. For
`TestE2E_RoundTrip/rm_permanent`, the harness now uses that same `stat`-or-parent-`ls`
visibility helper before issuing the permanent delete instead of insisting on a
second exact-path `stat` after `put` already reported success. The generic
`TestE2E_RoundTrip/put` subtest now follows the same rule and does not add its
own second exact-path `stat`; later `ls_folder` and `stat` subtests still own
the end-to-end readability assertions for that round-trip. For
the isolated fast file-op delete cases, the harness now waits for the exact
target path to become stat-readable before it uses `rm` / `rm --permanent` as
the next assertion step, so the delete command no longer doubles as the first
exact-path visibility probe after a softened fixture seed. For
`TestE2E_Sync_DriveRemoveAndReAdd`, the harness now asserts the thing the test
actually claims: the durable `baseline` rows survive config removal and are
reused after drive re-add. It no longer treats follow-on remote path
readability as the proof for that state-preservation contract. The same rule
now applies to `TestE2E_Sync_UploadOnly`: immediate success is proven through
the durable baseline row written by the sync outcome boundary, not by a
separate follow-on remote `stat`. After the April 9 recurring `rm_permanent`
repro proved the product boundary itself could still exhaust too early,
`PathVisibilityPolicy()` was widened to keep its deterministic `32s` cap for
three capped sleeps, which yields about `95.75s` of deterministic wait before
request overhead and roughly a two-minute wall-clock budget before surfacing
`ErrPathNotVisible`.
Promoted docs: [graph-api-quirks.md#post-mutation-visibility-lag](graph-api-quirks.md#post-mutation-visibility-lag), [drive-transfers.md](../design/drive-transfers.md), [cli.md](../design/cli.md)

## LI-20260407-04: Shared-file preflight assumed only one configured recipient could open the raw link

First seen: 2026-04-07
Last seen: 2026-04-07
Area: fast E2E, shared-file fixture preflight
Suite / test: local `go run ./cmd/devtool verify default`, `TestE2E_FixturePreflight_Fast`
Classification: test bug
Status: fixed
Recurring: no
Summary: The shared-file fixture resolver still assumed the raw shared link had
exactly one configured recipient account. On April 7, 2026, the live fixture
could be opened by two configured accounts, both resolving to the same remote
file identity. The product behavior was fine; the fast preflight failed only
because the harness encoded stale one-recipient lore.
Evidence:
- Local `go run ./cmd/devtool verify default` on April 7, 2026 failed
  `TestE2E_FixturePreflight_Fast` with
  `shared-file fixture should resolve to exactly one configured recipient account, got 2 matches`.
- The failure came from `discoverSharedFileFixture()` after both configured
  recipient candidates succeeded against the same raw link; no product command
  under test had failed yet.
Resolution / mitigation: Shared-file fixture resolution is now identity-based.
The harness accepts multiple configured recipients when they all resolve to the
same remote drive/item identity, prefers a unique listing-backed match when one
exists, and otherwise chooses a stable candidate deterministically instead of
failing the suite on recipient-slot assumptions.
Promoted docs: [system.md](../design/system.md)

## LI-20260407-03: Exact delete-target path lookup lagged parent listing during repeated sibling deletes

First seen: 2026-04-07
Last seen: 2026-04-07
Area: `e2e_full`, CLI `rm`, delete-safety setup
Suite / test: local isolated repro of `TestE2E_Sync_DeleteSafetyThreshold`
Classification: graph quirk
Status: fixed
Recurring: no
Summary: During repeated sibling deletes, Graph could return `404 itemNotFound`
for the exact path lookup of a still-existing child before the delete route was
even attempted. The product already treated delete-by-ID `404`s as a
path-convergence problem, but the same test proved the path lookup itself could
lie too. For a path-authoritative delete intent, one-shot `GET by path` was not
trustworthy enough to decide the file was already gone.
Evidence:
- The local isolated repro on April 7, 2026 first created
  `/e2e-sync-bigdel-1775633571947871000/file-10.txt`, then later failed while
  running `rm /e2e-sync-bigdel-1775633571947871000/file-10.txt` during the
  remote setup loop.
- The failing command died in the initial resolve step with
  `resolving "/e2e-sync-bigdel-1775633571947871000/file-10.txt": graph: HTTP 404`
  after earlier sibling deletes had already succeeded for `file-01.txt`
  through `file-09.txt`.
- [graph-api-quirks.md](graph-api-quirks.md) already documented the adjacent
  delete-by-ID `404 itemNotFound` family; this incident extended the same live
  consistency gap to the exact path route used before deletion starts.
Resolution / mitigation: `driveops.Session` now owns a delete-specific
`ResolveDeleteTarget()` helper. It falls back from exact path `itemNotFound` to
the parent collection before declaring the target missing, and
`DeleteResolvedPath()` / `PermanentDeleteResolvedPath()` reuse that helper
during delete retry reconciliation. CLI `rm` now resolves its initial delete
target through that same driveops helper instead of a one-shot `ResolveItem()`.
Promoted docs: [drive-transfers.md](../design/drive-transfers.md), [graph-api-quirks.md](graph-api-quirks.md)

## LI-20260407-02: Keep-local conflict resolution used parent-route upload despite known item identity

First seen: 2026-04-07
Last seen: 2026-04-07
Area: `e2e_full`, conflict resolution, upload execution
Suite / test: `verify e2e-full`, `TestE2E_Sync_CreateCreateConflict_ResolveKeepLocal`
Classification: product bug
Status: fixed
Recurring: no
Summary: A full-suite keep-local resolution proved that sync upload execution still recreated files through the parent-path upload route even when the conflict record already carried the authoritative remote `itemID`. During the live run, the small-file overwrite first hit the simple-upload `404` fallback and then exhausted the parent-based `createUploadSession` retry budget on the same folder. The Graph inconsistency was already known, but the executor widened its exposure by ignoring the narrower item-ID overwrite route it already had available.
Evidence:
- Local `go run ./cmd/devtool verify e2e-full` on April 7, 2026 failed in `TestE2E_Sync_CreateCreateConflict_ResolveKeepLocal` while resolving `e2e-sync-cc-1775631264479623000/collision.txt` with `resolve local`.
- The child CLI log showed `resolve local` restoring the conflict copy, then attempting a parent-path simple upload followed by repeated `POST /drives/bd50cf43646e28e6/items/BD50CF43646E28E6!s8842f8751f7c491fbfd30ddaa2fc0031:/collision.txt:/createUploadSession` failures ending with request ID `9dce082f-97ae-4f6c-9cc2-69b650dcf4c1`.
- [graph-api-quirks.md](graph-api-quirks.md) already documented the broader fresh-parent `createUploadSession` `404 itemNotFound` family; this incident showed the executor still depended on that family in an overwrite flow that already had stable remote item identity.
Resolution / mitigation: `ExecuteUpload` now overwrites by item ID whenever the action carries a non-empty `ItemID`, using parent-path upload only for true create flows with no remote identity yet. `resolve local` therefore restores the local conflict copy and then overwrites the known remote item directly instead of recreating it through the parent route.
Promoted docs: [drive-transfers.md](../design/drive-transfers.md), [sync-execution.md](../design/sync-execution.md), [graph-api-quirks.md](graph-api-quirks.md)

## LI-20260407-01: Follow-on `put` lost a freshly visible parent path

First seen: 2026-04-07
Last seen: 2026-04-09
Area: `e2e_full`, CLI `put`, conflict setup
Suite / test: `verify e2e-full`, `TestE2E_Resolve_Both_PreservesConflictCopy`
Classification: graph quirk
Status: fixed
Recurring: yes
Summary: A full-suite conflict setup proved that a parent folder could be visible to one command and still fail the immediate next command's parent-path resolution. The test helper confirmed the folder path before starting the remote edit step, but the subsequent CLI `put` still died resolving the same parent path with `404 itemNotFound` instead of treating it as a bounded visibility-convergence gap.
Evidence:
- Local `go run ./cmd/devtool verify e2e-full` on April 7, 2026 failed in `TestE2E_Resolve_Both_PreservesConflictCopy` while uploading `/e2e-cli-keepboth-1775630146992732000/both.txt`.
- The child CLI log showed `GET /me` first stalling to a transient `504`, then succeeding on retry, followed by `GET /drives/bd50cf43646e28e6/root:/e2e-cli-keepboth-1775630146992732000:` returning `404 itemNotFound` with request ID `55b3980f-1c7c-4465-b09f-6683a0771f08`.
- [graph-api-quirks.md](graph-api-quirks.md) already records the broader path-visibility lag family for adjacent `mkdir` / `put` / `mv` flows; this incident showed the same family could hit pre-upload parent resolution too.
- Local `go run ./cmd/devtool verify default` on April 8, 2026 hit the same family in fast E2E `TestE2E_Sync_SyncPathsExactFileDownloadsOnlySelectedRemoteFile`: after `mkdir /.../docs` succeeded, the harness still timed out polling `stat /.../docs` before the follow-on helper-driven `put`, even though the product `put` command already owns parent convergence through `WaitPathVisible()`.
- Local `go run ./cmd/devtool verify default` on April 9, 2026 hit the same
  family one step later in that same fast E2E `sync_paths` setup: the second
  `put /e2e-sync-scope-file-1775722231752954000/docs/other.txt` still failed
  resolving parent `/.../docs` even after `WaitPathVisible()`, because the
  exact parent path kept returning `404 itemNotFound` and the old visibility
  gate did not yet confirm the parent by exact-name ancestor listing. The
  final request ID on the last failing parent lookup was
  `264a05bf-5833-4592-9f73-3e4d5c2293df`.
- Local `go run ./cmd/devtool verify e2e-full` on April 10, 2026 hit the same
  family in `TestE2E_EdgeCases`: after `mkdir /onedrive-go-e2e-edge-...`
  succeeded and earlier subtests had already used that folder, later `put`
  calls for `spaces_in_filename` and `concurrent_uploads` still exhausted the
  parent-visibility budget because the long-lived shared parent folder kept
  flapping out of the path/listing read model.
- Local `go run ./cmd/devtool verify e2e-full` on April 10, 2026 later hit the
  same family again in `TestE2E_Status_JSON_ConflictDetails` and
  `TestE2E_Status_History_ShowsResolvedStrategies`: the tests treated a
  completed `sync --upload-only` as proof that the freshly uploaded remote file
  path was ready for immediate direct `put` edits, but the next remote edit
  still occasionally failed resolving `/e2e-cli-.../*.txt` through the shared
  path model.
Resolution / mitigation: CLI `put` and folder upload bootstrap now resolve the parent path through `driveops.Session.WaitPathVisible()` instead of one-shot `ResolveItem()`. That visibility boundary now confirms settling paths through exact-name parent/ancestor listing when the direct path route still lies with `itemNotFound`, instead of trusting only repeated exact-path retries. The E2E upload helper no longer tries to prove fresh-parent stability in a separate preflight command before invoking `put`; it now relies on the product command's owned convergence boundary and waits only for the uploaded child path afterward. The full-suite edge-case E2E no longer shares one long-lived remote parent folder across independent subtests; each subtest now gets its own fresh remote folder so the test stays focused on file-name/upload behavior instead of incidental parent-path flapping over minutes of unrelated activity. The status/conflict E2Es now also wait for the upload-only baseline file path itself to become remotely visible before starting direct remote edits, so they no longer assume that sync completion alone means the next raw REST mutation can resolve the same path immediately. The repo-owned quirk summary now mirrors that split explicitly: fixture `put` retry classification records only command-window parent-create and parent-resolution lag, while the follow-on readiness wait owns `post_mutation_destination_visibility_lag`.
Promoted docs: [graph-api-quirks.md](graph-api-quirks.md), [drive-transfers.md](../design/drive-transfers.md)

## LI-20260406-01: Personal scoped delta not ready after path resolution

First seen: 2026-04-06
Last seen: 2026-04-06
Area: fast-e2e, sync scope bootstrap
Suite / test: `e2e`, `TestE2E_Sync_IgnoreMarkerRemovalReconcilesBlockedRemoteDownload`
Classification: graph quirk
Status: fixed
Recurring: no
Summary: A newly created folder in a personal drive could resolve successfully by path, but the immediate first folder-scoped delta request for that same folder still returned `404 itemNotFound`. This caused `sync_paths` bootstrap to fail even though the configured folder was real and readable.
Evidence:
- [graph-api-quirks.md](graph-api-quirks.md) documents the folder-scoped delta readiness lag and dates it to the fast E2E lane on April 6, 2026.
- The original live investigation showed that a newly created personal-drive scope could resolve successfully by path while the first folder-scoped delta call for that same scope still returned transient `404 itemNotFound`, which is why the long-term fix falls back to recursive enumeration for that scope instead of trusting immediate folder-scoped delta readiness.
- Merged fix: `74da628` (`fix: replay crash recovery in one-shot sync (#420)`), which included the scoped-delta fallback.
Resolution / mitigation: `sync_paths` primary-scope observation now mirrors scoped-root behavior and falls back to recursive enumeration when folder-scoped delta is temporarily unavailable for the already-resolved scope.
Promoted docs: [graph-api-quirks.md](graph-api-quirks.md)

## LI-20260405-05: One-shot crash recovery left durable work unreplayed

First seen: 2026-04-05
Last seen: 2026-04-05
Area: fast-e2e, sync recovery
Suite / test: `e2e`, `TestE2E_Sync_ReconcilesDurableRemoteMirrorTruthWithoutFreshDelta`
Classification: product bug
Status: fixed
Recurring: no
Summary: A live recovery investigation showed that one-shot sync had tied durable remote observation too closely to in-progress execution lanes. Once the delta token advanced, later runs could lose the ability to settle already-observed remote drift unless that work was restated through synthetic queue-style bridge rows.
Evidence:
- The original live recovery investigation exposed that replay depended on queue-style `remote_state` lifecycle rather than durable remote truth plus baseline reconciliation, which made one-shot recovery brittle and over-coupled to startup lane repair.
- Merged fix: `74da628` (`fix: replay crash recovery in one-shot sync (#420)`).
Resolution / mitigation: The sync engine now persists remote truth in `remote_state`, observes both sides in every mode, and recomputes reconciliation from `baseline + remote_state + local disk`. Directional runs can therefore defer forbidden-direction work without losing it, and later runs settle already-observed remote drift directly from durable truth instead of from crash-shaped queue lanes.
Promoted docs: [sync-store.md](../design/sync-store.md), [sync-engine.md](../design/sync-engine.md)

## LI-20260405-04: Fast E2E download-only tests assumed delta visibility too early

First seen: 2026-04-05
Last seen: 2026-04-10
Area: fast-e2e, download-only sync
Suite / test: `e2e`, `TestE2E_Sync_DownloadOnly`; later `TestE2E_Sync_SyncPathsExactFileDownloadsOnlySelectedRemoteFile`
Classification: graph quirk
Status: closed as test
Recurring: yes
Summary: The tests treated direct remote visibility or newly-unblocked remote state as proof that the next incremental download-only sync pass would converge immediately. In live CI that assumption was false: first-pass sync could still lag delta visibility or hit a documented transient download-metadata `404`, even though a later pass converged correctly.
Evidence:
- [sync_e2e_test.go](../../e2e/sync_e2e_test.go#L340) now explicitly waits for the local synced file after delta catches up.
- [sync_scope_e2e_test.go](../../e2e/sync_scope_e2e_test.go#L35) now uses the same eventual-convergence helper for exact-file `sync_paths` download coverage.
- [graph-api-quirks.md](graph-api-quirks.md) already documents delta endpoint consistency lag as a live behavior.
- Merged fix chain is included in `74da628` after the earlier test hardening commit on the same PR line.
- April 7, 2026 local `go run ./cmd/devtool verify default` reproduced the same symptom once in the fast E2E lane, while an immediate targeted rerun of `go test -tags=e2e ./e2e -run '^TestE2E_Sync_DownloadOnly$' -count=1` passed, consistent with intermittent delta visibility lag rather than a deterministic product regression.
- April 8, 2026 local `go run ./cmd/devtool verify e2e-full --classify-live-quirks` hit the same family in the classified fast-E2E pre-pass, and the targeted rerun passed immediately, confirming the scheduled/manual rerun path is now correctly scoped to this exact recurrence.
- April 9, 2026 local `go run ./cmd/devtool verify default` reproduced the same family in `TestE2E_Sync_SyncPathsExactFileDownloadsOnlySelectedRemoteFile`: direct `stat` calls showed `/e2e-sync-scope-file-.../docs/report.txt` and `/other.txt` as visible, but the immediate `sync --download-only --force` pass still saw `No changes detected` because the incremental scoped observation had not caught up yet.
- April 9, 2026 the same local `go run ./cmd/devtool verify default` later hit `TestE2E_Sync_IgnoreMarkerRemovalReconcilesBlockedRemoteDownload`: after removing `.odignore`, the immediate `sync --download-only --force` pass planned the download but the worker hit the documented transient item-by-ID download-metadata `404` family for `secret.txt`. A later sync pass was sufficient to converge, so the test's first-pass assumption was too strict.
- April 10, 2026 local `go run ./cmd/devtool verify default` reproduced the
  same family again in
  `TestE2E_Sync_SyncPathsExactFileDownloadsOnlySelectedRemoteFile`: after
  `mkdir /e2e-sync-scope-file-1775867945012522000/docs` and the helper-driven
  `put` steps completed, the eventual-convergence loop still timed out once
  because the immediate `sync --download-only --force` pass saw `No changes
  detected` while `GET /root:/.../docs/report.txt`, `GET /root:/.../docs`, and
  `GET /root:/...` all still returned `404 itemNotFound`. The delta page later
  returned only unrelated root items and one shortcut, with no events for the
  fresh scoped subtree.
- An immediate isolated rerun on April 10, 2026 passed:
  `go test -tags=e2e ./e2e -run TestE2E_Sync_SyncPathsExactFileDownloadsOnlySelectedRemoteFile -count=1`
  completed successfully in about 21 seconds, which confirmed the same
  intermittent delta-lag family rather than a deterministic product
  regression.
- The same April 10, 2026 `go run ./cmd/devtool verify default` run later hit
  `TestE2E_Sync_IgnoreMarkerRemovalReconcilesBlockedRemoteDownload` again:
  after removing `.odignore`, repeated `sync --download-only` passes kept
  reporting `No changes detected` while the root delta feed still omitted the
  newly unblocked scoped subtree. That test is also a scope-transition case,
  so it now uses the existing 3-minute `remoteScopeTransitionTimeout` instead
  of the shorter 90-second generic sync-convergence budget.
Resolution / mitigation: The fast E2E tests now wait for the real product outcome, the expected local sync result, instead of assuming the first pass after direct REST visibility or scope unblocking must succeed. Delta-sensitive live sync tests now reuse the same eventual-convergence helper pattern, and scheduled/manual `devtool verify e2e-full --classify-live-quirks` may rerun this exact test family once when the known delta-lag family recurs. Those same live waits now emit `timing-summary.json`, so recurring convergence gaps show up as measured windows rather than only as pass/fail noise.
Promoted docs: [graph-api-quirks.md](graph-api-quirks.md)

## LI-20260405-03: Websocket watch tests timed websocket assertions before the steady-state subtree was ready

First seen: 2026-04-05
Last seen: 2026-04-08
Area: websocket watch E2E harness
Suite / test: `e2e`, `TestE2E_SyncWatch_WebsocketStartupSmoke`; later `e2e_full`, `TestE2E_SyncWatch_WebsocketRemoteWakeAndRestart`
Classification: test bug
Status: fixed
Recurring: yes
Summary: The websocket harness originally treated an open socket connection as the readiness boundary, even though the product only starts honoring websocket-specific timing after bootstrap sync drains and the steady-state remote observer comes online. The original smoke failure and the later restart failure were both harness timing bugs, not websocket transport regressions.
Evidence:
- [socketio_e2e_test.go](../../e2e/socketio_e2e_test.go#L132) now documents the correct remote-observer-first boundary.
- [socketio_helpers_test.go](../../e2e/socketio_helpers_test.go#L87) contains the helper that waits for `observer_started(remote)` before websocket-specific timing.
- Merged fix: `52cef0f` (`fix: close W8 validation audit gaps (#415)`).
- On April 8, 2026 local `go run ./cmd/devtool verify e2e-full --classify-live-quirks` reproduced the same harness gap in `TestE2E_SyncWatch_WebsocketRemoteWakeAndRestart`: after daemon restart, the test waited only for `websocket_connected`, so the first post-restart wake could still be consumed by bootstrap catch-up before the steady-state remote observer was ready.
- On April 8, 2026 a later local `go run ./cmd/devtool verify e2e-full --classify-live-quirks` run still failed the same test even after the remote-observer fix, because the timed assertion also depended on creating the parent folder after daemon startup. The first post-mutation wake could legitimately reflect unrelated live-drive traffic or an incremental delta read that still had not observed the fresh parent subtree.
Resolution / mitigation: Websocket watch tests now wait for `observer_started(remote)` before starting websocket-specific timing on both initial startup and daemon restart paths, and the long full-suite wake/restart test seeds its remote subtree before daemon startup so the timed websocket assertion only covers steady-state remote file creation inside an already materialized subtree.
Promoted docs: [system.md](../design/system.md)

## LI-20260405-02: Stale root-level E2E artifacts inflated bootstrap and polluted live drives

First seen: 2026-04-05
Last seen: 2026-04-05
Area: fast-e2e, suite hygiene
Suite / test: `e2e` suite startup / fixture preflight
Classification: test bug
Status: fixed
Recurring: yes
Summary: Failed or interrupted live E2E runs left disposable `e2e-*` and `onedrive-go-e2e*` folders behind in the test drives. That cruft accumulated at drive root, polluted the test accounts, and made later bootstrap scans appear much slower than the fresh-suite case.
Evidence:
- [e2e_test.go](../../e2e/e2e_test.go#L22) defines the disposable artifact prefixes.
- [e2e_test.go](../../e2e/e2e_test.go#L95) now performs suite startup scrub against those root-level prefixes before the fast live battery begins.
- Merged fix: `52cef0f` (`fix: close W8 validation audit gaps (#415)`).
Resolution / mitigation: The live E2E suite now pre-scrubs only known disposable root-level artifacts and surfaces remote cleanup failures instead of silently ignoring them.
Promoted docs: [system.md](../design/system.md)

## LI-20260403-01: Live Graph metadata requests stalled before response headers

First seen: 2026-04-03
Last seen: 2026-04-05
Area: e2e_full, integration, metadata transport
Suite / test: scheduled `e2e_full` setup and `internal/graph` integration tests (`TestIntegration_Me`)
Classification: graph quirk
Status: mitigated
Recurring: yes
Summary: Ordinary metadata requests could connect successfully and then stall for tens of seconds before sending response headers. This first showed up in the scheduled full E2E battery during delete-safety setup, then recurred in GitHub Actions integration when a normal `GET /me` call stalled long enough to hit the old 30-second budget.
Evidence:
- [graph-api-quirks.md](graph-api-quirks.md#slowstalled-metadata-response-headers) records the incident family with dates April 3, 2026 and April 5, 2026.
- [internal/graph/integration_test.go](../../internal/graph/integration_test.go#L24) now keeps the live integration timeout above the observed GitHub runner tail latency.
Resolution / mitigation: Runtime policy moved away from client-wide `http.Client.Timeout` for metadata callers and uses connection-level header deadlines instead. The live integration budget was also raised to avoid misclassifying service/header stalls as product regressions.
Promoted docs: [graph-api-quirks.md](graph-api-quirks.md)
