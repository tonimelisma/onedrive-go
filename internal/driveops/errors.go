package driveops

import (
	"errors"
	"fmt"
)

// MaxOneDriveFileSize is the maximum file size supported by OneDrive uploads
// and sync. Direct uploads reject larger files before hashing or transfer.
const MaxOneDriveFileSize = 250 * 1024 * 1024 * 1024 // 250 GB

// ErrDiskFull is returned when available disk space is below the configured
// min_free_space threshold. In the sync engine, this triggers a disk:local
// block scope (R-2.10.43). In the CLI, it simply fails the download.
var ErrDiskFull = errors.New("local disk full")

// ErrFileExceedsOneDriveLimit is returned when a local upload exceeds
// OneDrive's documented 250 GB maximum file size.
var ErrFileExceedsOneDriveLimit = errors.New("larger than OneDrive's 250 GB maximum file size")

// ErrFileTooLargeForSpace is returned when available disk space is above
// min_free_space but below file size + min_free_space. Per-file failure,
// no scope escalation — smaller files may still fit (R-2.10.44).
var ErrFileTooLargeForSpace = errors.New("not enough disk space for this file")

// ErrPathNotVisible is returned when a path is still not readable after the
// bounded visibility wait (MountSession.WaitPathVisible): after a mutation
// Graph acknowledged, or before one, as put's wait for the folder it uploads
// into. Callers surface this as a concrete degraded result instead of
// pretending the path settled immediately.
var ErrPathNotVisible = errors.New("remote path not yet visible")

// PathNotVisibleError records which remote path exhausted the bounded
// visibility budget while the path was still not found, or its answers still
// disagreed (graph.ErrPathAnswersDisagree). Cause is that disagreement, and
// nil for a path not found. Callers can branch on this typed error for
// user-facing degraded-success handling without weakening other visibility
// failures; it wraps both ErrPathNotVisible and Cause.
type PathNotVisibleError struct {
	Path  string
	Cause error
}

func (e *PathNotVisibleError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %q: %v", ErrPathNotVisible, e.Path, e.Cause)
	}

	return fmt.Sprintf("%s: %q", ErrPathNotVisible, e.Path)
}

func (e *PathNotVisibleError) Unwrap() []error {
	if e.Cause != nil {
		return []error{ErrPathNotVisible, e.Cause}
	}

	return []error{ErrPathNotVisible}
}
