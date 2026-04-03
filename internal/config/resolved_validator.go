package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ValidateResolved checks cross-field constraints on a fully resolved drive.
// Unlike Validate(), which checks raw config file values, this runs after the
// four-layer override chain (defaults -> file -> env -> CLI) has been applied.
// It catches constraints that only make sense on the final merged result.
func ValidateResolved(rd *ResolvedDrive) error {
	return validateResolvedWithIO(rd, defaultConfigIO())
}

func validateResolvedWithIO(rd *ResolvedDrive, io configIO) error {
	return newResolvedDriveValidator(io).validate(rd)
}

// ValidateResolvedForSync checks sync-specific constraints on a resolved drive.
// Unlike ValidateResolved() which checks general resolved-drive invariants,
// this enforces that sync_dir is set and valid — required only for the sync
// command, not for file operations like ls/get/put.
func ValidateResolvedForSync(rd *ResolvedDrive) error {
	return validateResolvedForSyncWithIO(rd, defaultConfigIO())
}

func validateResolvedForSyncWithIO(rd *ResolvedDrive, io configIO) error {
	return newResolvedDriveValidator(io).validateForSync(rd)
}

// resolvedDriveValidator owns post-override validation so resolve-time path
// checks live next to the IO boundary they depend on.
type resolvedDriveValidator struct {
	io configIO
}

func newResolvedDriveValidator(io configIO) resolvedDriveValidator {
	return resolvedDriveValidator{io: io}
}

func (v resolvedDriveValidator) validate(rd *ResolvedDrive) error {
	var errs []error

	if rd.SyncDir != "" && !filepath.IsAbs(rd.SyncDir) {
		errs = append(errs, fmt.Errorf("sync_dir: must be absolute after expansion, got %q", rd.SyncDir))
	}

	if rd.SyncDir != "" {
		info, err := v.io.statLocalPath(rd.SyncDir)
		switch {
		case err == nil && !info.IsDir():
			errs = append(errs, fmt.Errorf("sync_dir: %q exists but is not a directory", rd.SyncDir))
		case err != nil && !errors.Is(err, os.ErrNotExist):
			errs = append(errs, fmt.Errorf("sync_dir: %w", err))
		}
	}

	return errors.Join(errs...)
}

func (v resolvedDriveValidator) validateForSync(rd *ResolvedDrive) error {
	if rd.SyncDir == "" {
		return fmt.Errorf("drive %q has no sync_dir — set sync_dir in config or run 'drive add %s'",
			rd.CanonicalID.String(), rd.CanonicalID.String())
	}

	if !filepath.IsAbs(rd.SyncDir) {
		return fmt.Errorf("drive %q sync_dir must be absolute, got %q",
			rd.CanonicalID.String(), rd.SyncDir)
	}

	info, err := v.io.statLocalPath(rd.SyncDir)
	switch {
	case err == nil && !info.IsDir():
		return fmt.Errorf("drive %q sync_dir %q exists but is not a directory",
			rd.CanonicalID.String(), rd.SyncDir)
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("drive %q sync_dir: %w", rd.CanonicalID.String(), err)
	}

	return nil
}
