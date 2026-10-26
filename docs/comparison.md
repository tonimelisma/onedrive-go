# onedrive-go compared with other clients

The two most common ways to use OneDrive from Linux without Microsoft's own
client are [abraunegg/onedrive](https://github.com/abraunegg/onedrive) (the
"OneDrive Client for Linux") and [rclone](https://rclone.org/). This page
compares what each one offers, as a factual starting point for choosing. It
makes no performance claims: none of the three has been benchmarked here.

**Sources and date.** Checked on 2026-10-25 against:

- abraunegg/onedrive: its `readme.md` and `docs/` at commit `abbb800`
  (2026-10-24, release 2.5.12).
- rclone: `docs/content/onedrive.md`, `docs/content/bisync.md` and
  `docs/content/commands/rclone_mount.md` at commit `81e7372f6`
  (2026-10-19), also published at [rclone.org](https://rclone.org/).
- onedrive-go: this repository's `main` branch.

Projects change; check their documentation for the current state. Corrections
are welcome as issues or pull requests.

## At a glance

| | onedrive-go | abraunegg/onedrive | rclone |
| --- | --- | --- | --- |
| What it is | OneDrive CLI and two-way sync client | OneDrive sync client | General command-line tool for many storage services, including OneDrive |
| Language, license | Go, MIT | D, GPLv3 | Go, MIT |
| Maturity | Pre-release, no releases yet | Released, maintained since 2018 | Released; `bisync` is documented as an "advanced command" |
| Platforms | Linux, macOS (FreeBSD builds, untested) | Linux, FreeBSD, OpenBSD; Docker and Podman images. Not Windows or macOS natively | Linux, macOS, Windows, BSDs and more |
| Install | Build from source | Distribution packages, source builds, containers | Release binaries, packages |
| OneDrive Personal, work or school | Yes | Yes | Yes |
| SharePoint libraries | Yes | Yes | Yes |
| Folders shared with you | Yes | Yes | See rclone's OneDrive docs |
| National clouds | No | Yes (US Government, Germany, China) | Yes (`--onedrive-region`) |

## Syncing

| | onedrive-go | abraunegg/onedrive | rclone |
| --- | --- | --- | --- |
| Two-way sync | `sync` | Default mode | `rclone bisync` |
| One-way modes | `--download-only`, `--upload-only` | Upload-only and download-only modes | `rclone sync` / `rclone copy` (one-way commands) |
| Dry run | `sync --dry-run` | Dry-run mode | `--dry-run` |
| Continuous sync | `sync --watch` (inotify on Linux, kqueue on macOS; remote polling, optional websocket) | Monitor mode (inotify; websocket, or webhooks) | No watch mode; run `bisync` repeatedly, for example from cron |
| First sync with data on both sides | Merges: copies one-sided files, keeps both versions of differing files | Not compared here; see its usage guide | Requires an explicit `--resync` run, with `--resync-mode` choices |
| Conflicts | Both kept; local copy renamed `name.conflict-<timestamp>.ext` | "Safe conflict handling" by keeping local backups | `--conflict-resolve` and `--conflict-loser` options; by default no winner is picked and both are kept, auto-numbered |
| Mass-delete protection | None yet (see [safety](safety.md#known-gaps)) | `classify_as_big_delete` (default 1000 items) | `--max-delete` (default 50%), plus optional `--check-access` sentinel files |
| Local deletes caused by remote deletes | Deleted outright, after checking the file is unchanged | Can use the FreeDesktop.org trash | Deleted, unless `--backup-dir1`/`--backup-dir2` is set |
| Filtering | Per drive: included and ignored folders, glob patterns, dotfiles, junk files | Rules-based, with `*` and `**` wildcards; changing filters requires `--resync` | rclone filter rules |
| On-demand files | No | No | `rclone mount` gives on-demand access through a virtual filesystem (a different model from two-way sync) |

## Using it

| | onedrive-go | abraunegg/onedrive | rclone |
| --- | --- | --- | --- |
| File commands without syncing | `ls`, `get`, `put`, `rm`, `mkdir`, `stat`, `mv`, `cp` | A few options, such as `--create-directory`, `--remove-directory` and `--source-directory` (move) | Extensive (`ls`, `copy`, `move`, `delete`, `mount`, and many more) |
| Sign-in | Device code (default) or browser | Browser flow (default), device flow for Entra ID, Intune SSO | Browser flow during `rclone config` |
| Machine-readable output | `--json` (every command honors or refuses it); JSON lines from `sync` | Not covered in its README | `--use-json-log`, `lsjson` |
| Service integration | systemd `Type=notify` unit with readiness, status line, watchdog and reload; exit codes for restart policy | systemd and init.d files in `contrib/` | Run from cron, systemd or similar |
| Bandwidth limits | No | Yes | Yes (`--bwlimit`) |
| Desktop integration | None | Desktop notifications, file manager sidebar entry | None (third-party GUIs exist) |

## Choosing

- **abraunegg/onedrive** is the established Linux sync client: released,
  packaged, widely used, with national-cloud support, a local trash for
  remote-caused deletes and a big-delete safeguard.
- **rclone** fits when OneDrive is one of several storage services, when you
  want on-demand access through `rclone mount`, or when one-way copies and
  file commands matter more than a continuously running two-way sync.
- **onedrive-go** offers two-way sync plus direct file commands in one tool,
  on Linux and macOS, with systemd and scripting integration. It is
  pre-release: read [how it keeps files safe](safety.md), including the known
  gaps, before trusting it with data you cannot restore.

Whichever you choose, do not point two sync clients at the same folder.
