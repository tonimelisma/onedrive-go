# Scripting with onedrive-go

## Output streams

- **stdout** carries results: listings, file details, `status`, and with
  `--json` only JSON.
- **stderr** carries everything else: progress, warnings, prompts, errors and
  confirmations such as "Paused … until …". `sync` is the one command whose
  progress is its result: its "Syncing ~/OneDrive with …" line, the line per
  file and the summary go to stdout, and only failures and warnings to stderr.

`-q`/`--quiet` drops informational output; warnings and errors still print.
Transfer progress bars appear only when stderr is a terminal and neither
`--quiet` nor `--json` is set.

## `--json`

`--json` is a global flag. Every command either honors it or refuses it as a
command-line mistake (exit 2); none ignores it.

- A command that honors it prints **exactly one JSON document** on stdout and
  no prose confirmation. The exit status is the same as without `--json`.
- A command whose run partly failed (for example a folder `get` where some
  files failed) still prints its document, and exits non-zero.
- Mutating commands print their result too: `login`, `logout`, `pause`,
  `resume`, `drive add`, `drive remove`, `drive reset-sync-state`,
  `drive rebind-root`, `recycle-bin empty`, `trash restore` and
  `trash empty` (`trash list` prints the entries).
- `sync` is the exception to "one document": it **streams JSON lines**, one
  object per event (below).

Examples:

```bash
onedrive-go ls --json /Documents | jq -r '.[] | select(.is_folder | not) | .name'
onedrive-go stat --json /Documents/report.pdf | jq .size
onedrive-go status --json | jq '.summary'
```

### `status --json`

The document has `accounts` (each with its `drives`, and each drive with its
`shared_folders`) and a `summary` with totals such as `total_drives`,
`paused`, `with_issues`, `accounts_requiring_sign_in` and `total_issues`. Each
drive's `sync_state.issues` lists problem groups:

```json
{
  "type": "invalid_filename",
  "title": "Invalid filename",
  "reason": "OneDrive does not allow these names: they contain a forbidden character, are reserved, or end in a space or period.",
  "action": "Rename the files so OneDrive accepts their names.",
  "count": 1,
  "paths": ["/invalid:name.txt"]
}
```

Match on `type`; `title`, `reason` and `action` are for people and may be
reworded. `paths` is sampled like the text output (`examples_limit` gives the
sample size); add `--verbose` for every path. The shape is the same with or
without `--drive`.

### `sync --json` event lines

Each line is one JSON object with an `event` field:

| `event` | When | Main fields |
| --- | --- | --- |
| `start` | One-shot sync begins, one line per drive | `drive`, `sync_dir`, `account`, `mode`, `dry_run`, `paused` |
| `watching` | `sync --watch` is running, one line per drive | `drive`, `sync_dir`, `account`, `mode`, `paused` |
| `activity` | A file action finished (or, in a dry run, is planned) | `drive`, `action`, `path`, `old_path` (moves), `size`, `status`, `error` |
| `summary` | A drive's one-shot sync finished; `failed`, `will_retry` and `still_open` are what its last pass left | `drive`, `status`, `duration_ms`, `counts`, `failed`, `will_retry`, `still_open`, `dry_run`, `error` |
| `stopped` | `sync --watch` stopped | |

- `mode` is `bidirectional`, `download_only` or `upload_only`.
- `action` is `download`, `upload`, `delete_local`, `delete_remote`,
  `move_local`, `move_remote`, `create_folder` or `conflict_copy`.
- `status` on an `activity` is `done`, `planned` (dry run), `will_retry` or
  `failed`. On a `summary` it is `done`, `incomplete` (the drive left files
  unsynced, and the run exits 1) or `failed` (the drive could not sync;
  `error` says why).
- `counts` maps the same `action` names to how many were done.
- On a `summary`, `failed`, `will_retry` and `still_open` describe what the
  run's last pass left, each file once: `failed` counts the files whose
  action failed and is not retried by itself, `will_retry` the work waiting
  to be retried or left for the next sync, and `still_open` the files an
  issue listed by `status` keeps unsynced (a name OneDrive does not allow, a
  folder that cannot be read). A sync can make more than one pass, and a
  file an earlier pass failed and a later pass finished is in none of them.
  `still_open` never counts work waiting to be retried: all such work is work
  the run itself attempted or held, already in `failed` or `will_retry`.
  They are the numbers of the text summary.
- Held deletes are not part of the `summary` object; read `status --json` and
  look for an issue with `type` `deletes_held`.

```bash
onedrive-go sync --json | jq -c 'select(.event == "activity" and .status == "failed")'
```

## Exit codes

| Code | Meaning | Retry unchanged? |
| --- | --- | --- |
| 0 | Success, or `sync --watch` stopped normally | |
| 1 | Any other failure, including network, throttling and server errors, and a one-shot `sync` that left a file unsynced | May work |
| 2 | Command-line mistake: unknown command or flag, wrong arguments, conflicting flags | No |
| 77 | Sign-in required: no saved login, or Microsoft rejected or revoked it (sysexits `EX_NOPERM`) | No: run `onedrive-go login` |
| 78 | Invalid configuration: unreadable or invalid file, a `--drive` that matches nothing, no drives configured (sysexits `EX_CONFIG`) | No: fix the config |
| 130 | A command other than `sync --watch` stopped with Ctrl-C or SIGTERM | |

The code is chosen from the kind of error, never from its message text, so it
is safe to branch on. `sync` run as root without `--allow-root` is a
command-line mistake (2). One-shot `sync` exits non-zero when a selected drive
could not sync at all, and exits 1 when a drive it ran left a file unsynced:
a file action failed, work is waiting to be retried, or a condition `status`
lists keeps a file from syncing. It exits 0 only when `status` would list
nothing left for the drives it ran, so a condition that needs you (a name
OneDrive does not allow, a read-only shared folder) makes every run exit 1
until it is dealt with. Work `--upload-only` or `--download-only` holds back,
deletes held for `--confirm-deletes`, conflict copies, a paused drive beside
one that ran, and `--dry-run` do not fail the run. A drive that needs a new
sign-in or has an invalid configuration still exits 77 or 78. `sync --watch`
exits 77 or 78 only when every drive that could not start failed for that
same reason.

## Environment variables

| Variable | Effect |
| --- | --- |
| `ONEDRIVE_GO_CONFIG` | Config file path (same as `--config`) |
| `ONEDRIVE_GO_DRIVE` | Drive selector (same as `--drive`) |
| `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME` | Base directories for config, data and cache, on Linux and macOS |
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` | Proxy settings |
