# Configuration

onedrive-go reads one TOML file. `login` creates it, with every global setting
present as a commented-out default, and adds a section for your main drive;
`drive add` adds more sections. You only need to edit it to change a default or
add filters.

The engineering reference for the same schema is the
[Field Reference in spec/design/config.md](../spec/design/config.md#field-reference).

## Where the file is

| Platform | Config file |
| --- | --- |
| Linux | `~/.config/onedrive-go/config.toml` (or `$XDG_CONFIG_HOME/onedrive-go/config.toml`) |
| macOS | `~/Library/Application Support/onedrive-go/config.toml` (or `$XDG_CONFIG_HOME/onedrive-go/config.toml`) |

Other files onedrive-go keeps:

| What | Linux | macOS |
| --- | --- | --- |
| Sign-in tokens, sync state databases, control socket | `~/.local/share/onedrive-go/` | `~/Library/Application Support/onedrive-go/` |
| Cache | `~/.cache/onedrive-go/` | `~/Library/Caches/onedrive-go/` |

`XDG_DATA_HOME` and `XDG_CACHE_HOME` override the data and cache locations on
both platforms.

## Choosing a different file or drive

| Setting | How |
| --- | --- |
| Config file | `--config <path>`, or the `ONEDRIVE_GO_CONFIG` environment variable |
| Drive for a command | `--drive <selector>`, or the `ONEDRIVE_GO_DRIVE` environment variable |

Settings are resolved in this order, later ones winning: built-in defaults,
then the config file, then the environment, then command-line flags. A flag
replaces the config value; it is never merged with it.

A drive selector is a canonical ID (`personal:user@example.com`), a display
name (any letter case), or part of one. If it matches more than one drive the
command fails and lists the candidates.

## File layout

Global settings come first, without a section header. Each drive then has a
section named by its canonical ID, in quotes:

```toml
# Global settings
transfer_workers = 8
min_free_space = "2GB"
log_file = "~/.local/state/onedrive-go.log"

["personal:user@example.com"]
sync_dir = "~/OneDrive"
ignore_junk_files = true
ignored_dirs = ["node_modules", "Videos/Raw"]
ignored_paths = ["*.iso", "Photos/*.tmp"]

["business:user@contoso.com"]
sync_dir = "~/OneDrive - Contoso"
included_dirs = ["Projects", "Shared/Templates"]
```

Canonical IDs look like this:

| Drive type | Canonical ID |
| --- | --- |
| Personal OneDrive | `personal:<email>` |
| Work or school OneDrive | `business:<email>` |
| SharePoint library | `sharepoint:<email>:<site>:<library>` |
| Folder shared with you | `shared:<email>:<drive-id>:<item-id>` |

You do not need to write these by hand: `login`, `drive add` and
`drive search` print them.

## Global settings

| Key | Type | Default | Allowed values | What it does |
| --- | --- | --- | --- | --- |
| `transfer_workers` | integer | `8` | 4 to 64 | How many uploads and downloads run at once, in `sync` and in `get`/`put`. |
| `check_workers` | integer | `4` | 1 to 16 | How many local files are hashed at once while scanning. |
| `min_free_space` | size | `"1GB"` | a size; `"0"` turns the check off | Free local disk space to keep. Downloads stop while free space is below it, and a file that would take free space below it is reported instead of downloaded. |
| `poll_interval` | duration | `"5m"` | at least `"30s"` | How often `sync --watch` asks OneDrive for remote changes. |
| `websocket` | boolean | `false` | | In `sync --watch`, also listen for OneDrive change notifications so remote changes are noticed sooner. Polling continues as a fallback. |
| `dry_run` | boolean | `false` | | Make one-shot `sync` preview-only by default. `sync --watch` refuses to start while this is on. |
| `log_level` | string | `"info"` | `debug`, `info`, `warn`, `error` | How much goes into the log file. The console is controlled by `--verbose`, `--debug` and `--quiet`. |
| `log_file` | path | `""` (no log file) | a path | Also write structured JSON logs to this file. |
| `log_format` | string | `"auto"` | `auto`, `text`, `json` | Console log format. `auto` is plain text for people, with journal priorities under systemd; `text` forces key=value lines and `json` forces JSON. |
| `log_retention_days` | integer | `30` | 1 or more | Rotated log files older than this are deleted. |

**Sizes** are a number with an optional unit: `B`, `KB`, `MB`, `GB`, `TB`
(powers of 1000) or `KiB`, `MiB`, `GiB`, `TiB` (powers of 1024). A bare number
is bytes. **Durations** use Go syntax: `"90s"`, `"5m"`, `"1h30m"`.

## Drive settings

| Key | Type | Default | What it does |
| --- | --- | --- | --- |
| `sync_dir` | path | chosen at `login` or `drive add` (see below) | The local folder this drive syncs with. Must be an absolute path or start with `~/`. If it exists it must be a directory. `sync` does not create a missing folder; it reports the drive as unavailable. The folder must be the one the drive's sync state was built from; after moving or restoring it, see `drive rebind-root` in [safety](safety.md#previewing-and-starting-over). |
| `included_dirs` | list of folders | empty (whole drive) | Sync only these folders (and what is inside them). |
| `ignored_dirs` | list of folders | empty | Never sync these folders or anything inside them. |
| `ignored_paths` | list of patterns | empty | Never sync files or folders matching these patterns. A matching folder excludes everything inside it. |
| `ignore_dotfiles` | boolean | `false` | Skip every file or folder whose name starts with `.`, and everything inside such a folder. |
| `ignore_junk_files` | boolean | `true` | Skip OS, editor and browser junk on both sides: `.DS_Store`, `Thumbs.db`, `__MACOSX`, `.Spotlight-V100`, `.Trashes`, `.fseventsd`, `.TemporaryItems`, `.Trash-*`, `$RECYCLE.BIN`, `Icon\r`, names starting with `._`, `.~`, `~` (including Office `~$` owner files), `#...#` or `.#`, names ending in `~`, `.tmp`, `.swp`, `.swo`, `.swx`, `.partial`, `.crdownload`, `___jb_tmp___` or `___jb_old___`, the Vim probe file `4913`, and the `-wal`, `-shm` and `-journal` sidecars of a SQLite database whose name has an extension. The `~...`, `...~` and `4913` rules apply to files only, so a folder named `~Projects` syncs. Set to `false` to sync everything. |
| `follow_symlinks` | boolean | `false` | By default symbolic links in the sync folder are skipped. When true, the content a link points to is synced at the link's path, with protection against loops. |
| `cross_filesystems` | boolean | `false` | By default a folder inside the sync folder that is the mount point of another disk or filesystem is not entered; `status` reports it as a local read boundary and nothing under it is synced or deleted on either side. When true, such folders are synced, and onedrive-go remembers them so that a disk that is later not mounted reads as unavailable rather than as every file in it deleted. |
| `display_name` | string | derived from the drive | The name `status` and `drive list` show, and that `--drive` can match. |
| `owner` | string | empty | For shared folders, the owner shown in `status`. Written by `drive add`. |
| `paused` | boolean | not paused | Written by `pause` and `resume`. |
| `paused_until` | timestamp | empty | Written by `pause <duration>`; the drive resumes on its own after this time. |

Default sync folders:

| Drive type | Default `sync_dir` |
| --- | --- |
| Personal | `~/OneDrive` |
| Work or school | `~/OneDrive - <organization>` |
| SharePoint library | `~/SharePoint - <site> - <library>` |
| Shared folder | `~/OneDrive-Shared/<folder name>` |

If that folder is already used by another configured drive, a more specific
name is chosen (with the display name, then the account email added). Two
drives can never share or nest sync folders: overlapping `sync_dir` values are
a configuration error.

## Filters

`included_dirs`, `ignored_dirs` and `ignored_paths` are relative to the drive's
root: use `/` between folders, with no leading or trailing `/`, and no `.` or
`..` components.

- `included_dirs` and `ignored_dirs` name exact folders, without wildcards:
  `"Documents"` or `"Projects/2026"`.
- An `ignored_paths` pattern **without** `/` is matched against every name in
  a path: `"*.iso"` skips ISO files in every folder, and `"build"` skips every
  file or folder called `build`.
- A pattern **with** `/` is matched against the path from the drive root:
  `"Photos/*.tmp"` skips `.tmp` files directly inside `Photos`, not in its
  subfolders.
- Patterns support `*` (any run of characters except `/`), `?` (one
  character) and `[...]` character classes, as in Go's `path.Match`. `**` is
  not supported. A backslash matches the next character literally; write such
  patterns as TOML literal strings, in single quotes, so the backslash reaches
  onedrive-go: `'report\[1\].pdf'`. An invalid pattern is a configuration
  error.
- Ignoring wins over including: a folder listed in `included_dirs` is still
  skipped if an ignore rule matches it.

Filters apply to both sides. A filtered file is neither uploaded nor
downloaded, and filtering it later does not delete it on either side. When a
filter is removed, the files it hid are synced on the next run.

## Changing the configuration

- **Unknown keys are errors.** A misspelled key stops the command with an
  error naming the key, with a "did you mean" suggestion when a valid key is
  close. Commands that only
  display information (`status`, `drive list`, `shared`) report the problem as
  a warning and carry on.
- **A running `sync --watch` reloads by itself.** It checks the file every few
  seconds, and also reloads on `SIGHUP` (`systemctl --user reload onedrive-go`).
  Drives added, removed, paused or resumed take effect immediately.
- **Comments are kept.** onedrive-go edits the file line by line when `login`,
  `drive add`, `pause` and similar commands change it, so your comments and
  layout survive.
- An invalid configuration makes commands exit with status 78; see
  [troubleshooting](troubleshooting.md#invalid-configuration-exit-78).
