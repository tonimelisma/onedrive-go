# CLI Output Guidelines

How onedrive-go should talk to people, scripts and service managers. Linux has
no official console HIG; this distils the documents that together act as one,
and applies them to a sync client that runs in four very different settings.

Sources: [clig.dev](https://clig.dev/) (interactive CLI behaviour),
POSIX Utility Syntax Guidelines and GNU Coding Standards (arguments, `--help`,
`--version`), [no-color.org](https://no-color.org/),
[daemon(7)](https://www.freedesktop.org/software/systemd/man/latest/daemon.html),
[sd-daemon(3)](https://www.freedesktop.org/software/systemd/man/latest/sd-daemon.html),
[sd_notify(3)](https://www.freedesktop.org/software/systemd/man/latest/sd_notify.html),
`systemd.exec(5)` (`$JOURNAL_STREAM`),
[12-factor logs](https://12factor.net/logs), and the XDG Base Directory spec.

See [output-journeys-vs-abraunegg.md](output-journeys-vs-abraunegg.md) for how
the current output measures against these rules, journey by journey.

## 1. Every journey must answer three questions

At any moment a person looking at the output must be able to tell:

1. **What is it doing right now?** Startup, scanning, transferring, waiting,
   retrying, idle. Silence is only acceptable when the answer is "nothing, and
   all is well" and the run is unattended.
2. **What did it just do?** Which files moved, in which direction. A count
   without names ("Uploads: 1", "Remote deletes: 1") answers neither "was that
   my file?" nor "did it delete the right thing?". For a sync client, a
   deletion that isn't named is a trust failure.
3. **What, if anything, must I do?** Every error and warning states what
   happened, why, and the concrete next step (the exact command, with the
   account filled in). A warning with no possible user action is a DEBUG line.

## 2. Streams

| Stream | Carries | Never carries |
|---|---|---|
| stdout | The command's result: listings, `status`, `--json` documents, and for `sync` its activity (one line per file) and summary | Warnings, errors, logs |
| stderr | Warnings, errors, transient progress, log lines | Data a script must parse |
| Log file / journal | Durable diagnostic history | Anything the user needs to see now and nowhere else |

- **For `sync`, the activity report is the result.** `sync` has no other
  output, so the per-file lines and the summary go to stdout (as rsync and the
  D client do). `sync 2>/dev/null` then shows what happened, `sync >/dev/null`
  shows only problems.
- **One format per stream.** A stream is either human text or JSON lines,
  decided once at startup. Never interleave `key=value` lines, JSON objects
  and prose on the same stream.
- **Print each error once.** The command layer reports; lower layers return.
- **`--json` makes stdout a single valid JSON document** (or JSON lines for
  streaming commands) and nothing else.

## 3. Detecting the setting

Check each stream separately, once, at startup:

| Signal | Means |
|---|---|
| stderr is a TTY | A person is watching: progress and color allowed |
| stdout is a TTY | Human-formatted results; otherwise stable plain or `--json` |
| stdin is a TTY | Prompts allowed; otherwise never prompt, fail with the flag to pass |
| `$JOURNAL_STREAM` matches stderr's `dev:ino` | Running under systemd with stderr in the journal |
| `NO_COLOR` non-empty, `TERM=dumb`, `--no-color` | No color, regardless of TTY |
| `$NOTIFY_SOCKET` set | systemd wants readiness and status messages |

## 4. Per-setting behaviour

| | Interactive one-shot | Scripted / piped | cron or timer | Watch in a terminal (screen/byobu) | systemd service |
|---|---|---|---|---|---|
| **Audience** | Person, now | Program | Nobody unless it fails | Person, occasionally | `journalctl`, later |
| **Startup line** | Not needed for short runs | No | No | **Yes**: account, folder, mode, how changes are detected | Yes, once |
| **Per-file activity** | Yes, one line per completed transfer/delete/conflict | No | No (or to log file) | **Yes**, timestamped | Yes, one line each, INFO |
| **Progress** | Single redrawing line on stderr, for transfers over a few seconds | Never | Never | Periodic one-line summaries, no redraw spam in scrollback | Never; `STATUS=` instead |
| **Summary** | Human: "Downloaded 19 files (300 MB) in 6 s" | Exit code (and `--json`) | Silent on success | Per pass, only when something happened | Per pass, only when something happened |
| **Timestamps on console** | No | No | n/a | Yes (scrollback has none) | **No**: journald adds them |
| **Warnings/errors** | Plain reason + action | Errors only | Errors only (cron mails them) | Plain reason + action | With `<3>`/`<4>` priority prefixes |
| **Color** | If stderr TTY and not disabled | Never | Never | Allowed | Never |
| **Prompts** | Allowed | Never | Never | Never | Never |
| **Log file** | Optional | No | Useful | Recommended | **No**: the journal is the log |
| **Exit code** | Informational | The contract | Drives cron mail / unit failure | Read by the wrapper | Drives `Restart=` |

## 5. Message style

- **User vocabulary only at default verbosity.** Say account, drive, folder,
  file, shared folder. Never mount, scope, baseline, orchestrator, RunOnce,
  actionable set, observer, canonical ID, item ID, or Go error chains.
- **Paths relative to the sync folder**, the folder itself shown once with `~`.
- **Human units.** "6 s", "2 min 10 s", "300 MB", never `5.571055002s` or
  `"duration":1636185931`.
- **Errors**: one line `Error: <what happened>`, then `Reason:` and `Action:`
  lines. The underlying error chain belongs at `--debug` or in the log.
- **Retries are visible while they happen**: "Can't reach OneDrive (connection
  refused). Retrying in 8 s (attempt 3 of 6)". Not 30 seconds of silence
  followed by a stack of wrapped errors.
- **A clean shutdown produces no warnings.** Cancellation during shutdown is
  expected, not a WARN.
- **No level labels at default verbosity** (`level=WARN`, `ERROR:` prefixes on
  everything). Use `Warning:` / `Error:` words only where they help.

## 6. Verbosity ladder

| Level | Flag | Adds |
|---|---|---|
| quiet | `-q` | Errors only |
| default | | Startup line (watch), per-file activity, summaries, warnings with actions |
| verbose | `-v` | Failures that will be retried, the full error chain behind plain messages, and timestamped key=value progress logs (scanning, delta pages, plan counts) |
| debug | `--debug` | Internal state, HTTP, timings |

The log file has its own level and is always structured (JSON lines). The
console is always human text, whatever `log_format` says, unless the console
is the journal.

## 7. systemd

- `Type=notify`: send `READY=1` once the first pass has finished (or the
  watcher is running), `STATUS=` with a one-line state
  ("Watching ~/OneDrive, 3 uploads queued", "Sign-in required"),
  `STOPPING=1` on shutdown, `RELOADING=1`/`READY=1` around a reload, and
  `WATCHDOG=1` if `WatchdogSec=` is set.
- Under the journal, write plain text with `<N>` priority prefixes per line
  and no timestamps, so `journalctl -p warning` works.
- Exit codes distinguish "do not restart" (bad config, sign-in required) from
  transient failures, so units can use `RestartPreventExitStatus=`.
- Default log location, when one is used: `$XDG_STATE_HOME/onedrive-go/`
  (`~/.local/state/onedrive-go/`).

## 8. Signals

- First SIGINT/SIGTERM: say "Stopping…" immediately, finish or abandon
  in-flight work within a bounded time, exit 0 (or 130 for SIGINT).
- Second SIGINT: exit at once, and say so the first time ("press Ctrl-C again
  to force").
- Writing to a closed pipe (EPIPE): exit quietly.
