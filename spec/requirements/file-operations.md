# R-1 File Operations

Direct API calls against OneDrive. No sync database involved. Work regardless of whether sync is running.

## R-1.1 List (`ls`) [implemented]

When the user runs `ls [path]`, the system shall display remote file and folder names with size, modification time, and type.

- R-1.1.1: When `--json` is passed, the system shall output structured JSON. [implemented]
- R-1.1.2: When the path is a folder, the system shall list its children. [implemented]
- R-1.1.3: When the listing exceeds one page, the system shall paginate automatically. [implemented]

## R-1.2 Download (`get`) [implemented]

When the user runs `get <remote> [local]`, the system shall download the specified file or folder.

- R-1.2.1: When the remote path is a folder, the system shall download recursively. [implemented]
- R-1.2.2: When the download is interrupted, the system shall resume via `.partial` files on retry. [implemented]
- R-1.2.3: When download completes, the system shall verify hash and size against API metadata. [implemented]

## R-1.3 Upload (`put`) [implemented]

When the user runs `put <local> [remote]`, the system shall upload the specified file or directory.

- R-1.3.1: When the local path is a directory, the system shall upload recursively. [implemented]
- R-1.3.2: When the file exceeds 4 MiB, the system shall use a resumable upload session. [implemented]
- R-1.3.3: When upload completes, the system shall verify the server-reported hash matches the local file. [implemented]

## R-1.4 Delete (`rm`) [implemented]

When the user runs `rm <path>`, the system shall delete the item (to recycle bin by default).

- R-1.4.1: When the path is a folder, the system shall delete recursively. [implemented]

## R-1.5 Create Folder (`mkdir`) [implemented]

When the user runs `mkdir <path>`, the system shall create the folder on OneDrive.

## R-1.6 Metadata (`stat`) [implemented]

When the user runs `stat <path>`, the system shall display item metadata (ID, size, hashes, timestamps, parent, eTag, download URL).

## R-1.7 Move (`mv`) [implemented]

When the user runs `mv <src> <dst>`, the system shall perform a server-side move/rename.

## R-1.8 Copy (`cp`) [implemented]

When the user runs `cp <src> <dst>`, the system shall perform a server-side async copy with polling until complete.

## R-1.9 Recycle Bin [implemented]

- R-1.9.1: When the user runs `recycle-bin list`, the system shall list items in the recycle bin. [implemented]
- R-1.9.2: When the user runs `recycle-bin restore <id>`, the system shall restore the item. [implemented]
- R-1.9.3: When the user runs `recycle-bin empty`, the system shall permanently delete all recycled items. [implemented]
