// Package logfile manages log file creation and rotation for structured logging.
package logfile

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/fsroot"
)

// File permission constants.
const (
	dirPerm  = 0o755
	filePerm = 0o644

	// logExt is the suffix a rotated log keeps.
	logExt = ".log"
)

// Open creates or opens a log file at the given path in append mode.
// Parent directories are created if they don't exist. If retentionDays > 0,
// old *.log files in the same directory are deleted.
func Open(path string, retentionDays int) (*os.File, error) {
	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return nil, fmt.Errorf("open log root: %w", err)
	}

	if mkdirErr := root.MkdirAll(dirPerm); mkdirErr != nil {
		return nil, fmt.Errorf("create log directory: %w", mkdirErr)
	}

	if retentionDays > 0 {
		cleanOwnOldLogs(root, name, retentionDays)
	}

	file, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	if chmodErr := file.Chmod(filePerm); chmodErr != nil {
		closeErr := file.Close()
		return nil, errors.Join(fmt.Errorf("set log file permissions: %w", chmodErr), closeErr)
	}

	return file, nil
}

// cleanOwnOldLogs deletes this log file and its rotations when they are older
// than retentionDays.
//
// Scoping matters here. log_file is an arbitrary path the user chooses and has
// no default, so the directory it lands in is very likely shared -- a home
// logs directory, or a system one. Deleting every *.log there would destroy
// files this program never created and knows nothing about, which is not
// retention but collateral damage.
func cleanOwnOldLogs(root *fsroot.Root, logName string, retentionDays int) {
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)

	entries, err := root.ReadDir("")
	if err != nil {
		return // best-effort cleanup
	}

	for _, entry := range entries {
		if entry.IsDir() || !ownedLogName(logName, entry.Name()) {
			continue
		}

		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			removeErr := root.Remove(entry.Name())
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				continue
			}
		}
	}
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
