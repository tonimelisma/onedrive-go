# B3 Definition of Ready: control socket deadlines and honest client messages

Worktree `~/src/onedrive-go-wt/control-socket-deadlines`, branch
`fix/control-socket-deadlines`, base `main` at `06a217d7`.

1. PASS **Task intent**: every wait on the watch daemon's control socket is
   bounded (header, whole request, idle connection, reply write, the loop
   taking a command, an accepted reload finishing, a perf capture), each bound
   has an honest answer (`busy` 503, `reload_in_progress` 202,
   `invalid_request` for a capture over 5 minutes), a control server that
   stops ends the watch with exit 1 instead of one warning, and `pause`,
   `resume`, `drive add`, `login` and `status` say what actually happened.
2. PASS **Non-goals**: no change to `internal/sync` engine internals, the
   watchdog or heartbeat (B10, B11), watch runner stops (B8), panic
   boundaries and the server's `ErrorLog` (B9), the config lock (#870). Status
   keeps going through the watch loop (no direct status answer). No retry or
   restart of a failed listener: the watch ends (D15).
3. PASS **Governing docs**: `spec/design/sync-control-plane.md` (GOVERNS
   `internal/multisync/*.go`, `internal/synccontrol/*.go`; "### Control
   Socket", "## Verified By"), `spec/design/cli.md` (GOVERNS
   `internal/cli/*.go`; "## Control Socket", "## Pause / Resume"),
   `spec/requirements/sync.md` R-2.9.1 to R-2.9.6, R-2.3.3,
   `spec/requirements/non-functional.md` R-6.6.16, analysis
   `02-daemon-robustness.md` section 7 (lines 658-741), item 2 of the same
   analysis for the shape of the fatal channel (grep only).
4. PASS **Authority owner**: wire facts (codes, statuses, the owner's answer
   deadlines the client derives its timeouts from) in `internal/synccontrol`;
   server lifecycle, deadlines and the fatal report in
   `internal/multisync/control_socket.go`; the fatal channel is created and
   read by `RunWatch` (`orchestrator_watch.go`), the single owner of the watch
   loop; client transport and timeouts in `internal/cli/control_client.go`;
   user-facing messages in `internal/cli/pause.go` and
   `internal/cli/status_runtime.go` / `status_snapshot.go`.
5. PASS **Code scope**: `internal/synccontrol/protocol.go`;
   `internal/multisync/control_socket.go`, `orchestrator.go` (injectable
   timeouts and listener seam), `orchestrator_watch.go`,
   `orchestrator_run_once.go` (new call signature); `internal/cli/control_client.go`,
   `pause.go`, `status_runtime.go`, `status_snapshot.go`; tests beside them;
   the four docs above. Not touched: `internal/sync`, the main checkout,
   `~/sync-test-root`, `/srv`.
6. PASS **Regression plan** (tests written first, each seen failing):
   `internal/multisync/control_socket_hardening_test.go`:
   `TestControlSocketServer_SetsReadIdleAndHeaderLimits`,
   `TestControlSocket_SlowBodyIsCutOff`,
   `TestHandleControlRequest_LoopNotAcceptingAnswersBusy`,
   `TestHandleControlRequest_SlowReloadAnswersInProgress`,
   `TestHandleControlRequest_LongWaitOutlivesTheServerTimeouts`,
   `TestStartControlSocketServer_UnexpectedServeEndIsFatal`;
   `perf_control_test.go`: `TestPerfCaptureOptions_RejectsTooLongDuration`;
   `orchestrator_test.go`: `TestRunWatch_ControlServerExitEndsTheWatch`;
   `internal/cli/control_socket_semantics_test.go`:
   `TestNotifyDaemon_BusySyncSaysItWillPickTheChangeUp`,
   `TestNotifyDaemon_ReloadInProgressSaysSo`, extended
   `TestProbeControlOwner_ClassifiesOutcomes`;
   `control_client_additional_test.go`: extended
   `TestControlSocketClientReloadAndPostJSON` (202, 503, timeout budget);
   `status_runtime_test.go`: `TestRunStatusCommand_SaysWhenTheRunningSyncDidNotAnswer`.
   All deadline tests inject millisecond durations or a short parent context.
7. PASS **Verification plan**: `go test -race -count=1 -timeout 15m` on
   `./internal/multisync/...`, `./internal/synccontrol/...`,
   `./internal/cli/...`; `go build ./...`; `go vet ./...`; one targeted
   golangci-lint run over the three trees after checking `pgrep` for another
   lint; gofumpt and goimports on touched files; a live check with a binary
   built from the branch against the test account (temporary config and sync
   dir, synthetic data): `sync --watch`, then `pause`, `resume`, `status`,
   then SIGINT. No `devtool verify` (the reviewer runs it).
8. PASS **Data safety**: the change touches control-plane answers and the
   watch's exit path only; no local or remote file operation, no state DB
   write. Ending the watch on a dead control server uses the existing return
   path (runners cancelled, engines closed, socket and lock released). The
   live check uses a temporary sync dir and config, never `<production sync folder>` or
   `~/sync-test-root`'s running client.
9. PASS **Secret and credential safety**: no token, account name or drive ID
   in code, tests, docs, commit or report. The live check reads the test
   account's existing token bundle through a temporary config; its output is
   recorded in the report with account names and ids removed.
10. PASS **Live provider and destructive operations**: the live check runs
    a watch against the test account's synthetic data in a temporary sync
    dir; `pause`/`resume` edit only that temporary config. No reset, no
    release artifact, no history rewrite, no dependency change. Recovery:
    delete the temporary directory.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD
    start stage has already been run in this worktree; no carryover threads or
    nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does `ReadTimeout` on `http.Server` cut a long handler short?
      First assumption: yes, through net/http's background read. **Refreshed
      during implementation**: a mutation check showed it does not;
      `startBackgroundRead` clears the read deadline when the handler starts
      (`net/http/server.go`, go1.26.11), so only the server-wide
      `WriteTimeout` has to be moved by a handler that waits.
      Validation step: `TestHandleControlRequest_LongWaitOutlivesTheServerTimeouts`
      fails when the write-deadline move is removed (seen).
      Owner document/code path affected if wrong: `control_socket.go`
      `extendControlWriteDeadline`, `sync-control-plane.md` Control Socket.
    - Question: is `reload_in_progress` an error code or a status? Current
      assumption: a status (`MutationResponse.Status`), since 202 is a
      success-class answer and the `{status, code, message}` error shape is
      for errors; recorded as a deviation from the analysis' wording.
      Validation step: client tests decode it as a status. Owner:
      `internal/synccontrol/protocol.go`, R-2.9.3.
    - Question: what does a probe that times out mean? Current assumption: a
      busy watch loop, because only a watch owner routes `status` through its
      loop; a one-shot owner answers status in the handler. Validation step:
      code read of `handleOneShotControlRequest`. Owner: `internal/cli/pause.go`.
    - Question: does a `RunWatch` error exit 1? Current assumption: yes,
      `commandExitCode` maps any error that is not usage, sign-in, config or
      a user stop to 1. Validation step: code read of `failure_class.go`;
      `TestCommandExitCode`. Owner: `internal/cli/failure_class.go`.
