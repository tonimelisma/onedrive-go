# Frequently asked questions

### Can I use onedrive-go alongside another OneDrive client?

Yes, as long as they do not sync the **same local folder**. Two sync clients
on one folder see each other's changes as user edits and can undo or
duplicate each other's work. onedrive-go refuses to start a second sync of
the same folder from the same data directory and warns when it finds another
client's marker (Dropbox, Syncthing, Nextcloud, ownCloud) or a folder under
`~/Library/CloudStorage`, but it cannot detect every other client, so keep
the folders separate. Using several clients on the same OneDrive account,
each with its own folder (or on different computers), is fine.

### Will the first sync delete anything?

No file is deleted on the first sync because it is missing from the other
side. Files that exist on only one side are copied, identical files are left
alone, and files that differ are kept as two copies. Run
`onedrive-go sync --dry-run` to see the exact plan first. See
[how it keeps files safe](safety.md#the-first-sync).

### Where do my files go?

Personal OneDrive syncs to `~/OneDrive`, work or school accounts to
`~/OneDrive - <organization>`, SharePoint libraries to
`~/SharePoint - <site> - <library>`, and shared folders you add to
`~/OneDrive-Shared/<name>`. `login` and `drive add` print the folder; each
drive's `sync_dir` in the [configuration](configuration.md) can change it
before the first sync.

### How do I move my sync folder after it has synced?

1. Stop syncing (`systemctl --user stop onedrive-go`, or Ctrl-C).
2. Move the folder with `mv`.
3. Change `sync_dir` in the config file.
4. Start syncing again. onedrive-go recognizes the folder by a marker it
   keeps on it, so a move within one filesystem needs nothing more. If the
   folder was copied to another filesystem or restored from a backup without
   extended attributes, sync refuses to start and names the fix: run
   `onedrive-go drive rebind-root --drive <drive>`, check that the file
   counts it shows match, and type `REBIND`. Your sync state is kept, so
   files whose content is unchanged are not transferred again.

Pointing `sync_dir` at a new, empty folder by mistake is safe: the folder is
refused as not the one that was synced, and even if it were accepted,
deleting everything on one side is held for confirmation (see
[how it keeps files safe](safety.md#deletes)). Use `drive reset-sync-state`
only when you want to discard the sync state; it makes the next sync merge
both sides like a first sync.

### How do I sync only some folders?

Add `included_dirs = ["Documents", "Projects/2026"]` to the drive's section.
To skip folders or file types instead, use `ignored_dirs` and
`ignored_paths`. See [filters](configuration.md#filters).

### Does it sync folders other people shared with me?

Yes. Shared folders you added to your own OneDrive ("Add shortcut to My
files") are synced automatically, inside the drive that holds the shortcut.
Any other shared folder can be added as its own drive with
`onedrive-go drive add "<folder name>"` or `onedrive-go drive add <share-link>`.
`onedrive-go shared` lists what is shared with you.

### Does `rm` delete permanently?

No. `rm` moves the item to the OneDrive recycle bin, as do deletes made by
sync. `rm --permanent` skips the recycle bin.

### Why do the `recycle-bin` commands only work on work or school drives?

Microsoft's API offers the recycle bin only for OneDrive for work or school
and SharePoint. On a personal account, restore deleted items from the
OneDrive website.

### Is there Files On-Demand?

No. Every synced file is downloaded in full. Use `included_dirs` or the
filters to keep large folders off the computer.

### Can I import my abraunegg/onedrive or rclone configuration?

Not yet. Configuration migration is planned but not built, and sign-in tokens
cannot be carried over in any case (each client is registered with Microsoft
separately), so you sign in once with `onedrive-go login`. If you switch
clients, give onedrive-go its own empty folder or stop the old client first.

### Does it work on a server without a desktop?

Yes. `login` uses Microsoft's device-code sign-in by default: it prints a code
and a web address, and you can sign in from any other device. Then run
`sync --watch` under systemd; see [running as a service](running-as-a-service.md).

### How quickly are changes synced?

Local changes are noticed as they happen in `sync --watch` (with a full
rescan every 5 minutes as a safety net, every 2 minutes when the sync folder
is on a network or FUSE filesystem). Remote changes are fetched every
`poll_interval`, 5 minutes by default; `websocket = true` makes OneDrive
notify onedrive-go sooner.

### Can I sync more than one account?

Yes. Run `onedrive-go login` once per account. `sync` then syncs every
configured drive that is not paused; use `--drive` to pick one.

### Does it work behind a proxy?

Yes. It uses the standard `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY`
environment variables.

### Where does onedrive-go send my data?

Only to Microsoft: sign-in goes to Microsoft's login service and everything
else to Microsoft Graph and OneDrive's storage servers. There is no telemetry.
Sign-in tokens are stored on your computer with owner-only permissions; see
[SECURITY.md](../SECURITY.md).
