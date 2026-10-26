# onedrive-go

[![CI](https://github.com/tonimelisma/onedrive-go/actions/workflows/ci.yml/badge.svg)](https://github.com/tonimelisma/onedrive-go/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/tonimelisma/onedrive-go)](https://goreportcard.com/report/github.com/tonimelisma/onedrive-go)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A OneDrive command-line client and two-way sync client for Linux and macOS.

## Status

> [!WARNING]
> **Pre-release.** There are no releases yet: you build it from source, and it
> has not been used outside its own testing. Data-safety hardening is in
> progress (see [known gaps](docs/safety.md#known-gaps)). Try it first with
> `sync --dry-run` or a test account, and keep a backup of anything you cannot
> lose. onedrive-go is not affiliated with Microsoft.

File commands, two-way sync, drive management, configuration and transfers
are implemented and covered by tests; some status and retry details are still
being finished, and performance has not been measured yet. The
[requirements](spec/requirements/index.md) are the source of truth for what
each capability does and how far it is proven.

## What it does

- **Two-way sync** between a local folder and OneDrive, once (`sync`) or
  continuously (`sync --watch`), plus `--download-only`, `--upload-only` and
  `--dry-run`.
- **Keeps both versions** when a file changed on both sides: OneDrive's copy
  stays at the original name and yours is saved next to it as
  `name.conflict-YYYYMMDD-HHMMSS.ext`.
- **Every OneDrive type**: personal, work or school, SharePoint document
  libraries, and folders other people shared with you.
- **Unix-style file commands** (`ls`, `get`, `put`, `rm`, `mkdir`, `stat`,
  `mv`, `cp`) that work directly against OneDrive, with or without sync.
- **Filters** per drive: sync only some folders, or skip folders, glob
  patterns, dotfiles and common editor or OS junk files.
- **Built for unattended use**: a systemd `Type=notify` unit, journal-aware
  output, exit codes a service manager can act on, and `--json` output for
  scripts.
- Resumable uploads and downloads, hash-checked transfers, and automatic
  backoff when OneDrive throttles or is unavailable.

## What it looks like

```console
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

$ onedrive-go sync --watch
[00:19:03] Watching ~/OneDrive for user@example.com.
[00:19:03] Press Ctrl-C to stop.
[00:19:26] Uploaded notes.txt (11 B)
[00:19:49] Deleted on OneDrive notes.txt
```

## Install

There are no Homebrew, deb, rpm, AUR or container packages yet.

### Building from source

You need Go 1.26 or newer, on Linux or macOS. FreeBSD builds on every CI run
but is not otherwise tested.

```bash
git clone https://github.com/tonimelisma/onedrive-go.git
cd onedrive-go
go build -ldflags "-X github.com/tonimelisma/onedrive-go/internal/cli.version=$(git rev-parse --short HEAD)" -o onedrive-go .
install -m 0755 onedrive-go ~/.local/bin/
```

Keep the `-ldflags` part. A build without it (including `go install`) calls
itself a development build and, to protect your real files, refuses to run
unless `XDG_CONFIG_HOME`, `XDG_DATA_HOME` and `XDG_CACHE_HOME` all point at a
scratch location (`source scripts/dev-env.sh` sets them).

## Quick start

1. **Sign in.** This shows a code to enter at Microsoft's sign-in page
   (`--browser` opens a browser instead) and adds your main OneDrive:

   ```console
   $ onedrive-go login
   …
   Signed in as user@example.com (personal account).
   Your files will sync to ~/OneDrive.
   Next: run 'onedrive-go sync' to sync now, or 'onedrive-go sync --watch' to keep syncing.
   ```

   Personal accounts sync to `~/OneDrive`, work or school accounts to
   `~/OneDrive - <organization>`.

2. **Preview** what the first sync would do. Nothing is changed:

   ```bash
   onedrive-go sync --dry-run
   ```

3. **Sync once**, or **keep syncing** until you press Ctrl-C:

   ```bash
   onedrive-go sync
   onedrive-go sync --watch
   ```

4. **Check on it** at any time, even while a sync is running:

   ```bash
   onedrive-go status
   ```

If the folder already has files, the first sync merges them with OneDrive:
files on only one side are copied to the other, identical files are left
alone, and files that differ are kept as conflict copies. Nothing is deleted
on either side on the first sync just because it is missing from the other.

## How your files are kept safe

- **Deletes are only propagated for files that were synced before.** A file
  missing on one side is deleted on the other only if onedrive-go recorded it
  as synced; otherwise it is copied over.
- **Deletes on OneDrive go to the OneDrive recycle bin.** Deletes on your
  computer are real deletes, so the recycle bin is where a mistake is undone.
- **Local deletes are checked first.** Before deleting a local file because it
  was deleted on OneDrive, onedrive-go checks that it is still what it last
  synced. If you changed it, your version is kept and uploaded again.
- **Conflicts keep both copies.**
- **An interrupted OneDrive listing never causes deletes**, and neither does a
  folder inside the sync folder that onedrive-go is not allowed to read.
- **Downloads land atomically.** Files are written under a temporary name,
  checked against OneDrive's hash, and then renamed into place. Downloads stop
  when free space would fall below `min_free_space` (1 GB by default).
- **Personal Vault and OneNote notebooks are skipped.**
- **One sync at a time** per configuration; other commands can run alongside.

There is no limit on how many deletes one sync propagates, and an unmounted
or emptied sync folder is not yet detected. Read
[docs/safety.md](docs/safety.md) for the full model and its known gaps.

## Commands

| Command | What it does |
| --- | --- |
| `login` / `logout` | Sign in (adds your main drive) / sign out (`--purge` also removes sync state) |
| `sync` | Sync once; `--watch`, `--download-only`, `--upload-only`, `--dry-run`, `--full` |
| `status` | Accounts, drives, shared folders, and anything that needs you |
| `pause [duration]` / `resume` | Pause syncing a drive (for example `2h`, `1d`) / resume it |
| `ls`, `stat` | List a OneDrive folder / show an item's details |
| `get`, `put` | Download / upload a file or folder |
| `mkdir`, `mv`, `cp`, `rm` | Create, move, copy (server-side) and delete (`rm` uses the recycle bin; `--permanent` skips it) |
| `shared` | List files and folders shared with you |
| `drive list`, `add`, `remove`, `search` | Choose which drives are configured; find SharePoint libraries |
| `drive reset-sync-state` | Start a drive's sync state over (your files are not touched) |
| `recycle-bin list`, `restore`, `empty` | Work or school and SharePoint drives only |

Every command has `--help`. Global flags: `--drive`, `--account`, `--config`,
`--json`, `-v`/`--verbose`, `--debug`, `-q`/`--quiet`.

## Work drives, SharePoint and shared folders

```bash
onedrive-go drive list                   # configured drives, and drives you could add
onedrive-go drive search marketing       # find SharePoint sites and libraries
onedrive-go drive add sharepoint:user@contoso.com:marketing:Documents
onedrive-go drive add "Shared Folder"    # a folder shared with you, by name or share link
```

Run `login` once per account. With several drives configured, `sync` syncs
every drive that is not paused, and other commands need `--drive` (a
canonical ID such as `personal:user@example.com`, a display name, or part of
one). Shared folders you added to your own OneDrive as shortcuts are synced
automatically with the drive that holds the shortcut.

## Configuration

`login` creates the configuration file with every global setting present as a
commented-out default:

- Linux: `~/.config/onedrive-go/config.toml`
- macOS: `~/Library/Application Support/onedrive-go/config.toml`

Use `--config` or `ONEDRIVE_GO_CONFIG` to point elsewhere.

```toml
# min_free_space = "1GB"
# poll_interval = "5m"

["personal:user@example.com"]
sync_dir = "~/OneDrive"
ignore_junk_files = true
ignored_dirs = ["node_modules"]
ignored_paths = ["*.iso"]
```

Unknown or misspelled keys are an error. A running `sync --watch` picks up
edits within a few seconds, without a restart. Every setting is described in
[docs/configuration.md](docs/configuration.md).

## Running in the background

On Linux, use the systemd user unit in
[`contrib/systemd/onedrive-go.service`](contrib/systemd/onedrive-go.service).
Sign in first: the service cannot sign in for you.

```bash
mkdir -p ~/.config/systemd/user
cp contrib/systemd/onedrive-go.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now onedrive-go
loginctl enable-linger "$USER"     # keep syncing while logged out
```

`systemctl --user status onedrive-go` shows what is being watched, and
`systemctl --user reload onedrive-go` applies config edits at once. On macOS,
run `sync --watch` in a terminal for now; no launchd example is provided yet.
See [docs/running-as-a-service.md](docs/running-as-a-service.md).

Exit codes tell a service manager or script whether to retry:

| Code | Meaning |
| --- | --- |
| 0 | Success, or `sync --watch` stopped normally |
| 1 | Other failure (network, throttling, server); running again may work |
| 2 | Command-line mistake |
| 77 | Sign-in required: run `onedrive-go login` |
| 78 | Invalid configuration: fix `config.toml` or `--drive` |
| 130 | Stopped with Ctrl-C |

See [docs/scripting.md](docs/scripting.md) for `--json` output.

## Limitations

- Linux and macOS only. Windows is out of scope; Microsoft ships a native
  client.
- Files are downloaded in full. There is no Files On-Demand.
- Microsoft's global cloud only; national clouds (US Government, China) are
  not supported.
- No packages, no `service install` command, and no migration from other
  clients yet.
- Do not point two sync clients at the same folder.

More in [docs/limitations.md](docs/limitations.md) and the
[FAQ](docs/faq.md). For how it compares with
[abraunegg/onedrive](https://github.com/abraunegg/onedrive) and
[rclone](https://rclone.org/), see [docs/comparison.md](docs/comparison.md).

## Getting help

Start with [docs/troubleshooting.md](docs/troubleshooting.md). To report a
bug, open an [issue](https://github.com/tonimelisma/onedrive-go/issues) with
the output of `onedrive-go --version` and `onedrive-go status`, what you ran
and what happened. `--debug` adds detail, but check it for file names and
account details before posting it. Report security problems privately as
described in [SECURITY.md](SECURITY.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and the
[developer onboarding guide](spec/reference/developer-onboarding.md).
Behavior is specified in [spec/requirements](spec/requirements/) and
[spec/design](spec/design/).

## License

[MIT](LICENSE)
