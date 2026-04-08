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
| LI-20260406-01 | Personal scoped delta not ready after path resolution | fixed | graph quirk | 2026-04-06 | no |
| LI-20260405-05 | One-shot crash recovery left durable work unreplayed | fixed | product bug | 2026-04-05 | no |
| LI-20260405-04 | Fast E2E download-only assumed delta visibility too early | closed as test | graph quirk | 2026-04-07 | yes |
| LI-20260405-03 | Websocket smoke timed startup before remote observer readiness | closed as test | test bug | 2026-04-05 | no |
| LI-20260405-02 | Stale root-level E2E artifacts inflated bootstrap and polluted live drives | fixed | test bug | 2026-04-05 | yes |
| LI-20260403-01 | Live Graph metadata requests stalled before response headers | mitigated | graph quirk | 2026-04-05 | yes |

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
