# How onedrive-go keeps your files safe

This page explains how onedrive-go decides what to copy, what to delete and
what to keep when both sides have changed, and where its protection currently
ends. The engineering rules behind it are the safety invariants in
[non-functional requirements R-6.2 and R-6.4](../spec/requirements/non-functional.md)
and the decision tables in [sync-planning.md](../spec/design/sync-planning.md).

## The baseline: what was last synced

For every file and folder it has synced, onedrive-go records what both sides
looked like at that moment: the content hash, size, modification time and
OneDrive item ID. This record is the **baseline**, and it lives in a per-drive
state database in the data directory, not in the sync folder.

Each sync compares three things: the baseline, your local folder now, and
OneDrive now. That is what lets it tell "you deleted this file" apart from
"this file is new on OneDrive", and "you edited it" apart from "someone edited
it online".

## The first sync

With no baseline yet, nothing has been synced, so nothing can have been
deleted:

| Situation | What happens |
| --- | --- |
| File only on your computer | Uploaded |
| File only on OneDrive | Downloaded |
| Same file on both, same content | Recorded as synced; nothing is transferred |
| Same name on both, different content | Both kept: OneDrive's copy at the original name, yours renamed to a conflict copy |

Two files are only treated as the same when their content hashes match. Equal
size or equal timestamps are never taken as proof.

Run `onedrive-go sync --dry-run` first to see exactly what the first sync will
do.

## Later syncs

| Your computer | OneDrive | What happens |
| --- | --- | --- |
| Changed | Unchanged | Uploaded |
| Unchanged | Changed | Downloaded |
| Changed | Changed, same content | Recorded as synced |
| Changed | Changed, different content | Conflict: both kept (see below) |
| Deleted | Unchanged | Deleted on OneDrive (to the recycle bin) |
| Deleted | Changed | OneDrive's newer version is downloaded again |
| Unchanged | Deleted | Deleted on your computer, after checking it is unchanged |
| Changed | Deleted | Your version is kept and uploaded again |
| Deleted | Deleted | Forgotten |

Deletes in the last three rows are subject to the delete hold and, on your
computer, go to the local trash (see Deletes below).

A rename or move on one side is applied as a move on the other side when
onedrive-go can prove it is the same item; otherwise it is handled as a delete
plus a new file. When an item changes from a file to a folder (or back) on one
side, it is replaced on the other side; if both sides changed, your local
version wins and the replaced OneDrive item goes to the recycle bin.

## Conflicts

When a file changed differently on both sides, onedrive-go never picks a
winner by overwriting:

- OneDrive's version is downloaded to the original name.
- Your version is renamed next to it, for example
  `report.conflict-20261025-143000.docx` (a name such as `.bashrc` becomes
  `.bashrc.conflict-20261025-143000`).
- The conflict copy is then an ordinary new file, so it is uploaded too, and
  both versions exist on both sides.

Conflicts are handled as soon as they are found; there is no separate command
to resolve them. Delete the copy you do not want.

`--download-only` and `--upload-only` do not change this: a real conflict is
still kept as two files rather than resolved in the direction of the mode.

## Deletes

- **On OneDrive, deletes go to the recycle bin.** Sync, `rm` and folder deletes
  all use OneDrive's recycle bin, where you can restore them from the OneDrive
  website (or with `recycle-bin restore` on work, school and SharePoint
  drives). Only `rm --permanent` and `recycle-bin empty` bypass it;
  `trash empty --confirm` is the local equivalent.
- **On your computer, deletes go to onedrive-go's own trash.** Every file sync
  deletes on your computer, and the previous version of every file a download
  replaces, is moved to a per-drive trash in the data directory and kept for
  30 days. `onedrive-go trash list --drive <drive>` shows it,
  `trash restore <id>` puts a file back where it was (never overwriting
  anything; the next sync uploads it again), and `trash empty --confirm`
  empties it. Before deleting a local file because it was deleted on OneDrive,
  onedrive-go checks the file against the baseline (by hash, or by size and
  modification time when no hash was recorded) and acts on exactly the file it
  checked. If it does not match, or the file is replaced while being checked,
  your file is kept and uploaded again instead.
