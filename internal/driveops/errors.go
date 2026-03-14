package driveops

import "errors"

// ErrDiskFull is returned when available disk space is below the configured
// min_free_space threshold. In the sync engine, this triggers a disk:local
// scope block (R-2.10.43). In the CLI, it simply fails the download.
var ErrDiskFull = errors.New("local disk full")

// ErrFileTooLargeForSpace is returned when available disk space is above
// min_free_space but below file size + min_free_space. Per-file failure,
// no scope escalation — smaller files may still fit (R-2.10.44).
var ErrFileTooLargeForSpace = errors.New("insufficient disk space for file")
