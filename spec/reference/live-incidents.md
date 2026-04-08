# Live Incidents

This ledger records every investigated live CI / E2E / integration issue.

Use it as the exhaustive history. When the same live issue recurs, update the
existing entry instead of creating a duplicate. Behavior-shaping recurring
issues may also be summarized in curated reference docs such as
[graph-api-quirks.md](graph-api-quirks.md), but the ledger remains the source
of truth for what was seen, when it was seen, and how it was handled.

## Index

| Incident | Title | Status | Classification | Last seen | Recurring |
| --- | --- | --- | --- | --- | --- |
| LI-20260405-06 | `/me/drives` stayed `403 accessDenied` past the strict auth-preflight window | mitigated | graph quirk | 2026-04-08 | yes |
| LI-20260405-09 | Fresh parent folder rejected immediate `createUploadSession` | mitigated | graph quirk | 2026-04-07 | yes |
| LI-20260405-08 | Delete-by-ID returned `404 itemNotFound` after successful path lookup | mitigated | graph quirk | 2026-04-07 | yes |
| LI-20260405-07 | Destination path stayed unreadable after successful mutation | mitigated | graph quirk | 2026-04-08 | yes |
| LI-20260407-04 | Shared-file preflight assumed only one configured recipient could open the raw link | fixed | test bug | 2026-04-07 | no |
| LI-20260407-03 | Exact delete-target path lookup lagged parent listing during repeated sibling deletes | fixed | graph quirk | 2026-04-07 | no |
| LI-20260407-02 | Keep-local conflict resolution used parent-route upload despite known item identity | fixed | product bug | 2026-04-07 | no |
| LI-20260407-01 | Follow-on `put` lost a freshly visible parent path | fixed | graph quirk | 2026-04-07 | no |
| LI-20260406-01 | Personal scoped delta not ready after path resolution | fixed | graph quirk | 2026-04-06 | no |
| LI-20260405-05 | One-shot crash recovery left durable work unreplayed | fixed | product bug | 2026-04-05 | no |
| LI-20260405-04 | Fast E2E download-only assumed delta visibility too early | closed as test | graph quirk | 2026-04-07 | yes |
| LI-20260405-03 | Websocket smoke timed startup before remote observer readiness | closed as test | test bug | 2026-04-05 | no |
| LI-20260405-02 | Stale root-level E2E artifacts inflated bootstrap and polluted live drives | fixed | test bug | 2026-04-05 | yes |
| LI-20260403-01 | Live Graph metadata requests stalled before response headers | mitigated | graph quirk | 2026-04-05 | yes |

## LI-20260405-06: `/me/drives` stayed `403 accessDenied` past the strict auth-preflight window

First seen: 2026-04-05  
Last seen: 2026-04-08  
Area: scheduled/full live verification, auth preflight, drive catalog  
Suite / test: scheduled `e2e_full` `whoami`, local `verify default` `TestE2E_AuthPreflight_Fast`  
Classification: graph quirk  
Status: mitigated  
Recurring: yes  
Summary: Microsoft Graph can accept the token for `/me` while still returning
`403 accessDenied` for `/me/drives`. The production CLI already degrades
gracefully after its bounded retry budget, but the repo-owned strict auth
preflight can still fail a live suite when that inconsistency lasts longer than
the preflight poll window.  
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
- An immediate isolated rerun of the same preflight later passed for that
  account in about 1.4 seconds, confirming the failure window was transient.
Resolution / mitigation: `graph.Client.Drives()` now owns a narrow 5-attempt
retry for `/me/drives` `403 accessDenied`, and caller behavior above it treats
retry exhaustion as authenticated degraded discovery rather than auth failure.
The repo-owned auth preflight remains intentionally strict so CI fails early
with exact account, request ID, failed-call count, and elapsed-time evidence
when this consistency gap lasts longer than the test budget.  
Promoted docs: [graph-api-quirks.md](graph-api-quirks.md), [graph-client.md](../design/graph-client.md), [degraded-mode.md](../design/degraded-mode.md), [cli.md](../design/cli.md)

## LI-20260405-09: Fresh parent folder rejected immediate `createUploadSession`

