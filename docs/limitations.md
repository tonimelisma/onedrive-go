# Limitations and known issues

What onedrive-go does not do, and OneDrive behavior you may notice. Data-safety
gaps that are being fixed are listed separately in
[safety.md](safety.md#known-gaps).

## Not supported

- **Windows.** Out of scope: Microsoft ships a native client.
- **Files On-Demand.** Every synced file is downloaded in full. Use
  `included_dirs` or the filters to sync less.
- **National clouds** (Microsoft Cloud for US Government, operated by 21Vianet
  in China). onedrive-go only talks to Microsoft's global endpoints.
- **Client-side encryption.** Out of scope; use rclone crypt, Cryptomator, or
  disk encryption.
- **A GUI.** onedrive-go is a command-line tool.

## Not yet available

- Release binaries and packages (Homebrew, deb, rpm, AUR, containers). Build
  from source for now.
- A launchd example for macOS, and a `service install` command for either
  platform. A systemd unit is provided; see
  [running as a service](running-as-a-service.md).
- Migrating configuration from other clients. Their sign-in tokens cannot be
  reused in any case, so you sign in again with `onedrive-go login`.
- Bandwidth limits.
- Performance measurements. Performance has not been benchmarked, so no speed
  or memory claims are made.

## Command limits

- **Recycle bin commands** (`recycle-bin list`, `restore`, `empty`) work on
  work or school and SharePoint drives only. Microsoft's API does not offer
  the recycle bin for personal accounts; use the OneDrive website there.
- **`cp` is a server-side copy** within OneDrive.
- **Local deletes do not use a trash folder.** When a file is deleted on
  OneDrive, the local copy is deleted outright (after checking it is
  unchanged). Restore from the OneDrive recycle bin if needed.
- **Hard links, ACLs and extended attributes** are not preserved; OneDrive
  has no equivalent. Each hard-linked name is synced as a separate file.

## OneDrive's own rules

- **File size:** at most 250 GB per file.
- **Name and path length:** OneDrive allows at most 400 characters for the
  full path, and for any single name; in practice a few dozen fewer. Longer
  paths are reported in `status`. Separately, your computer's filesystem
  usually allows at most 255 bytes per name, so a long OneDrive name may not
  fit locally (also reported in `status`).
- **Names:** OneDrive rejects `" * : < > ? / \ |`, names ending in a period,
  names starting or ending with a space, names containing `_vti_`, and
  reserved names such as `CON`, `PRN`, `AUX`, `NUL`, `COM0`–`COM9` and
  `LPT0`–`LPT9` (with any extension). Work, school and SharePoint drives also
  reject `.lock`, `desktop.ini` and names starting with `~$`, and SharePoint
  rejects a root-level `forms` folder. Such files are listed in `status` and
  not uploaded.
- **Letter case:** OneDrive does not distinguish `Report.txt` from
  `report.txt`. Two local names that differ only in case are both held back
  until one is renamed.
- **Timestamps:** OneDrive stores modification times to the whole second, and
  only from 1900-01-01 onward. A file dated earlier cannot keep its date on
  OneDrive; onedrive-go does not yet adjust such dates, so uploading such a
  file can fail with a misleading "not found" error.
- **Emoji in names:** when OneDrive compares names it ignores emoji and other
  characters outside Unicode's Basic Multilingual Plane, so `notes😀.txt` and
  `notes.txt` are the same name there. Keep such files in different folders
  (see [safety](safety.md#known-gaps)).
- **Personal Vault** is never synced.

## OneDrive behavior you may notice

- **Delayed remote changes.** OneDrive's change feed can lag a direct look at
  a file by seconds to minutes, so a change made on the web may need a second
  sync to arrive. `sync --full` lists everything instead.
- **A rename resets the modification time on OneDrive.** Renaming a file,
  on the web or through onedrive-go, sets its OneDrive modification time to
  the time of the rename; the content is unchanged. onedrive-go does not yet
  preserve the original time when it renames, so after a rename the file's
  time on OneDrive differs from the local one.
- **Watch mode notices remote changes every 5 minutes by default**
  (`poll_interval`). `websocket = true` makes it react sooner.
- **Extra versions.** Uploading a file and then setting its modification time
  creates two versions on OneDrive. onedrive-go sets the time as part of the
  upload for large files, but files of 4 MiB or less are still uploaded and
  then updated, so each upload of a small file adds two versions. Re-uploading
  a changed file on work or school drives can also add an extra version
  ([onedrive-api-docs#877](https://github.com/OneDrive/onedrive-api-docs/issues/877)).
- **Files changed by SharePoint.** SharePoint can add metadata to uploaded
  Office and media files, which changes their size and hash. onedrive-go
  records each side's hash separately, so this does not cause repeated
  uploads or downloads.
- **Shared folders from other organizations** may not appear in
  `drive list` or `shared`. Add them with the share link instead:
  `onedrive-go drive add <share-url>`.
