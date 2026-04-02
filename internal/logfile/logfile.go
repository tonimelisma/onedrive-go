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
		cleanOld(root, retentionDays)
	}

	file, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	return file, nil
}

// cleanOld deletes *.log files in dir that are older than retentionDays.
func cleanOld(root *fsroot.Root, retentionDays int) {
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)

	entries, err := root.ReadDir("")
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
			removeErr := root.Remove(entry.Name())
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "warning: removing old log file %s: %v\n", entry.Name(), removeErr)
			}
		}
	}
}
