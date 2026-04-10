package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const invalidSizeStr = "abc"

func validConfig() *Config {
	return DefaultConfig()
}

func TestValidate_ValidDefaults(t *testing.T) {
	err := Validate(validConfig())
	assert.NoError(t, err)
}

// Validates: R-4.8.2
func TestValidate_TransferWorkers_BelowMin(t *testing.T) {
	cfg := validConfig()
	cfg.TransferWorkers = 3
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transfer_workers")
}

func TestValidate_TransferWorkers_AboveMax(t *testing.T) {
	cfg := validConfig()
	cfg.TransferWorkers = 65
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transfer_workers")
}

func TestValidate_TransferWorkers_Boundaries(t *testing.T) {
	// Min boundary (4) passes.
	cfg := validConfig()
	cfg.TransferWorkers = 4
	require.NoError(t, Validate(cfg))

	// Max boundary (64) passes.
	cfg = validConfig()
	cfg.TransferWorkers = 64
	assert.NoError(t, Validate(cfg))
}

func TestValidate_CheckWorkers_BelowMin(t *testing.T) {
	cfg := validConfig()
	cfg.CheckWorkers = 0
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "check_workers")
}

func TestValidate_CheckWorkers_AboveMax(t *testing.T) {
	cfg := validConfig()
	cfg.CheckWorkers = 17
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "check_workers")
}

func TestValidate_CheckWorkers_Boundaries(t *testing.T) {
	// Min boundary (1) passes.
	cfg := validConfig()
	cfg.CheckWorkers = 1
	require.NoError(t, Validate(cfg))

	// Max boundary (16) passes.
	cfg = validConfig()
	cfg.CheckWorkers = 16
	assert.NoError(t, Validate(cfg))
}

func TestValidate_DeleteSafetyThreshold_BelowMin(t *testing.T) {
	cfg := validConfig()
	cfg.DeleteSafetyThreshold = 0
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete_safety_threshold")
}

func TestValidate_PollInterval_TooShort(t *testing.T) {
	cfg := validConfig()
	cfg.PollInterval = "10s"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "poll_interval")
}

// Validates: R-4.8.2
func TestValidate_PollInterval_InvalidFormat(t *testing.T) {
	cfg := validConfig()
	cfg.PollInterval = "not-a-duration"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "poll_interval")
}

func TestValidate_SafetyScanInterval_TooShort(t *testing.T) {
	cfg := validConfig()
	cfg.SafetyScanInterval = "5s"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "safety_scan_interval")
}

// Validates: R-4.8.2
func TestValidate_LogLevel_Invalid(t *testing.T) {
	cfg := validConfig()
	cfg.LogLevel = "verbose"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "log_level")
}

func TestValidate_LogLevel_AllValid(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		cfg := validConfig()
		cfg.LogLevel = level
		err := Validate(cfg)
		assert.NoError(t, err, "expected %s to be valid", level)
	}
}

func TestValidate_LogFormat_Invalid(t *testing.T) {
	cfg := validConfig()
	cfg.LogFormat = "xml"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "log_format")
}

func TestValidate_LogFormat_AllValid(t *testing.T) {
	for _, format := range []string{"auto", "text", "json"} {
		cfg := validConfig()
		cfg.LogFormat = format
		err := Validate(cfg)
		assert.NoError(t, err, "expected %s to be valid", format)
	}
}

func TestValidate_LogRetentionDays_BelowMin(t *testing.T) {
	cfg := validConfig()
	cfg.LogRetentionDays = 0
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "log_retention_days")
}

func TestValidate_SyncPaths_MustStartWithSlash(t *testing.T) {
	cfg := validConfig()
	cfg.SyncPaths = []string{"/Documents", "Photos"}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sync_paths")
	assert.Contains(t, err.Error(), "Photos")
}

func TestValidate_IgnoreMarker_Empty(t *testing.T) {
	cfg := validConfig()
	cfg.IgnoreMarker = ""
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ignore_marker")
}

// Validates: R-4.8.2
func TestValidate_MinFreeSpace_Invalid(t *testing.T) {
	cfg := validConfig()
	cfg.MinFreeSpace = invalidSizeStr
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "min_free_space")
}

func TestValidate_MultipleErrors(t *testing.T) {
	cfg := validConfig()
	cfg.TransferWorkers = 0
	cfg.CheckWorkers = 0
	cfg.PollInterval = invalidSizeStr
	cfg.LogLevel = "invalid-value"

	err := Validate(cfg)
	require.Error(t, err)

	errStr := err.Error()
	assert.Contains(t, errStr, "transfer_workers")
	assert.Contains(t, errStr, "check_workers")
	assert.Contains(t, errStr, "poll_interval")
	assert.Contains(t, errStr, "log_level")
}

