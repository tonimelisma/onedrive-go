package driveops

import (
	"io/fs"
	"log/slog"
	"path/filepath"
	"time"
)

// ReportStalePartials scans syncRoot for .partial files older than threshold
// and logs them as warnings. Called after sync completes to alert the user
// about potentially abandoned downloads.
func ReportStalePartials(syncRoot string, threshold time.Duration, logger *slog.Logger) {
	var stale []string

	err := filepath.WalkDir(syncRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		if filepath.Ext(path) != ".partial" {
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}

		if time.Since(info.ModTime()) > threshold {
			rel, relErr := filepath.Rel(syncRoot, path)
			if relErr != nil {
				rel = path
			}

			stale = append(stale, rel)
		}

		return nil
	})
	if err != nil {
		logger.Warn("error scanning for stale partials", slog.String("error", err.Error()))
		return
	}

	if len(stale) > 0 {
		logger.Warn("stale .partial files found",
			slog.Int("count", len(stale)),
			slog.String("threshold", threshold.String()),
		)

		for _, p := range stale {
			logger.Warn("stale partial", slog.String("path", p))
		}
	}
}
