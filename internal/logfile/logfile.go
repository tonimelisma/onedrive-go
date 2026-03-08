// Package logfile manages log file creation and rotation for structured logging.
package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// File permission constants.
const (
	dirPerm  = 0o755
	filePerm = 0o644
)

// Open creates or opens a log file at the given path in append mode.
// Parent directories are created if they don't exist. If retentionDays > 0,
// old *.log files in the same directory are deleted.
func Open(path string, retentionDays int) (*os.File, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, err
	}

	if retentionDays > 0 {
		cleanOld(dir, retentionDays)
	}

	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
}

// cleanOld deletes *.log files in dir that are older than retentionDays.
func cleanOld(dir string, retentionDays int) {
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return // best-effort cleanup
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}

		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}