First seen: 2026-04-05  
Last seen: 2026-04-08  
Area: `e2e_full`, upload-session creation after fresh parent visibility  
Suite / test: `verify e2e-full`, `TestE2E_SyncWatch_WebsocketStartupSmoke`; later `TestE2E_Sync_CreateCreateConflict_ResolveKeepLocal`  
Classification: graph quirk  
Status: mitigated  
Recurring: yes  
Summary: A folder can become readable by path lookup before Graph accepts the
immediate follow-on `createUploadSession` against that same fresh parent. The
API surface is internally inconsistent: the parent exists for reads, but upload
session creation still returns `404 itemNotFound`.  
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
Resolution / mitigation: `graph.Client.CreateUploadSession()` now owns a
bounded retry for the exact fresh-parent `404 itemNotFound` case, and flows
that already know the authoritative remote `itemID` avoid parent-route create
paths entirely by overwriting via item ID instead.  
Promoted docs: [graph-api-quirks.md](graph-api-quirks.md), [graph-client.md](../design/graph-client.md), [drive-transfers.md](../design/drive-transfers.md)

## LI-20260405-08: Delete-by-ID returned `404 itemNotFound` after successful path lookup

First seen: 2026-04-05  
Last seen: 2026-04-07  
Area: `e2e_full`, CLI `rm`, forced-overwrite cleanup  
Suite / test: `go test -tags='e2e e2e_full' ./e2e -run '^TestE2E_EdgeCases$|^TestE2E_Sync_BidirectionalMerge$'`; later isolated `TestE2E_Sync_BigDeleteProtection`  
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
- On April 7, 2026, the isolated repro for `TestE2E_Sync_BigDeleteProtection`
  extended the same family: after repeated sibling deletes, the initial exact
  path lookup for `file-10.txt` returned `404 itemNotFound` even though the
  file had been created successfully earlier in the same run.
Resolution / mitigation: `driveops.Session` now owns delete-specific recovery.
`ResolveDeleteTarget()` falls back from exact path lookup to the parent
collection before declaring a path missing, and
`DeleteResolvedPath()` / `PermanentDeleteResolvedPath()` retry delete intent
against the currently resolved target while treating a now-missing path as
success.  
Promoted docs: [graph-api-quirks.md](graph-api-quirks.md), [drive-transfers.md](../design/drive-transfers.md)

## LI-20260405-07: Destination path stayed unreadable after successful mutation

First seen: 2026-04-05  
Last seen: 2026-04-07  
Area: `e2e_full`, CLI mutation follow-on path reads  
Suite / test: `verify e2e-full`, `TestE2E_Sync_EditEditConflict_ResolveKeepRemote`; later `TestE2E_Sync_BidirectionalMerge` and `TestE2E_Conflicts_ResolveKeepBoth`  
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
- On April 7, 2026, `TestE2E_Conflicts_ResolveKeepBoth` hit the same broader
  family earlier in the flow when a freshly visible parent path still returned
  `404 itemNotFound` to the next `put`, which is tracked separately in
  `LI-20260407-01`.
Resolution / mitigation: CLI mutation flows now treat destination visibility as
a bounded driveops-owned convergence concern. `mkdir`, single-file `put`, and
`mv` wait for the destination path to become readable before reporting success,
and `put` also routes already-expected parent-path reads through the same
bounded visibility gate. Repo-owned E2E sync-upload visibility checks now use
the shared `waitForRemoteWriteVisible()` helper with
`remoteWritePropagationTimeout` instead of the older generic 30-second poll
when they are asserting follow-on remote readability after a successful write.  
Promoted docs: [graph-api-quirks.md](graph-api-quirks.md), [drive-transfers.md](../design/drive-transfers.md), [cli.md](../design/cli.md)

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
Area: `e2e_full`, CLI `rm`, big-delete setup  
Suite / test: local isolated repro of `TestE2E_Sync_BigDeleteProtection`  
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
- Local `go run ./cmd/devtool verify e2e-full` on April 7, 2026 failed in `TestE2E_Sync_CreateCreateConflict_ResolveKeepLocal` while resolving `e2e-sync-cc-1775631264479623000/collision.txt` with `--keep-local`.
- The child CLI log showed `conflicts resolve` restoring the conflict copy, then attempting a parent-path simple upload followed by repeated `POST /drives/bd50cf43646e28e6/items/BD50CF43646E28E6!s8842f8751f7c491fbfd30ddaa2fc0031:/collision.txt:/createUploadSession` failures ending with request ID `9dce082f-97ae-4f6c-9cc2-69b650dcf4c1`.
- [graph-api-quirks.md](graph-api-quirks.md) already documented the broader fresh-parent `createUploadSession` `404 itemNotFound` family; this incident showed the executor still depended on that family in an overwrite flow that already had stable remote item identity.
Resolution / mitigation: `ExecuteUpload` now overwrites by item ID whenever the action carries a non-empty `ItemID`, using parent-path upload only for true create flows with no remote identity yet. `conflicts resolve --keep-local` therefore restores the local conflict copy and then overwrites the known remote item directly instead of recreating it through the parent route.  
Promoted docs: [drive-transfers.md](../design/drive-transfers.md), [sync-execution.md](../design/sync-execution.md), [graph-api-quirks.md](graph-api-quirks.md)

