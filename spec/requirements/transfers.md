# R-5 Transfers

Download and upload transfer infrastructure shared by file operations and sync.

## R-5.1 Parallel Transfers [implemented]

- R-5.1.1: The system shall use configurable concurrent transfer workers (`transfer_workers`, default: `runtime.NumCPU()`, minimum 4). [implemented]
- R-5.1.2: The system shall use a separate pool of check workers for local hash computation (`check_workers`). [implemented]

## R-5.2 Resumable Transfers [implemented]

- R-5.2.1: When a file exceeds 4 MiB, the system shall use a resumable upload session. [implemented]
- R-5.2.2: When a download is interrupted, the system shall resume via `.partial` files and HTTP Range requests on retry. [implemented]
- R-5.2.3: Upload session state shall be persisted to disk and survive process restart. [implemented]
- R-5.2.4: Interrupted transfers shall resume automatically on next sync. [implemented]

## R-5.3 Upload Chunking [implemented]

- R-5.3.1: Files <= 4 MiB shall use simple PUT upload (single request). [implemented]
- R-5.3.2: Files > 4 MiB shall use resumable upload sessions with configurable chunk size. [planned]
- R-5.3.3: Chunk size shall be a multiple of 320 KiB (API requirement). [implemented]

## R-5.4 Bandwidth Limiting [future]

- R-5.4.1: The system shall support a global bandwidth limit (`bandwidth_limit`). [future]
- R-5.4.2: The system shall support time-of-day bandwidth scheduling (`bandwidth_schedule`). [future]

## R-5.5 Transfer Validation [implemented]

- R-5.5.1: After download, the system shall verify hash and size against API metadata. [implemented]
- R-5.5.2: After upload, the system shall verify the server-reported hash matches the local file. [implemented]
- R-5.5.3: Validation shall be individually disableable via `disable_download_validation` and `disable_upload_validation`. [implemented]
