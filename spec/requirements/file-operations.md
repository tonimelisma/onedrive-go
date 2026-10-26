# R-1 File Operations

Direct API calls against OneDrive. No sync database involved. Work regardless of whether sync is running.

## R-1.1 List (`ls`) [verified]

When the user runs `ls [path]`, the system shall display remote file and folder names with size, modification time, and type.

- R-1.1.1: When `--json` is passed, the system shall output structured JSON. [verified]
- R-1.1.2: When the path is a folder, the system shall list its children. [verified]
- R-1.1.3: When the listing exceeds one page, the system shall paginate automatically. [verified]

## R-1.2 Download (`get`) [verified]

When the user runs `get <remote> [local]`, the system shall download the specified file or folder.

- R-1.2.1: When the remote path is a folder, the system shall download recursively. [verified]
- R-1.2.2: When the download is interrupted, the system shall resume via `.partial` files on retry. [verified]
- R-1.2.3: When download completes, the system shall verify hash and size against API metadata. [verified]
- R-1.2.4: When `--json` is passed, the system shall output structured JSON with path, size, and hash_verified; for folders, with files array, folders_created, total_size, and errors. [verified]
- R-1.2.5: When the user runs `get <shared-target> [local]`, where `<shared-target>` is either a raw OneDrive share URL or a `shared:<recipientEmail>:<remoteDriveID>:<remoteItemID>` selector, the system shall resolve the underlying shared item and download it without requiring `drive add` first. Shared folder targets shall download recursively by item identity. [verified]
- R-1.2.6: When the local destination of a file `get` ends in a path separator or names an existing directory, the system shall download into that directory under the remote name. When a download fails and keeps a partial file, it shall name that partial file. [verified]

## R-1.3 Upload (`put`) [verified]

When the user runs `put <local> [remote]`, the system shall upload the specified file or directory.

- R-1.3.1: When the local path is a directory, the system shall upload recursively. [verified]
- R-1.3.2: When the file exceeds 4 MiB, the system shall use a resumable upload session. [verified]
- R-1.3.3: When upload completes, the system shall verify the server-reported hash matches the local file. [verified]
- R-1.3.4: When `--json` is passed, the system shall output structured JSON with path, id, and size; for directories, with files array, folders_created, total_size, and errors. [verified]
- R-1.3.5: When the user runs `put <local> <shared-target>`, where `<shared-target>` is either a raw OneDrive share URL or a `shared:<recipientEmail>:<remoteDriveID>:<remoteItemID>` selector, the system shall overwrite that exact shared file by item identity. Shared folder targets shall be rejected with guidance to `drive add` the folder first. [verified]
- R-1.3.6: When a single-file `put` command reports success, the destination path shall already be readable by an immediate follow-on CLI path lookup. [verified]
- R-1.3.7: When the remote destination of a file `put` ends in `/` or names an existing folder, the system shall upload into that folder under the local name, never onto the folder itself. [verified]

## R-1.4 Delete (`rm`) [verified]

When the user runs `rm <path>`, the system shall delete the item (to recycle bin by default; `--permanent` bypasses the recycle bin).

- R-1.4.1: When the path is a folder, the system shall delete recursively. [verified]
- R-1.4.2: Deletions shall go to the OneDrive recycle bin by default. [verified]
- R-1.4.3: When `--json` is passed, the system shall output structured JSON with the deleted path. [verified]
- R-1.4.4: When `rm` reports success, the system shall reconcile transient delete-route `itemNotFound` errors against the target path. Once the target path is proved absent, a bounded non-root parent readability lag may surface as a warning rather than a hard failure. [verified]
- R-1.4.5: `rm` shall refuse the drive root, with or without `--recursive`, before sending any delete request. [verified]

## R-1.5 Create Folder (`mkdir`) [verified]

When the user runs `mkdir <path>`, the system shall create the folder on OneDrive.

- R-1.5.1: When `--json` is passed, the system shall output structured JSON with created path and folder ID. [verified]
- R-1.5.2: When `mkdir` reports success, the created path shall already be readable by an immediate follow-on CLI path lookup. [verified]
- R-1.5.3: When the folder already exists, `mkdir` shall succeed and say that it already exists rather than that it was created. [verified]

## R-1.6 Metadata (`stat`) [verified]

When the user runs `stat <path>`, the system shall display item metadata (ID, size, hashes, timestamps, parent, eTag, download URL).

- R-1.6.1: When `--json` is passed, the system shall output structured JSON with item metadata. [verified]
- R-1.6.2: When the user runs `stat <shared-target>`, where `<shared-target>` is either a raw OneDrive share URL or a `shared:<recipientEmail>:<remoteDriveID>:<remoteItemID>` selector, the system shall resolve the underlying shared item and display metadata for that item without requiring `drive add`. [verified]

## R-1.7 Move (`mv`) [verified]

When the user runs `mv <src> <dst>`, the system shall perform a server-side move/rename.

- R-1.7.1: When `--json` is passed, the system shall output structured JSON with source, destination, and item ID. [verified]
- R-1.7.2: When `mv` reports success, the destination path shall already be readable by an immediate follow-on CLI path lookup. [verified]
- R-1.7.3: `mv` shall refuse to move a folder into itself or its own subtree before sending any request, including the `--force` replacement of an existing destination. `mv`, `cp` and `mkdir` shall print remote paths with a leading `/`. [verified]

## R-1.8 Copy (`cp`) [verified]

When the user runs `cp <src> <dst>`, the system shall perform a server-side async copy with polling until complete.

- R-1.8.1: When `--json` is passed, the system shall output structured JSON with source, destination, and item ID. [verified]
- R-1.8.2: `cp --force` and `mv --force` shall never delete an existing destination before the copy or move has succeeded. The existing file is renamed aside, the operation runs, and then the aside copy goes to the recycle bin on success or gets its name back on any failure, including a copy the monitor reports as failed or a canceled command. [verified]
- R-1.8.3: A completed `cp` of a file shall carry the source's modification time; a failure to set it is a warning, since the copy itself succeeded. [verified]
- R-1.8.4: Every copy-monitor status other than `inProgress`, `notStarted`, `waiting` and `updating` shall end the poll: `completed` as success, anything else (including statuses Microsoft adds later) as a failure, instead of polling until the timeout. [verified]

## R-1.9 Recycle Bin [verified]

- R-1.9.1: When the user runs `recycle-bin list`, the system shall list items in the recycle bin. [verified]
- R-1.9.2: When the user runs `recycle-bin restore <id>`, the system shall restore the item. [verified]
- R-1.9.3: When the user runs `recycle-bin empty`, the system shall permanently delete all recycled items. [verified]
- R-1.9.4: When `--json` is passed, `recycle-bin list` and `recycle-bin restore` shall output structured JSON. [verified]
- R-1.9.5: The recycle-bin help shall say the commands need a work, school or SharePoint drive, because Microsoft Graph offers no recycle bin API for personal accounts. [verified]