## LI-20260407-01: Follow-on `put` lost a freshly visible parent path

First seen: 2026-04-07  
Last seen: 2026-04-07  
Area: `e2e_full`, CLI `put`, conflict setup  
Suite / test: `verify e2e-full`, `TestE2E_Conflicts_ResolveKeepBoth`  
Classification: graph quirk  
Status: fixed  
Recurring: no  
Summary: A full-suite conflict setup proved that a parent folder could be visible to one command and still fail the immediate next command's parent-path resolution. The test helper confirmed the folder path before starting the remote edit step, but the subsequent CLI `put` still died resolving the same parent path with `404 itemNotFound` instead of treating it as a bounded visibility-convergence gap.  
Evidence:
- Local `go run ./cmd/devtool verify e2e-full` on April 7, 2026 failed in `TestE2E_Conflicts_ResolveKeepBoth` while uploading `/e2e-cli-keepboth-1775630146992732000/both.txt`.
- The child CLI log showed `GET /me` first stalling to a transient `504`, then succeeding on retry, followed by `GET /drives/bd50cf43646e28e6/root:/e2e-cli-keepboth-1775630146992732000:` returning `404 itemNotFound` with request ID `55b3980f-1c7c-4465-b09f-6683a0771f08`.
- [graph-api-quirks.md](graph-api-quirks.md) already records the broader path-visibility lag family for adjacent `mkdir` / `put` / `mv` flows; this incident showed the same family could hit pre-upload parent resolution too.
Resolution / mitigation: CLI `put` and folder upload bootstrap now resolve the parent path through `driveops.Session.WaitPathVisible()` instead of one-shot `ResolveItem()`. That keeps parent-path convergence under the same bounded driveops authority already used for destination visibility after successful mutations.  
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
- [test-assurance-audit.md](/Users/tonimelisma/Development/onedrive-go-live-incident-ledger/spec/reviews/test-assurance-audit.md#L783) records the live failure and the resulting production fallback.
- Merged fix: `74da628` (`fix: replay crash recovery in one-shot sync (#420)`), which included the scoped-delta fallback.
Resolution / mitigation: `sync_paths` primary-scope observation now mirrors scoped-root behavior and falls back to recursive enumeration when folder-scoped delta is temporarily unavailable for the already-resolved scope.  
Promoted docs: [graph-api-quirks.md](graph-api-quirks.md)

## LI-20260405-05: One-shot crash recovery left durable work unreplayed

First seen: 2026-04-05  
Last seen: 2026-04-05  
Area: fast-e2e, sync recovery  
Suite / test: `e2e`, `TestE2E_Sync_CrashRecovery_ReplaysDurableInProgressRows`  
Classification: product bug  
Status: fixed  
Recurring: no  
Summary: A live crash-recovery pass showed that one-shot sync created durable retry bridge rows for interrupted work but did not actually replay them on that same invocation. The live investigation then exposed two related bugs in the same lane: delete-side bridge rows were typed as remote deletes instead of local deletes, and interrupted downloads could still no-op when the baseline said the file was already synced.  
Evidence:
- [test-assurance-audit.md](/Users/tonimelisma/Development/onedrive-go-live-incident-ledger/spec/reviews/test-assurance-audit.md#L780) records the live crash-recovery investigation and the three production gaps it exposed.
- Merged fix: `74da628` (`fix: replay crash recovery in one-shot sync (#420)`).
Resolution / mitigation: One-shot startup now consumes due retry rows immediately, preserves delete replay as `ActionLocalDelete`, and carries an explicit forced-download hint through planning so missing local files are redownloaded even without a fresh delta event.  
Promoted docs: [test-assurance-audit.md](/Users/tonimelisma/Development/onedrive-go-live-incident-ledger/spec/reviews/test-assurance-audit.md)

## LI-20260405-04: Fast E2E download-only assumed delta visibility too early

First seen: 2026-04-05  
Last seen: 2026-04-07  
Area: fast-e2e, download-only sync  
Suite / test: `e2e`, `TestE2E_Sync_DownloadOnly`  
Classification: graph quirk  
Status: closed as test  
Recurring: yes  
Summary: The test treated successful direct REST visibility of a newly uploaded remote file as proof that the next root-delta sync pass would also see it immediately. In live CI that assumption was false: direct path/stat visibility arrived first, while root delta still lagged.  
Evidence:
- [sync_e2e_test.go](/Users/tonimelisma/Development/onedrive-go-live-incident-ledger/e2e/sync_e2e_test.go#L340) now explicitly waits for the local synced file after delta catches up.
- [graph-api-quirks.md](graph-api-quirks.md) already documents delta endpoint consistency lag as a live behavior.
- Merged fix chain is included in `74da628` after the earlier test hardening commit on the same PR line.
- April 7, 2026 local `go run ./cmd/devtool verify default` reproduced the same symptom once in the fast E2E lane, while an immediate targeted rerun of `go test -tags=e2e ./e2e -run '^TestE2E_Sync_DownloadOnly$' -count=1` passed, consistent with intermittent delta visibility lag rather than a deterministic product regression.
Resolution / mitigation: The fast E2E test now waits for the real product outcome, the downloaded local file with the expected content, instead of assuming first-pass delta visibility after a direct REST read succeeds.  
Promoted docs: [graph-api-quirks.md](graph-api-quirks.md)

## LI-20260405-03: Websocket smoke timed startup before remote observer readiness

First seen: 2026-04-05  
Last seen: 2026-04-05  
Area: fast-e2e, websocket watch smoke  
Suite / test: `e2e`, `TestE2E_SyncWatch_WebsocketStartupSmoke`  
Classification: test bug  
Status: closed as test  
Recurring: no  
Summary: The websocket smoke test originally measured websocket startup from daemon launch, even though the product intentionally performs bootstrap sync first and only starts the websocket wake source after the steady-state remote observer comes online. The failure looked like a slow websocket connection, but the real issue was the test’s readiness boundary.  
Evidence:
- [socketio_e2e_test.go](/Users/tonimelisma/Development/onedrive-go-live-incident-ledger/e2e/socketio_e2e_test.go#L132) now documents the correct remote-observer-first boundary.
- [socketio_helpers_test.go](/Users/tonimelisma/Development/onedrive-go-live-incident-ledger/e2e/socketio_helpers_test.go#L87) contains the helper that waits for `observer_started(remote)` before websocket-specific timing.
- Merged fix: `52cef0f` (`fix: close W8 validation audit gaps (#415)`).
Resolution / mitigation: The smoke now waits for `observer_started(remote)` before starting its websocket-specific timeout and failure classification.  
Promoted docs: none

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
- [e2e_test.go](/Users/tonimelisma/Development/onedrive-go-live-incident-ledger/e2e/e2e_test.go#L22) defines the disposable artifact prefixes.
- [e2e_test.go](/Users/tonimelisma/Development/onedrive-go-live-incident-ledger/e2e/e2e_test.go#L95) now performs suite startup scrub against those root-level prefixes before the fast live battery begins.
- Merged fix: `52cef0f` (`fix: close W8 validation audit gaps (#415)`).
Resolution / mitigation: The live E2E suite now pre-scrubs only known disposable root-level artifacts and surfaces remote cleanup failures instead of silently ignoring them.  
Promoted docs: none

## LI-20260403-01: Live Graph metadata requests stalled before response headers

First seen: 2026-04-03  
Last seen: 2026-04-05  
Area: e2e_full, integration, metadata transport  
Suite / test: scheduled `e2e_full` setup and `internal/graph` integration tests (`TestIntegration_Me`)  
Classification: graph quirk  
Status: mitigated  
Recurring: yes  
Summary: Ordinary metadata requests could connect successfully and then stall for tens of seconds before sending response headers. This first showed up in the scheduled full E2E battery during big-delete setup, then recurred in GitHub Actions integration when a normal `GET /me` call stalled long enough to hit the old 30-second budget.  
Evidence:
- [graph-api-quirks.md](graph-api-quirks.md#slowstalled-metadata-response-headers) records the incident family with dates April 3, 2026 and April 5, 2026.
- [internal/graph/integration_test.go](/Users/tonimelisma/Development/onedrive-go-live-incident-ledger/internal/graph/integration_test.go#L24) now keeps the live integration timeout above the observed GitHub runner tail latency.
Resolution / mitigation: Runtime policy moved away from client-wide `http.Client.Timeout` for metadata callers and uses connection-level header deadlines instead. The live integration budget was also raised to avoid misclassifying service/header stalls as product regressions.  
Promoted docs: [graph-api-quirks.md](graph-api-quirks.md)
