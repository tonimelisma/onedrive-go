# Output by User Journey: onedrive-go vs abraunegg/onedrive

Audit of what each client actually prints for each user journey, measured
against [cli-output-guidelines.md](cli-output-guidelines.md).

- **onedrive-go**: `78fdc5d`, captured 2026-10-23 on purkki against a personal
  test account with 19 files (~300 MB), in an isolated
  `XDG_CONFIG_HOME`/`XDG_DATA_HOME` (paths below shortened to `~/odgo-test`).
  stdout and stderr were captured separately; watch mode ran on a pty.
- **abraunegg/onedrive** (the D client): v2.5.11 running in production on the
  same host (`onedrive --monitor`, captured from its byobu pane), plus source
  strings from `src/*.d` at master (2.5.12) and output users pasted into
  GitHub issues (#3300, #3344, #3463, #3609, #3718).

## After the console-output increment

The fix list at the end was implemented in the increment that followed this
audit (except the items marked otherwise there). The same journeys against
the same account now print:

```
$ onedrive-go sync --dry-run
Checking (dry run) ~/OneDrive with user@example.com...
Would download Accessory Structures.pdf
Would create folder Documents
…
Dry run, nothing was changed. Would download 19 files, create 2 folders.

$ onedrive-go sync
Syncing ~/OneDrive with user@example.com...
Downloaded Accessory Structures.pdf (746.6 KB)
Created folder Documents
…
Done in 6.2 s: downloaded 19 files (339.2 MB), created 2 folders.

$ onedrive-go sync            # after deleting one local file
Syncing ~/OneDrive with user@example.com...
Deleted on OneDrive odgo-journey-test.txt
Done in 2.5 s: deleted 1 file on OneDrive.

$ onedrive-go sync            # nothing changed
Syncing ~/OneDrive with user@example.com...
Up to date (1.6 s).

$ onedrive-go sync --watch    # local add, local delete, Ctrl-C
[00:19:03] Watching ~/OneDrive for user@example.com.
[00:19:03] Press Ctrl-C to stop.
[00:19:26] Uploaded odgo-watch-test.txt (11 B)
[00:19:49] Deleted on OneDrive odgo-watch-test.txt
Stopping... (press Ctrl-C again to force quit)
[00:20:08] Stopped.

$ onedrive-go sync            # refresh token rejected (invalid_grant)
Syncing ~/OneDrive with user@example.com...
Authentication required: Microsoft no longer accepts the saved login for this account (it expired or was revoked). Run 'onedrive-go login' to sign in again.

$ onedrive-go sync            # network unreachable
Syncing ~/OneDrive with user@example.com...
Warning: can't reach OneDrive, retrying (retry_in=2 s, attempt=2, max_attempts=6, error="… connection refused")
Error: can't reach OneDrive through the configured proxy (connection refused). Check your network connection, then run the command again.
```

Two bugs surfaced while diagnosing the watch output and were fixed in the
same increment: watch mode never advanced the delta token when a window held
only deletes of items never synced here, so it replayed them (and warned)
every poll; and closing the store with the canceled shutdown context made
every clean Ctrl-C log a false checkpoint failure.

## Verdict (at the time of the audit)

The D client is chatty, sends everything (errors included) to stdout, has no
journal priorities and spams progress lines into the journal. But at every
moment it tells you what it is doing, names every file it touches, explains
retries while they happen, and says what to do on failure.

onedrive-go has the right *plumbing* (separate stdout/stderr, verbosity
flags, a JSON log file, a control socket, graceful shutdown) and almost none
of the *conversation*. It fails the three questions of the guidelines in
nearly every journey:

| Journey | What is it doing? | What did it do? | What must I do? |
|---|---|---|---|
| Login | Partly | Yes | Partly |
| First sync | **No** (silent until done) | **No** (counts only) | n/a |
| Regular sync | No | **No** (counts only) | n/a |
| Watch mode | **No** (prints nothing at all) | **No** | Spurious warnings |
| Not signed in | n/a | n/a | Yes, but printed twice in internal vocabulary |
| Expired sign-in | n/a | n/a | **No**: `status` doesn't say sign-in is required |
| Network down | **No** (30 s silence) | n/a | **No**: raw Go error chain, twice |
| `--verbose` | Yes, in internal jargon | No | n/a |

## 1. Login

**onedrive-go** (`login`, device code; both lines on stderr):

```
To sign in, visit: https://login.microsoft.com/device
Enter code: ABCD1234E
```

then, on success, `Signed in as <email> (personal account).`

**D client** (device-code mode):

```
Authorise this application by visiting:

https://microsoft.com/devicelogin

Enter the following code when prompted: ABCDEFGHI

This code expires at: 2025-Jun-02 15:27:30
```

and after success prints account type, drive ID, data centre and free space,
plus what to run next if no sync option was given.

**Gaps in onedrive-go**: no expiry time, no "waiting for you to sign in…",
no hint which account to sign in as (it knows: `--account` was given), nothing
on what to do next ("run `onedrive-go sync`"). The real failure seen during
this audit, a passkey sign-in that ends on a `localhost` error page, gives
the user nothing to go on; a one-line tip ("if a passkey sign-in fails, choose
*Other ways to sign in → Password*") would have saved the round trip.

## 2. Before the first sync

```
$ onedrive-go drive list
Configured drives:
  DRIVE                                                             SYNC DIR            AUTH   STATE
  user@example.com (personal:user@example.com)  ~/odgo-test/sync    ready  up_to_date

Available drives (not configured):
  Shared Projects (shared:user@example.com:<drive-id>:<item-id>) (shared by sharer@example.com)
```

- `STATE up_to_date` for a drive **that has never been synced** (empty state
  directory). This is wrong, not just ugly.
- Canonical IDs are internal identifiers on the default screen; `up_to_date`
  is an enum, not English.
- `status` likewise shows the folder and storage with no "never synced".

## 3. First sync (19 files, ~300 MB into an empty folder)

**onedrive-go**: silent for the whole run, then (stderr):

```
Mode: bidirectional
Duration: 5.571055002s

Plan:
  Folder creates: 2
  Downloads:      19

Results:
  Succeeded: 21
  Failed:    0
```

**D client**: a line per phase and per file, with live progress on large
files (from the production pane on purkki):

```
Number of items to download from Microsoft OneDrive: 7
Downloading: Pictures/Camera Roll/2026/10/20261022_193753.mp4 ... 95%  |  ETA    00:00:05
Downloading: Pictures/Camera Roll/2026/10/20261022_193753.mp4 ... 100% | DONE in 00:01:29
Downloading file: Pictures/Camera Roll/2026/10/20261022_193753.mp4 ... done
Sync with Microsoft OneDrive is complete
```

**Gaps**: no indication anything is happening (on a real first sync of a
large drive this is hours of silence), no file names, nanosecond durations,
"Mode: bidirectional" and "Folder creates" are implementation terms, and
"Plan"/"Results" double-report the same numbers. What it should say:

```
Syncing ~/OneDrive with user@example.com (first sync)
  ↓ Documents/report.pdf
  ↓ Pictures/walkbikewc_signup_qr_code.png
  …
Downloaded 19 files (300 MB) in 6 s.
```

## 4. Regular sync

No changes:

```
Mode: bidirectional
Duration: 1.577261989s
No changes detected
```

One new local file, then the same file deleted locally:

```
Plan:
  Uploads:        1
Results:
  Succeeded: 1
  Failed:    0
```
```
Plan:
  Remote deletes: 1
Results:
  Succeeded: 1
  Failed:    0
```

The second one deleted a file from the user's OneDrive and never said which.
The D client prints `Uploading new file: <path> ... done` and
`Deleting item from Microsoft OneDrive: <path>`. The same applies to conflicts:
onedrive-go reports `Conflict copies: N`, while the D client names both the
original and the `-<hostname>-safeBackup-0001` copy.

## 5. Watch mode (the long-running mode)

**onedrive-go** `sync --watch` on a pty for 60 s, with a file created and
deleted locally in between, then Ctrl-C. Complete output:

```
time=2026-10-23T22:47:14.183-07:00 level=WARN msg="skipping remote delete without recoverable path" item_id=<item-id> name="" drive_id=<drive-id>
time=2026-10-23T22:47:14.953-07:00 level=WARN msg="skipping remote delete without recoverable path" item_id=<item-id> name="" drive_id=<drive-id>
time=2026-10-23T22:48:12.618-07:00 level=INFO msg="received signal, initiating graceful shutdown" signal=interrupt
time=2026-10-23T22:48:12.623-07:00 level=WARN msg="checkpoint failed on close" error="checkpoint sync store WAL: context canceled"
```

- No startup line: nothing says it is running, which folder, or which account.
- The upload and the delete it performed are not mentioned.
- The only output is two identical warnings in internal terms with no possible
  user action (breaks the project's own R-6.6.11), and a false-alarm WARN on a
  clean shutdown.
- The instance under `onedrive-go-watch` in byobu showed exactly one line, the
  wrapper's own, for its whole run.

**D client** `--monitor` startup on purkki:

```
Using IPv4 and IPv6 (if configured) for all network operations
Attempting to contact the Microsoft OneDrive Service
Successfully reached the Microsoft OneDrive Service
Attempting to enable WebSocket support to monitor Microsoft Graph API changes in near real-time.
Enabled WebSocket support to monitor Microsoft Graph API changes in near real-time.
OneDrive synchronisation interval (seconds): 300
Initialising filesystem inotify monitoring ...
Performing initial synchronisation to ensure consistent local state ...
Starting a sync with Microsoft OneDrive
Syncing this OneDrive Personal Shared Folder: ./Perheen kuvat/Monan kuvat
Processing 21620 applicable JSON items received from Microsoft OneDrive .............
```

then per change `Received 1 signal(s) from WebSocket handler (2026-Oct-23 22:22:16)`
and the per-file lines, and on Ctrl-C:

```
Received termination signal, attempting to cleanly shutdown application
Attempting to perform a database vacuum to optimise database
Database vacuum is complete
```

The D client also has noise worth not copying: a `threads = 8 exceeds CPU
cores` warning and a D-Bus warning on every start, drive IDs in progress
lines, and no timestamps on the console. At default verbosity it suppresses
most per-cycle output after the first (`monitor_log_frequency`).

## 6. `--verbose`

```
time=2026-10-23T22:46:57.901-07:00 level=INFO msg="loaded saved token" path=…/token_personal_user@example.com.json expiry=… expired=false
{"time":"…","level":"INFO","msg":"orchestrator starting RunOnce","mounts":1,"mode":"bidirectional"}
{"time":"…","level":"INFO","msg":"sqlite actionable-set plan complete","total_actions":0,"folder_creates":0,"moves":0,"downloads":0,"uploads":0,"local_deletes":0,"remote_deletes":0,"conflict_copies":0,"baseline_updates":0,"cleanups":0,"deferred_folder_creates":0,…}
{"time":"…","level":"INFO","msg":"received parent child work snapshot","namespace_id":"personal:user@example.com","children":0}
{"time":"…","level":"INFO","msg":"sync pass complete: no changes detected","duration":1636185931}
Mode: bidirectional
Duration: 1.636203631s
No changes detected
{"time":"…","level":"INFO","msg":"performance summary",…}
```

- **Three formats on one stream**: logfmt from the bootstrap logger, JSON from
  the command logger (`log_format = auto` picked JSON because stderr was not a
  TTY), and the human summary wedged in the middle.
- `--verbose` is effectively `--debug`: internal component names, 17 zero
  counters, nanosecond durations. There is no level that shows "scanning…,
  3 remote changes, uploading x" in plain words.

## 7. Failures

**Not signed in** (`sync`, token file absent), exit 1:

```
Error: session error for mount personal:user@example.com: not logged in — run 'onedrive-go login' first: graph: not logged in
Error: session error for mount personal:user@example.com: not logged in — run 'onedrive-go login' first: graph: not logged in
```

Printed twice; "session error for mount", the stuttering chain, and the
suggested command lacks `--account`. By contrast `status` gets this right and
is the best output in the app:

```
Account: Test User <user@example.com>
  Sign-in required: No saved login was found for this account.
  Action: Run 'onedrive-go login' to sign in.
```

**Expired sign-in** (refresh token expired after five months unused), `status`:

```
time=… level=WARN msg="token acquisition failed" error="oauth2: \"invalid_grant\" \"AADSTS70000: The user could not be authenticated as the grant is expired. The user must sign in again. Trace ID: … Correlation ID: … Timestamp: …\" \"https://login.microsoftonline.com/error?code=70000\""
time=… level=WARN msg="status: live account probe failed" account=… error="graph: GET /me: obtaining token: graph: obtaining token: oauth2: …"
Account: Test User <user@example.com>
  Test User's personal OneDrive
    Folder: ~/OneDrive
    Files: 7
```

The most common real-world failure for a sync client is shown as two raw log
lines, and the status block itself does **not** say sign-in is required: the
"Sign-in required / Action" path only fires for a missing token, not an
expired one. The D client: `ERROR: You will need to issue a --reauth and
re-authorise this client to obtain a fresh auth token.`

**Network unreachable** (`sync` through a dead proxy), 32.6 s of silence, exit 1:

```
time=… level=WARN msg="request failed after all retries" method=GET url=https://graph.microsoft.com/v1.0/me attempts=6 error="round trip: proxyconnect tcp: dial tcp 127.0.0.1:9: connect: connection refused"
Error: run sync engine once: sync: verifying drive identity: graph: GET /drives/<drive-id>: HTTP GET https://graph.microsoft.com/v1.0/drives/<drive-id>: dispatching request: Get "https://graph.microsoft.com/v1.0/drives/<drive-id>": round trip: proxyconnect tcp: dial tcp 127.0.0.1:9: connect: connection refused
Error: run sync engine once: sync: verifying drive identity: graph: GET /drives/…: …
```

Silent during the retries, then a nine-level Go error chain, printed twice,
with no plain reason or action. The D client is verbose here too, but says
what is happening while it happens:

```
ERROR: Encountered a std.net.curl.CurlException:
Error Message:    Could not resolve hostname on handle 55DD85E397A0
Retry attempt:           1 - Internal Thread ID: 2sbuCYXp8PE1UmcW
Next retry in approx:   1 seconds
```

**Dry run**: the same unactionable `skipping remote delete without recoverable
path` warnings appear, as JSON this time, ahead of `Dry run — no changes
applied`. A dry run does not list what it would do.

**Mass delete**: the D client stops with three `ERROR:` lines naming the count
and the two ways to proceed (`--force` or `classify_as_big_delete`).

## 8. Under systemd

Neither client is good here. The D client writes everything to stdout at
journal priority info, progress percentages become separate journal lines, and
its shipped units are `Type=simple`. onedrive-go's default `log_format = auto`
would put JSON objects into the journal, also all at priority info, and has no
`sd_notify`. Neither detects `$JOURNAL_STREAM`. (Historical: onedrive-go has
since gained journal detection, `Type=notify` and distinct exit statuses; see
the fix list below.)

## Fix list, in order of user impact

Status: items 1–6, 10 and 11 are done. Item 5's logger split is finished: every
command gets the configured log file before it runs, and the records logged
while the config file is being found are replayed through the command logger,
so one run uses one console format and the log file has the whole run. Item 7 was resolved differently: `-v`
stays the "extra detail" level (timestamped key=value logs, retried failures,
full error chains) rather than a separate plain-words tier, because default
output now carries the plain-words narrative. Item 9 is done for state and
wording; canonical IDs stay in `drive list` because `drive add` takes them.
Item 10 is done: journal priorities, `Type=notify` (R-6.6.32) and exit
statuses 77/78 for `RestartPreventExitStatus=` (R-6.6.31). Item 8
(progress bars) remains `[future]` (R-6.6.5).

1. **Watch mode startup line and per-file activity lines** at default
   verbosity (stderr, timestamped when on a TTY and not the journal).
2. **Name files** in one-shot output: one line per transfer, delete, move and
   conflict copy; keep the counts as a one-line summary in human units.
3. **Expired or revoked sign-in is "Sign-in required"** everywhere: `status`,
   `sync`, watch mode; with the exact `onedrive-go login --account <email>`.
4. **Errors**: print once, plain `Error/Reason/Action`, no error chains below
   `--debug`. Network retries announced while they happen.
5. **One console format**: human text on the console always; JSON only in
   the log file or with `--json`. Fix the bootstrap/command logger split.
6. **Demote internal warnings** (`skipping remote delete without recoverable
   path`, `checkpoint failed on close` on cancellation) to DEBUG.
7. **Make `-v` a real middle level** in user vocabulary; move today's `-v`
   content to `--debug`.
8. **Progress** for long transfers on a TTY (already `[future]` R-6.6.5).
9. **`drive list` correctness and vocabulary**: no `up_to_date` for a never-synced
   drive; English states; IDs only with `-v`.
10. **systemd**: `$JOURNAL_STREAM` detection with `<N>` priorities and no
    timestamps, `Type=notify` with `READY=1`/`STATUS=`.
11. **Login**: show code expiry, "waiting for sign-in…", which account, the
    passkey tip, and the next command.
