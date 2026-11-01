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
immediately; you do not need to restart it.

`onedrive-go sync --confirm-deletes --drive <drive>` applies deletes the
running sync held for confirmation (the journal and `status` report them).
Stop the service first, since only one sync may run, and `--confirm-deletes`
is one-shot only; then start it again.

### Stopping

`systemctl --user stop onedrive-go` (or Ctrl-C when running in a terminal)
asks onedrive-go to finish in-flight work and exit cleanly; a second signal
exits immediately. A clean stop exits with status 0.

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