- **Many deletes at once are held for your confirmation.** If one side has no
  files at all while the last sync recorded some, or if more than 50 files and
  folders, and more than 30 % of what is synced, would be deleted toward one
  side, those deletes (and anything that depends on them) are held. Everything
  else keeps syncing. `status` lists them as "Deletes held for confirmation",
  the sync summary says "Held N deletes for confirmation", and
  `onedrive-go sync --confirm-deletes` applies exactly that set after you have
  looked. The hold is worked out again on every sync, so it lifts by itself if
  the files come back. Renames and moves never count.
- **Only synced files are ever deleted because they are missing.** If a file
  is missing on one side and there is no baseline entry for it, it is copied
  over, not deleted.
- **A folder is deleted only after everything inside it.** A local folder that
  still contains anything onedrive-go did not delete itself is not removed
  (junk files such as `.DS_Store` do not count unless you set
  `ignore_junk_files = false`). A local folder that stays only because it
  holds content a filter excludes is not re-created on OneDrive.

### What never counts as a delete

- **An interrupted OneDrive listing.** Deletes are only worked out from a
  complete listing; a partial one is discarded and fetched again.
- **A folder onedrive-go may not read.** A local or OneDrive folder that
  returns "permission denied" is treated as unknown, so nothing under it is
  deleted on the other side. `status` reports it.
- **A shared folder that became read-only.** Your local changes there are held
  and reported; downloads continue.
- **Filtered paths.** Adding a filter hides files from sync; it does not delete
  them on either side. Removing the filter makes them sync again.
- **A synced file replaced by something sync cannot handle.** A synced path
  now holding a symbolic link, FIFO or device, a file renamed to a name
  OneDrive rejects or to an excluded name, or an entry that cannot be read or
  hashed, is reported in `status` and left alone on OneDrive.
- **A disk mounted inside the sync folder.** A folder that is the mount point
  of another filesystem is not entered (unless `cross_filesystems = true`),
  and a disk that was synced and is later not mounted reads as unavailable,
  not as deleted.
- **An item missing from a full listing.** Before an item that a full listing
  did not return is deleted locally, OneDrive is asked for it by ID; only a
  definite "gone" answer deletes it.
