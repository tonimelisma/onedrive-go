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
