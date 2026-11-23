# B3 report: control socket deadlines and honest client messages

## What changed
- `internal/synccontrol/protocol.go`: `ErrorBusy`, `StatusReloadInProgress`; owner waits `LoopAcceptTimeout` 5 s, `ReloadAnswerTimeout` 60 s, `ReplyWriteTimeout` 5 s (the client derives its timeout from them).
- `internal/multisync/control_socket.go`: `controlTimeouts` (injectable), `controlServerConfig`; `http.Server` read 10 s, header 5 s, idle 30 s, write 5 s, 16 KiB headers; `handleControlRequest` answers `busy` 503 / `reload_in_progress` 202; waiting handlers move their own write deadline; capture max 5 min (checked before ms->duration conversion); `reportControlServeEnd` sends an unexpected `Serve` end to the fatal channel.
- `orchestrator.go` (`controlTimeouts`, `controlListen` seam), `orchestrator_watch.go` (RunWatch owns `fatal`, returns its error), `orchestrator_run_once.go` (no fatal channel: warn only).
- `internal/cli/control_client.go`: reload timeout = owner waits + 2 s; probe state `controlOwnerStateUnresponsive` (busy or deadline); `controlOwnerDidNotAnswer`; `readControlErrorResponse`; `reload` returns the status; `postJSON` and the one-shot guard removed.
- `pause.go`: one message per outcome (`reportDaemonReloadError`); "applies when sync next starts" only where no watch will apply it.
- `status_runtime.go`, `status_snapshot.go`: stderr note when a sync holds the socket but its live state is missing.
- `status_perf.go`, `perf.go`, `drive_reset_sync_state.go`: handle the new probe state (reset still refuses).
- Docs: R-2.9.3 extended, new R-2.9.7 (sync.md); `sync-control-plane.md` (Implements, Verified By, RunWatch, Control Socket: Deadlines, fatal end); `cli.md` (Verified By, Control Socket client timeouts, status note, Pause/Resume outcome table).
- Tests: four `// Validates: R-2.10.5` corrected to R-2.9.3; existing call sites moved to the new server signature.

## Tests added (all seen failing first)
- `TestControlSocketServer_SetsReadIdleAndHeaderLimits`: server fields set; a 64 KiB header gets 431.
- `TestControlSocket_SlowBodyIsCutOff`: a half-sent body is cut off at the read timeout.
- `TestHandleControlRequest_LoopNotAcceptingAnswersBusy`: status, reload, stop get 503 `busy`.
- `TestHandleControlRequest_SlowReloadAnswersInProgress`: taken but unfinished reload gets 202.
- `TestHandleControlRequest_LongWaitOutlivesTheServerTimeouts`: a wait past the server read/write timeouts still answers (fails if the write-deadline move is removed; mutation checked).
- `TestStartControlSocketServer_UnexpectedServeEndIsFatal`, `TestRunWatch_ControlServerExitEndsTheWatch`: listener closed under Serve ends the watch with the error.
- `TestPerfCaptureOptions_RejectsTooLongDuration`: 5 min accepted, 5 min + 1 ms refused.
- `TestNotifyDaemon_BusySyncSaysItWillPickTheChangeUp` (busy/timeout on probe and on reload), `TestNotifyDaemon_ReloadInProgressSaysSo`.
- `TestRunStatusCommand_SaysWhenTheRunningSyncDidNotAnswer`; extended `TestProbeControlOwner_ClassifiesOutcomes`, `TestControlSocketClientReloadAndPostJSON` (202, 503, timeout budget), `TestControlSocketErrorClassification`.

## Checks
- `go test -race -count=1` on `./internal/multisync/...`, `./internal/synccontrol/...`, `./internal/cli/...`: pass.
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/...`: pass. gofumpt, goimports: clean.
- golangci-lint over the three trees: `0 issues.` (`B3-lint.log`).
- Live (branch binary, test account, temp HOME/XDG dirs, temp sync dir, `included_dirs` set to a folder that does not exist so nothing transfers):
  `status` -> "Status: Up to date, watching for changes"; `pause` -> "Paused <drive> until you run 'onedrive-go resume'." + "The running 'sync --watch' picked up the change."; `status` -> "Status: Paused"; `resume` -> "Drive <drive> resumed" + "picked up the change"; `status` -> watching again.
  Watch frozen with SIGSTOP: `status` -> "Note: the running sync did not answer in time, so its live state is not shown." (then the saved state); `pause` -> "The running 'sync --watch' is busy; it will pick up the change when it is free." After SIGCONT the watch applied the pause from its config file by itself (`status --json`: paused, no active runtime drive), so the note was true. SIGINT -> "Stopping..." / "Stopped.", exit 0.
  (A first attempt's SIGSTOP got the watch killed by the tool harness; rerun under `setsid`.)

## Deviations (behaviour, scope or interface)
- `reload_in_progress` is a `status` value with HTTP 202, not an error `code`: 202 is a success answer and the `{status, code, message}` error shape is for errors.
- A server-wide `WriteTimeout` (5 s) is set; handlers that wait move their own write deadline. The analysis set write deadlines per handler only; this also bounds answers that are ready at once.
- The analysis' premise that `ReadTimeout` would cut long handlers short is wrong (net/http clears the read deadline when the handler starts); handlers move only the write deadline.
- Owner waits are exported from `synccontrol` (interface addition) so the client timeout is derived, not duplicated.
- New probe state `controlOwnerStateUnresponsive`; a probe that times out now also prints the busy note. Probe timeout stays 2 s, so `status` against a busy watch waits 2 s, not the server's 5 s.
- Status or stop taken by the loop but unanswered within 5 s gets `busy` (cannot happen today: the loop answers them on receipt).
- `http.Server.ErrorLog` not set: left to B9, which routes the server's logging and panics through slog.

## Recommendations
- `sync` one-shot early refusal (`sync_flow.go`) could treat an unresponsive owner as an owner; today it announces, then fails on the lock.
- `status --perf` probes twice (runtime and perf overlays), so a busy watch costs 4 s; share one probe.
- `decodeControlJSONResponse` flattens decode errors to strings, so a body-read timeout on a 2xx answer reads as `internal_error`, not "did not answer".
- The closer goroutine in `startControlSocketServer` has no owner that waits for it (B8/B9).

## Unfinished: none. Packages for the reviewer: none beyond the three trees (no other package calls the changed APIs).