- **A sync folder that is not the one that was synced.** See
  [Previewing and starting over](#previewing-and-starting-over).

## Transfers

- **Downloads are atomic.** A file is downloaded to a temporary name in the
  same folder (a namespaced `.partial` file that onedrive-go never uploads),
  checked against the hash OneDrive reports (or its size when OneDrive has no
  hash), written to disk (`fsync`), and only then renamed over the real name;
  the file it replaces goes to the local trash. A download whose content
  cannot be verified fails after three attempts and leaves your file
  untouched. An interrupted download never leaves a half-written file at the
  real name, and large downloads resume where they stopped, but only while
  OneDrive still has the same version. A file that an application has open as
  a SQLite database is never replaced underneath it.
- **Uploads are verified and conditional.** Every upload goes through a
  resumable session that resumes after an interruption; afterwards the stored
  file is compared with what was sent (hash, or size), and a mismatch fails
  the upload instead of being recorded as synced. Overwrites are conditional
  on the version onedrive-go last saw, so an edit made on OneDrive while the
  upload was in flight makes the upload fail and be replanned rather than
  overwrite the edit. A rename made by onedrive-go keeps the file's
  modification time.
- **Disk space is reserved.** Before each download onedrive-go checks free
  space; below `min_free_space` (1 GB by default) downloads stop and `status`
  says so.
- **Everything is written inside the sync folder.** Local writes go through a
  boundary that refuses `..`, absolute paths and symbolic links that would lead
  out of the sync folder.

## What is skipped

- **Personal Vault.** It locks itself after 20 minutes, and its locked items
  look deleted to an API client; syncing it could delete your vault contents.
- **OneNote notebooks**, which are not ordinary files.
- **Symbolic links** (unless `follow_symlinks = true`), and FIFOs, sockets and
  device files, which are never opened.
- **Names OneDrive does not allow** (such as `CON`, names ending in a period or
  a space, or names with `:` and other reserved characters). They are listed
  in `status` instead of uploaded.
- **Name collisions.** Two local names that OneDrive treats as one (differing
  only in letter case, in how accents are composed, in emoji, or by characters
  OneDrive ignores) cannot both exist there, so neither is uploaded until one
  is renamed; `status` names both. A new file is never created over an
  existing OneDrive item of the same name. Two OneDrive items that this
  computer would store under one name are likewise left alone and reported.

## One sync at a time

Only one `sync` or `sync --watch` can run per configuration; a second one
refuses to start. A second onedrive-go using the same data directory cannot
sync the same folder either, whatever path names it. `status`, `ls`, `get`
and the other commands can run alongside it. File commands (`ls`, `get`,
`put`, `rm`, `mkdir`, `stat`, `mv`, `cp`) talk to OneDrive directly and never
touch the sync state. `sync` refuses to run as root unless `--allow-root` is
given, because files it created would be owned by root.

## Previewing and starting over

- `sync --dry-run` plans a full sync and prints it without changing any file
  on either side. It may still refresh your sign-in, write logs and update
  internal state files.
- `drive reset-sync-state` deletes a drive's baseline and state database and
  starts it fresh. Your files are not touched; the next sync behaves like a
  first sync and merges both sides.
- `drive rebind-root` accepts the folder now at a drive's sync path as its
  sync folder after you moved or restored it on purpose. The sync state is
  bound to the folder it was built from (a marker in the folder's extended
  attributes, with its inode, filesystem and creation time as fallback), and
  a different folder at the path, such as an unmounted disk's empty mount
  point, is refused rather than synced. `drive rebind-root` shows the
  remembered and present file counts and asks you to type `REBIND`. The sync
  state is kept; missing files are held for confirmation rather than deleted
  on OneDrive.

## Known gaps

onedrive-go is pre-release. The protections above are implemented and tested,
but they have been used only in the project's own testing. These are the
limits of the protection **today**.

| Today | What to do meanwhile |
| --- | --- |
| **A folder re-created at the sync path can pass the identity check on a few filesystems.** onedrive-go marks the sync folder with an extended attribute and, where the filesystem has none, falls back to its inode, filesystem ID and creation time. On a filesystem that records neither user extended attributes nor creation times, a folder deleted and re-created at the same path that happens to get the same inode is accepted; the delete hold is then the only guard. | Keep `sync_dir` on a local disk filesystem, or `pause` the drive (or stop the service) before unmounting or restoring it. |
| **Fewer than 50 deletes (or under 30 % of your files) are not held.** The delete hold is for mass loss, not for individual mistakes. | Local deletes can be undone from `trash list` / `trash restore` for 30 days; OneDrive deletes from the recycle bin. |
| **Moving a synced file into a folder you excluded with a filter deletes it on OneDrive** (to the recycle bin). The file is kept on your computer. | Move the file out of the excluded folder, or restore it from the recycle bin. |
| **An edit that keeps a file's size, modification time *and* change time is not seen.** Normal tools cannot do this (even `cp -p` and `touch -r` move the change time); it needs root or a changed clock. | Rare in practice. |
| **A second sync client on the same folder is only partly detected.** A second onedrive-go using the same data directory is refused, and the markers of Dropbox, Syncthing, Nextcloud and ownCloud (or a folder under `~/Library/CloudStorage`) trigger a warning; a second onedrive-go under another user or data directory, or a tool without a marker (rclone, abraunegg/onedrive), is not detected. | Do not point two sync clients at the same folder. |
| **Files OneDrive reports without a hash** (some work, school and SharePoint files) are checked by size only after transfer. | None needed; a size mismatch still fails the transfer. |
| **Concurrent-edit protection on work, school and SharePoint drives is unprobed.** Overwrites are conditional on the file's version (`If-Match`); OneDrive Personal honours this. On other drive types the worst case is that an upload is replanned, never that an edit is overwritten silently. | Check OneDrive's version history after a conflict. |
