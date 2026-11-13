// Package logfile opens the log file structured logging writes to and removes
// this program's expired logs. It does not rotate logs.
package logfile

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/fsroot"
)

// Logs carry item names and account details, and other local users reading
// them is an actor in the threat model, so the file and any directory this
// package creates for it are owner-only.
const (
	dirPerm  = 0o700
	filePerm = 0o600

	// logExt is the suffix a rotated log keeps.
	logExt = ".log"

	// retentionDay is the unit of log_retention_days: 24 hours back from now,
	// not a calendar day.
	retentionDay = 24 * time.Hour
)

// Open creates or opens the log file at path in append mode, owner-only, and
// creates missing parent directories owner-only.
func Open(path string) (*os.File, error) {
	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return nil, fmt.Errorf("open log root: %w", err)
	}

	if mkdirErr := root.MkdirAll(dirPerm); mkdirErr != nil {
		return nil, fmt.Errorf("create log directory: %w", mkdirErr)
	}

	file, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	if chmodErr := file.Chmod(filePerm); chmodErr != nil {
		chmodErr = fmt.Errorf("set log file permissions: %w", chmodErr)
		if closeErr := file.Close(); closeErr != nil {
			return nil, errors.Join(chmodErr, fmt.Errorf("close log file: %w", closeErr))
		}

		return nil, chmodErr
	}

	return file, nil
}

// RemoveExpired deletes the log file at path and its rotations when they are
// older than retentionDays; retentionDays <= 0 keeps everything. Call it
// before Open: an expired current log is removed, and Open starts it afresh.
//
// Scoping matters here. log_file is an arbitrary path the user chooses and has
// no default, so the directory it lands in is very likely shared -- a home
// logs directory, or a system one. Deleting every *.log there would destroy
// files this program never created and knows nothing about, which is not
// retention but collateral damage.
//
// Every failure to list, check or remove is returned, joined, and none stops
// the others: a log that cannot be removed keeps growing unseen otherwise.
// They are returned rather than logged because this runs before the logger
// that would carry them exists, and they are not Open's failures, because the
// log still works without retention.
func RemoveExpired(path string, retentionDays int) error {
	if retentionDays <= 0 {
		return nil
	}

	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return fmt.Errorf("open log root: %w", err)
	}

	entries, err := root.ReadDir("")
	if errors.Is(err, os.ErrNotExist) {
		return nil // no log folder yet, so no expired logs in it
	}
	if err != nil {
		return fmt.Errorf("list log directory: %w", err)
	}

	return removeExpiredEntries(root, name, entries, time.Now().Add(-time.Duration(retentionDays)*retentionDay))
}

// removeExpiredEntries removes each of entries that is logName or one of its
// rotations and was last written before cutoff, returning every failure
// joined. An entry gone since the listing is not a failure.
func removeExpiredEntries(root *fsroot.Root, logName string, entries []os.DirEntry, cutoff time.Time) error {
	var errs []error

	for _, entry := range entries {
		if entry.IsDir() || !ownedLogName(logName, entry.Name()) {
			continue
		}

		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("check age of log %s: %w", entry.Name(), err))
			continue
		}

		if !info.ModTime().Before(cutoff) {
			continue
		}

		if err := root.Remove(entry.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove expired log %s: %w", entry.Name(), err))
		}
	}

	return errors.Join(errs...)
}

// ownedLogName reports whether candidate is the configured log file or one of
// its rotations. A rotation carries the configured name as a prefix followed
// by a separator, so "onedrive.log" owns "onedrive.log.1" and
// "onedrive-2026-01-01.log" but never a neighboring "backup.log".
func ownedLogName(logName, candidate string) bool {
	if candidate == logName {
		return true
	}

	if strings.HasPrefix(candidate, logName+".") {
		return true
	}

	base := strings.TrimSuffix(logName, logExt)
	if base == logName {
		return false
	}

	return strings.HasPrefix(candidate, base+"-") && strings.HasSuffix(candidate, logExt)
}