// --- ValidateResolved tests ---

func TestValidateResolved_SyncDirExistsButIsFile(t *testing.T) {
	// Create a regular file where sync_dir points.
	f := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(f, []byte("data"), 0o600))

	rd := &ResolvedDrive{SyncDir: f}
	err := ValidateResolved(rd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sync_dir")
	assert.Contains(t, err.Error(), "not a directory")
}

func TestValidateResolved_SyncDirDoesNotExist_OK(t *testing.T) {
	// Non-existent path is fine — sync will create it.
	rd := &ResolvedDrive{SyncDir: filepath.Join(t.TempDir(), "nonexistent")}
	err := ValidateResolved(rd)
	assert.NoError(t, err)
}

func TestValidateResolved_AbsoluteSyncDir(t *testing.T) {
	rd := &ResolvedDrive{SyncDir: "/absolute/path"}
	err := ValidateResolved(rd)
	assert.NoError(t, err)
}

func TestValidateResolved_RelativeSyncDir(t *testing.T) {
	rd := &ResolvedDrive{SyncDir: "relative/path"}
	err := ValidateResolved(rd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sync_dir")
	assert.Contains(t, err.Error(), "absolute")
}

func TestValidateResolved_EmptySyncDir(t *testing.T) {
	rd := &ResolvedDrive{SyncDir: ""}
	err := ValidateResolved(rd)
	assert.NoError(t, err)
}

func TestValidateResolved_SyncDirStatError(t *testing.T) {
	rd := &ResolvedDrive{SyncDir: "/tmp/sync-dir"}
	err := validateResolvedWithIO(rd, configIO{
		statLocalPath: func(path string) (os.FileInfo, error) {
			return nil, errors.New("boom")
		},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "sync_dir")
	assert.Contains(t, err.Error(), "boom")
}

// --- ValidateResolvedForSync tests ---

// Validates: R-4.8.6
func TestValidateResolvedForSync_EmptySyncDir(t *testing.T) {
	rd := &ResolvedDrive{
		CanonicalID: driveid.MustCanonicalID("personal:user@example.com"),
		SyncDir:     "",
	}
	err := ValidateResolvedForSync(rd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no sync_dir")
	assert.Contains(t, err.Error(), "personal:user@example.com")
}

// Validates: R-4.8.6
func TestValidateResolvedForSync_RelativeSyncDir(t *testing.T) {
	rd := &ResolvedDrive{
		CanonicalID: driveid.MustCanonicalID("personal:user@example.com"),
		SyncDir:     "relative/path",
	}
	err := ValidateResolvedForSync(rd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be absolute")
}

// Validates: R-4.8.6
func TestValidateResolvedForSync_SyncDirIsFile(t *testing.T) {
	// Create a temp file to simulate sync_dir pointing to a file.
	tmpFile := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(tmpFile, []byte("data"), 0o600))

	rd := &ResolvedDrive{
		CanonicalID: driveid.MustCanonicalID("personal:user@example.com"),
		SyncDir:     tmpFile,
	}
	err := ValidateResolvedForSync(rd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

// Validates: R-4.8.6
func TestValidateResolvedForSync_Valid(t *testing.T) {
	rd := &ResolvedDrive{
		CanonicalID: driveid.MustCanonicalID("personal:user@example.com"),
		SyncDir:     "/tmp/valid-sync-dir",
	}
	err := ValidateResolvedForSync(rd)
	assert.NoError(t, err)
}

// Validates: R-4.8.6
func TestValidateResolvedForSync_NonExistentDir_OK(t *testing.T) {
	// Non-existent paths are valid — sync creates the directory on first run.
	rd := &ResolvedDrive{
		CanonicalID: driveid.MustCanonicalID("personal:user@example.com"),
		SyncDir:     "/nonexistent/path/that/does/not/exist",
	}
	err := ValidateResolvedForSync(rd)
	assert.NoError(t, err)
}

// Validates: R-4.8.6
func TestValidateResolvedForSync_SyncDirStatError(t *testing.T) {
	rd := &ResolvedDrive{
		CanonicalID: driveid.MustCanonicalID("personal:user@example.com"),
		SyncDir:     "/tmp/sync-dir",
	}
	err := validateResolvedForSyncWithIO(rd, configIO{
		statLocalPath: func(path string) (os.FileInfo, error) {
			return nil, errors.New("boom")
		},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "sync_dir")
	assert.Contains(t, err.Error(), "boom")
}
