# Troubleshooting

Start with `onedrive-go status`. It lists every account and drive, says
whether each is syncing, paused or needs attention, and groups problems with a
reason and an action, for example:

```text
    Issues:
      Invalid filename: 1 item
        OneDrive does not allow these names: they contain a forbidden character, are reserved, or end in a space or period.
        Action: Rename the files so OneDrive accepts their names.

        /invalid:name.txt
```

`status` shows a few example paths per problem; `status --verbose` shows all
of them. `status --json` gives the same information for scripts (see
[scripting](scripting.md)).

## Problems `status` reports

Most of these clear by themselves once the cause is fixed: onedrive-go retries
them on later passes, and `sync --watch` keeps trying in the background.

| Shown as | What it means | What to do |
| --- | --- | --- |
| Sign-in required | The saved login is missing, unreadable, expired or was revoked, or OneDrive rejected the last sync. | Run `onedrive-go login` (with `--account` if you have several). |
| Storage full | The OneDrive storage quota for this drive (or the owner's, for a shared folder) is full. | Free up space on OneDrive, or ask the shared folder's owner to. |
| Service outage | OneDrive is temporarily unavailable. | Nothing; onedrive-go retries automatically. |
| Rate limited | OneDrive asked onedrive-go to slow down. | Nothing; it waits for the time OneDrive asked for, then retries. |
| Shared folder writes blocked | A folder shared with you is read-only for you. Downloads continue. | Undo or ignore your local changes there, or ask the owner for edit access. |
| Remote read blocked | You can no longer download this shared content. | Restore access, or remove it from the sync scope with a filter. |
| Local read blocked | A local file or folder cannot be read. | Fix its permissions (for example with `chmod`) so it can be uploaded. |
| Local write blocked | onedrive-go cannot create, rename or update a local path. | Fix the permissions of the folder. |
| Invalid filename | The name contains a character OneDrive forbids (`" * : < > ? / \ \|`), is reserved (such as `CON`, `NUL`, `desktop.ini` on work drives), contains `_vti_`, or starts or ends with a space or ends with a period. | Rename the file. |
| Path too long | The full path is longer than OneDrive allows: 400 characters, in practice a few dozen fewer. | Shorten folder or file names. |
| File too large | The file is larger than OneDrive accepts (250 GB). | Move it out of the sync folder, or filter it out. |
| Case collision | Two local names differ only in letter case, which OneDrive cannot tell apart. Neither is uploaded. | Rename one of them. |
| Disk full | Free local space is below `min_free_space` (1 GB by default). Downloads are paused. | Free up space, or lower `min_free_space`. |
| File too large for available space | This file would not fit in the free space. | Free up space. |
| Name too long for this computer | A OneDrive name is longer than your filesystem allows (255 bytes per name; fewer characters for accented or non-Latin names). | Rename the item on OneDrive. |
| Hash error | Hashing a local file failed unexpectedly. | Check the file and the disk; it is retried. |
| Sync issue | Something unexpected. | Check the logs (below), and run `status` again after the next sync. |

## Sign-in required (exit 77)

Commands exit with status 77 when no saved login exists or Microsoft no longer
accepts it (it expired after a long time unused, or was revoked):

```bash
onedrive-go login                         # one account
onedrive-go login --account user@example.com
```

If the device-code sign-in is blocked by your organization, try
`onedrive-go login --browser`. A systemd unit stops restarting on exit 77; run
`systemctl --user restart onedrive-go` after signing in.

## Invalid configuration (exit 78)

Status 78 means the configuration cannot be used as it is: the file cannot be
read or parsed, a value is out of range, a key is unknown or misspelled, two
drives' sync folders overlap, `--drive` matches no configured drive, or no
drives are configured. The error names the file, the line where possible, and
the key. Fix it and run the command again; see
[configuration](configuration.md) for every key.

`status`, `drive list` and `shared` still run with a broken configuration and
show the problem as a warning.

## "refusing to run a development build"

Your binary was built without a version, which makes it a development build
that refuses to touch real data. Rebuild it with the `-ldflags` shown under
[Building from source](../README.md#building-from-source).

## The sync folder is missing

`sync` never creates a missing `sync_dir`: a folder that is gone might be an
unmounted disk, and syncing an empty stand-in would be dangerous. The drive is
reported as unavailable and the other drives continue. Create the folder (or
mount the disk), or point `sync_dir` at the right place. `login` and
`drive add` create the folder when they set a drive up.

## "another onedrive-go sync is already running"

Only one `sync` or `sync --watch` runs per configuration. Stop the other one
(for example `systemctl --user stop onedrive-go`) or wait for it to finish.
Commands such as `status`, `ls` and `pause` work alongside a running sync.

## The sync state database cannot be used

If a drive's state database is from an incompatible version or cannot be
opened, sync stops for that drive and prints the command to reset it:

```bash
onedrive-go drive reset-sync-state --drive personal:user@example.com
```

This deletes only onedrive-go's record of what was synced. Your files on both
sides are kept, and the next sync merges them like a first sync (identical
files are recognized; differing ones become conflict copies).

## Changes on OneDrive are slow to arrive

- `sync --watch` asks OneDrive for changes every `poll_interval` (5 minutes by
  default). Set `websocket = true` to be notified sooner, or lower
  `poll_interval` (minimum 30 seconds).
- OneDrive's change feed can lag a direct look at a file by seconds to
  minutes. `onedrive-go sync --full` lists everything instead of relying on
  the change feed.

## "Too many folders to watch" (Linux)

Watch mode uses inotify, which has a per-user limit on watched folders. When a
tree comes close to it, onedrive-go warns; when it is reached, new folders are
noticed only by the periodic rescan (every 5 minutes). Raise the limit:

```bash
sudo sysctl -w fs.inotify.max_user_watches=524288
# make it permanent:
echo fs.inotify.max_user_watches=524288 | sudo tee /etc/sysctl.d/90-onedrive-go.conf
```

On macOS, watching uses kqueue, which keeps one open file per watched file
and folder; very large trees can run into the open-files limit (`ulimit -n`).

## Network problems and proxies

When OneDrive cannot be reached, onedrive-go retries with backoff and says so
("can't reach OneDrive, retrying"). It uses the standard `HTTPS_PROXY`,
`HTTP_PROXY` and `NO_PROXY` environment variables.

## Getting more detail

- `-v`/`--verbose` shows more on the console; `--debug` adds every HTTP
  request.
- `log_file = "<path>"` in the config also writes structured JSON logs, at the
  level set by `log_level`.
- Under systemd: `journalctl --user -u onedrive-go -p warning` shows warnings
  and errors only.

Logs at debug level contain file names, paths and account details. Check them
before sharing, and replace personal details (for example with
`user@example.com`). Then open an
[issue](https://github.com/tonimelisma/onedrive-go/issues).
