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
| Case collision | Two local names that OneDrive treats as the same (letter case, accent composition or emoji). Neither is uploaded. | Rename one of them. |
| Two OneDrive items, one name here | OneDrive has two items this computer would store under one name. Neither syncs. | Rename one of them on OneDrive. |
| Disk full | Free local space is below `min_free_space` (1 GB by default). Downloads are paused. | Free up space, or lower `min_free_space`. |
| File too large for available space | This file would not fit in the free space. | Free up space. |
| Name too long for this computer | A OneDrive name is longer than your filesystem allows (255 bytes per name; fewer characters for accented or non-Latin names). | Rename the item on OneDrive. |
| Hash error | Hashing a local file failed unexpectedly. | Check the file and the disk; it is retried. |
| Deletes held for confirmation | More was deleted on one side than looks intended (an empty side, or over 50 items and 30 % of the drive). Nothing is deleted; everything else keeps syncing. | Check the folder. If the deletes are right, `onedrive-go sync --confirm-deletes`; if not, restore the files and the hold clears. |
| Another filesystem inside the sync folder | A folder inside the sync folder is a mount point for another filesystem. Sync does not enter it unless `cross_filesystems = true`, so a disk that is not mounted never looks like deleted files. | Move the mount outside the sync folder, or set `cross_filesystems`. |
| Synced item can no longer sync | A synced path now holds a symbolic link, FIFO or device, or a name OneDrive rejects or a filter excludes. Its OneDrive copy is kept. | Rename or move it back; if you meant to remove it, delete it on OneDrive. |
| Database open locally | A download would replace a SQLite database whose `-wal` or `-journal` file is in use. | Close the application; the download is retried. |
| Blocked as malware | OneDrive's scan flagged the file and will not let it be downloaded. | Check it on the OneDrive website; delete it there if it is unwanted. |
| Interrupted removal | A `.onedrive-go.<id>.detach` file was left in the sync folder by a delete or replace that was interrupted. It is your content; sync never deletes it on its own. | Check the file. Rename it back to keep it, or delete it yourself. |
| Folder too large to delete safely | You deleted a folder here, but on OneDrive it holds more items than sync checks before a recursive delete, so it is not deleted there. | Delete the folder on the OneDrive website, or restore it here. |
| Download doesn't match OneDrive / Upload not stored as sent | A transfer did not match OneDrive's hash or size after retries; your file is untouched. | Nothing at first; it is retried. If it keeps failing, check the file on the OneDrive website or upload it again with `put`. |
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

## The sync folder is not the one that was synced

Sync records which folder its state belongs to (a marker in the folder's
extended attributes, with its inode, filesystem and creation time as fallback)
and refuses to run when a different folder is at `sync_dir`: an unmounted
disk's empty mount point, a wiped or restored folder, a re-pointed symbolic
link. The error says whether the folder lacks the `user.onedrive-go.root`
marker or carries a different one. If you moved or restored the folder on
purpose, run `onedrive-go drive rebind-root --drive <drive>`: it shows how
many files the state remembers and how many the folder holds now, and asks
you to type `REBIND` (or pass `--yes`). The sync state is kept; if files are
missing from the folder, the next sync holds their deletes for confirmation
instead of deleting them on OneDrive.

## "another onedrive-go sync is already running"

Only one `sync` or `sync --watch` runs per configuration. Stop the other one
(for example `systemctl --user stop onedrive-go`) or wait for it to finish.
Commands such as `status`, `ls` and `pause` work alongside a running sync.
Each sync folder is also locked per data directory, so a second onedrive-go
sharing the data directory cannot sync the same folder even under a different
path or symbolic link.

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
tree comes close to it, onedrive-go warns. If the limit is already reached
when watching starts (or the inotify instance or open-file limit is), watch
mode keeps syncing by scanning the whole folder every 5 minutes instead, warns
once with the setting to raise, and tries to watch again after 10 minutes,
doubling up to an hour. If the limit is reached for a new folder later, that
folder is picked up by the periodic rescan. Raise the limit:

```bash
sudo sysctl -w fs.inotify.max_user_watches=524288
# make it permanent:
echo fs.inotify.max_user_watches=524288 | sudo tee /etc/sysctl.d/90-onedrive-go.conf
```

On macOS, watching uses kqueue, which keeps one open file per watched file
and folder; when a tree would need more than half of the open-files limit
(`ulimit -n`), watch mode falls back to the periodic rescan in the same way.

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
