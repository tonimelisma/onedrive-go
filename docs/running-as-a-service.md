# Running onedrive-go as a service

`onedrive-go sync --watch` keeps every configured, unpaused drive in sync
until it is stopped. On Linux, run it as a systemd user service so it starts
at login (or at boot) and restarts after failures.

## Linux: systemd user unit

The repo ships an example unit,
[`contrib/systemd/onedrive-go.service`](../contrib/systemd/onedrive-go.service).
It expects the binary at `~/.local/bin/onedrive-go`; edit `ExecStart` if yours
is elsewhere.

1. Sign in first. The service cannot sign in for you:

   ```bash
   onedrive-go login
   onedrive-go sync --dry-run     # optional: check what the first sync will do
   ```

2. Install and start the unit:

   ```bash
   mkdir -p ~/.config/systemd/user
   cp contrib/systemd/onedrive-go.service ~/.config/systemd/user/
   systemctl --user daemon-reload
   systemctl --user enable --now onedrive-go
   ```

3. To keep syncing while you are logged out, and to start at boot rather than
   at login:

   ```bash
   loginctl enable-linger "$USER"
   ```

### What the unit does

```ini
[Service]
Type=notify
ExecStart=%h/.local/bin/onedrive-go sync --watch
ExecReload=/bin/kill -HUP $MAINPID
TimeoutStartSec=5min
Restart=on-failure
RestartSec=30s
RestartPreventExitStatus=77 78
WatchdogSec=2min
```

- **`Type=notify`**: onedrive-go tells systemd it is ready once every drive
  that could start is watching, and keeps a one-line status ("Watching
  ~/OneDrive", or which drive needs sign-in) that `systemctl status` shows.
- **`ExecReload`**: `systemctl --user reload onedrive-go` sends `SIGHUP`, which
  re-reads the config at once. Edits to `config.toml` are also picked up on
  their own within a few seconds, so a reload is only needed to apply them
  immediately.
- **`TimeoutStartSec=5min`**: startup checks each drive with OneDrive before
  reporting ready, which can take a while on a slow network.
- **`RestartPreventExitStatus=77 78`**: exit 77 means sign-in is required and
  78 means the configuration is invalid. Restarting cannot fix either, so
  systemd stops trying. Fix the cause (`onedrive-go login`, or edit
  `config.toml`), then `systemctl --user restart onedrive-go`.
- **`WatchdogSec=2min`**: onedrive-go sends keepalives at half this interval;
  if it hangs, systemd restarts it.

There is no `network-online.target` dependency: user units cannot order
against it, and onedrive-go waits out an offline network by itself.

`sync` refuses to run as root unless `--allow-root` is given (it would leave
root-owned files in your folder), and exits with status 2. Run it as a user
unit, or set `User=` in a system unit.

### Day-to-day commands

```bash
systemctl --user status onedrive-go               # running? what is it watching?
journalctl --user -u onedrive-go -f               # follow the log
journalctl --user -u onedrive-go -p warning       # warnings and errors only
systemctl --user reload onedrive-go               # apply config edits now
onedrive-go status                                # drives, issues, what needs you
onedrive-go pause --drive personal:user@example.com 2h   # pause one drive for 2 hours
onedrive-go resume --drive personal:user@example.com
```

Under the journal every line carries a priority, so `-p warning` filters
reliably. `pause` and `resume` take effect in the running service
immediately; you do not need to restart it. Pausing every drive does not stop
the service: it stays up, `systemctl status` shows "All drives are paused;
run 'onedrive-go resume' to continue", and syncing starts again at `resume`
(or when a timed pause expires). The same goes for starting the service
while every drive is paused.

`onedrive-go sync --confirm-deletes --drive <drive>` applies deletes the
running sync held for confirmation (the journal and `status` report them).
Stop the service first, since only one sync may run, and `--confirm-deletes`
is one-shot only; then start it again.

### Stopping

`systemctl --user stop onedrive-go` (or Ctrl-C when running in a terminal)
asks onedrive-go to stop: transfers in progress are canceled, work that
already finished is recorded, and it exits; a second signal exits
immediately. A clean stop exits with status 0.

onedrive-go never waits without limit for a drive to stop. A drive that has
not stopped 45 seconds after it was told to (on a stop, a reload, or a
pause), or whose sync state has not closed 15 seconds after that, is logged
as an error, and onedrive-go exits with status 1. A one-shot `sync` (from
cron or a timer) whose drive's sync state does not close within those 15
seconds exits 1 the same way, after reporting the pass. The unit's
`Restart=on-failure` then starts a fresh process, which picks up where the
stuck one left off. The 45 seconds fit inside systemd's default stop timeout
of 90 seconds, so the unit needs no `TimeoutStopSec=`. If it happens, report
the journal line starting "Syncing ... did not stop when told to" together
with the matching entry in onedrive-go's log file, which carries the stack of
every goroutine.

### A crash that repeats on every start

A bug that makes onedrive-go panic outside one drive's sync is logged as an
error with its stack (in the journal, and in onedrive-go's log file when one
is configured), and onedrive-go exits with status 1. A panic inside one
drive's sync only stops that drive, which restarts on its own. After an exit,
`Restart=on-failure` starts the service again 30 seconds later
(`RestartSec=30s`). If the bug is hit on every start, that is a restart every
30 seconds, each one logged with its stack, until you stop the unit:
systemd's default start limit (`DefaultStartLimitIntervalSec=10s` and
`DefaultStartLimitBurst=5`, see `systemd-system.conf(5)`; user units use the
same defaults) never trips at that pace. The example unit keeps retrying on
purpose, because a sync client that gives up stays down until someone
notices. To make it give up instead, add a start limit to its `[Unit]`
section with an interval longer than the burst times `RestartSec`:

```ini
[Unit]
StartLimitIntervalSec=10min
StartLimitBurst=5
```

systemd then stops after five starts within ten minutes (five restarts take
about two and a half minutes). Once the cause is fixed,
`systemctl --user reset-failed onedrive-go` and
`systemctl --user start onedrive-go` start it again. Report the journal lines
starting "Internal error", with the stack below them.

## macOS

No launchd example is provided yet, and there is no `service install`
command. Run `onedrive-go sync --watch` in a terminal (or a terminal
multiplexer such as tmux) for now.

## Running one-shot syncs from cron or a timer

`onedrive-go sync` (without `--watch`) syncs once and exits, so it can also be
run from cron or a systemd timer. Only one sync runs at a time per
configuration: a scheduled run that starts while another is still running
exits with an error instead of running twice. Branch on the
[exit code](scripting.md#exit-codes) rather than on output text.

A one-shot sync exits 0 only when it left nothing unsynced. It exits 1 when a
file stays unsynced: a transfer failed, work is waiting to be retried, or a
condition `onedrive-go status` lists keeps a file from syncing (a name
OneDrive does not allow, a write into a read-only shared folder, a folder
that cannot be read). The run after a transient failure usually finishes the
work and exits 0 again; a condition that needs you makes every run exit 1
until you deal with it, so a timer that alerts on failure keeps alerting
while `status` lists it. Work `--upload-only` or `--download-only` holds
back, deletes held for `--confirm-deletes`, and conflicts resolved by keeping
both versions do not fail the run.
