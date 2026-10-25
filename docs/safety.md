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
  drives). Only `rm --permanent` and `recycle-bin empty` bypass it.
- **On your computer, deletes are real deletes.** onedrive-go does not move
  files to a local trash. Before deleting a local file because it was deleted
  on OneDrive, it checks the file against the baseline (by hash, or by size
  and modification time when no hash was recorded). If it does not match, your
  file is kept and uploaded again instead.
- **Only synced files are ever deleted because they are missing.** If a file
  is missing on one side and there is no baseline entry for it, it is copied
  over, not deleted.
- **A folder is deleted only after everything inside it.** A local folder that
  still contains anything onedrive-go did not delete itself is not removed
  (with `ignore_junk_files = true`, leftover junk files such as `.DS_Store` do
  not count).

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

## Transfers

- **Downloads are atomic.** A file is downloaded to a temporary name in the
  same folder (a namespaced `.partial` file that onedrive-go never uploads),
  checked against the hash OneDrive reports, and only then renamed over the
  real name. An interrupted download never leaves a half-written file at the
  real name, and large downloads resume where they stopped.
- **Uploads of large files use resumable upload sessions**, and resume after an
  interruption.
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
- **Case collisions.** Two local files whose names differ only in letter case
  cannot both exist on OneDrive, so neither is uploaded until one is renamed.

## One sync at a time

Only one `sync` or `sync --watch` can run per configuration; a second one
refuses to start. `status`, `ls`, `get` and the other commands can run
alongside it. File commands (`ls`, `get`, `put`, `rm`, `mkdir`, `stat`, `mv`,
`cp`) talk to OneDrive directly and never touch the sync state.

## Previewing and starting over

- `sync --dry-run` plans a full sync and prints it without changing any file
  on either side. It may still refresh your sign-in, write logs and update
  internal state files.
- `drive reset-sync-state` deletes a drive's baseline and state database and
  starts it fresh. Your files are not touched; the next sync behaves like a
  first sync and merges both sides.

## Known gaps

onedrive-go is pre-release, and the safety work above is not finished. These
are the behaviors to know about **today**; each is being fixed, and this list
will shrink as fixes land.

| Today | What to do meanwhile |
| --- | --- |
| **An unmounted, emptied or replaced sync folder is not detected.** If `sync_dir` is on a disk or network share that is not mounted, or the folder was emptied or swapped for another one, every synced file looks deleted and the deletes are sent to OneDrive. The same applies to a separate filesystem mounted *inside* the sync folder. | Keep `sync_dir` on a local, always-present disk. If it must be removable or remote, `pause` the drive (or stop the service) before unmounting. Deleted items can be restored from the OneDrive recycle bin. |
| **There is no limit on the number of deletes.** Deleting a large tree on one side deletes it on the other: to the recycle bin on OneDrive, permanently on your computer. | Use `sync --dry-run` before syncing after large changes. Keep a backup of local data. |
| **Some changes that make a file unsyncable look like deletes.** Renaming a synced file to a name OneDrive rejects (for example `report?.docx`), or replacing a synced file or folder with a symbolic link or special file, deletes the original on OneDrive. Some disk read errors other than "permission denied" can do the same. | Check `sync --dry-run` after such changes. |
| **Names containing `%` sequences are decoded.** A local file such as `a%20b.txt` is uploaded correctly but then renamed locally to `a b.txt`. | Avoid `%` followed by two hex digits in names for now. |
| **Some name variants are not detected as collisions.** OneDrive treats `straße` and `STRASSE`, or the composed and decomposed forms of `café`, as the same name; onedrive-go currently detects collisions by simple lower-casing, which misses these pairs, so a second such file can overwrite the first on OneDrive. | Avoid keeping such pairs in one folder. |
| **Upload checks are incomplete.** An edit made on OneDrive while the same file is being uploaded can be overwritten (OneDrive keeps it in version history), and an upload that OneDrive reports as successful is not compared against what was sent. | For files edited in several places at once, check OneDrive's version history after conflicts. |
| **Download checks are incomplete.** If a download keeps failing its hash check, the last attempt is accepted after a few retries; files OneDrive reports without any hash cannot be checked; and a just-downloaded file is not flushed to disk before it is renamed, so a power cut right after a download can leave it empty or truncated. | After a crash or power loss, `drive reset-sync-state` followed by a sync re-checks every file. |
| **Edits that keep the size and modification time are not seen**, because unchanged size plus timestamp is taken to mean unchanged content. | Rare in practice; tools that restore timestamps after editing are the usual cause. |
| **Two sync clients on one folder are not detected.** A second onedrive-go configuration, or another sync tool, pointed at the same folder is not refused. | Do not point two sync clients at the same folder. |
