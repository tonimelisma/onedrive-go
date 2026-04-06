package driveops

import "errors"

// MaxOneDriveFileSize is the maximum file size supported by OneDrive uploads
// and sync. Direct uploads reject larger files before hashing or transfer.
const MaxOneDriveFileSize = 250 * 1024 * 1024 * 1024 // 250 GB

// ErrDiskFull is returned when available disk space is below the configured
// min_free_space threshold. In the sync engine, this triggers a disk:local
// scope block (R-2.10.43). In the CLI, it simply fails the download.
var ErrDiskFull = errors.New("local disk full")

// ErrFileExceedsOneDriveLimit is returned when a local upload exceeds
// OneDrive's documented 250 GB maximum file size.
var ErrFileExceedsOneDriveLimit = errors.New("file exceeds OneDrive 250 GB limit")

// ErrFileTooLargeForSpace is returned when available disk space is above
// min_free_space but below file size + min_free_space. Per-file failure,
// no scope escalation — smaller files may still fit (R-2.10.44).
var ErrFileTooLargeForSpace = errors.New("insufficient disk space for file")

// ErrPathNotVisible is returned when Graph acknowledged a metadata-changing
// mutation, but the destination path still is not readable after the bounded
// post-success visibility wait. Callers surface this as a concrete degraded
// mutation result instead of pretending the path settled immediately.
var ErrPathNotVisible = errors.New("remote path not yet visible")
