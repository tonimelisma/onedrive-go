package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	invalidSizeStr = "not-a-size"
	invalidEnumStr = "invalid-value"
)

func validConfig() *Config {
	return DefaultConfig()
}

func loggedAttrValues(t *testing.T, logBuf *bytes.Buffer, key string) []string {
	t.Helper()

	text := strings.TrimSpace(logBuf.String())
	if text == "" {
		return nil
	}

	lines := strings.Split(text, "\n")
	values := make([]string, 0, len(lines))
	for _, line := range lines {
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record))

		value, ok := record[key].(string)
		if ok {
			values = append(values, value)
		}
	}

	return values
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

func TestValidate_BigDeleteThreshold_BelowMin(t *testing.T) {
	cfg := validConfig()
	cfg.BigDeleteThreshold = 0
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "big_delete_threshold")
}

// Validates: R-4.8.2, R-6.2.9
func TestValidate_Permissions_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"too short", "07"},
		{"too long", "07000"},
		{"not octal", "abc"},
		{"above max", "1000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.SyncDirPermissions = tt.value
			err := Validate(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "sync_dir_permissions")
		})
	}
}

// Validates: R-4.8.2, R-6.2.9
func TestValidate_Permissions_Valid(t *testing.T) {
	for _, perm := range []string{"0600", "0700", "0755", "0644", "777"} {
		cfg := validConfig()
		cfg.SyncDirPermissions = perm
		cfg.SyncFilePermissions = perm
		err := Validate(cfg)
		assert.NoError(t, err, "expected %s to be valid", perm)
	}
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

func TestValidate_ShutdownTimeout_TooShort(t *testing.T) {
	cfg := validConfig()
	cfg.ShutdownTimeout = "1s"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shutdown_timeout")
}

func TestValidate_ConflictStrategy_Invalid(t *testing.T) {
	cfg := validConfig()
	cfg.ConflictStrategy = "keep_remote"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflict_strategy")
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
	cfg.ConflictStrategy = invalidEnumStr
	cfg.LogLevel = invalidEnumStr

	err := Validate(cfg)
	require.Error(t, err)

	errStr := err.Error()
	assert.Contains(t, errStr, "transfer_workers")
	assert.Contains(t, errStr, "check_workers")
	assert.Contains(t, errStr, "conflict_strategy")
	assert.Contains(t, errStr, "log_level")
}

// --- WarnDeprecatedKeys tests ---

func TestWarnDeprecatedKeys_OldKeysPresent(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	rawMap := map[string]any{
		"parallel_downloads": 4,
		"parallel_uploads":   4,
		"parallel_checkers":  8,
	}

	WarnDeprecatedKeys(rawMap, logger)

	warnedKeys := loggedAttrValues(t, &logBuf, "key")

	assert.Contains(t, warnedKeys, "parallel_downloads")
	assert.Contains(t, warnedKeys, "parallel_uploads")
	assert.Contains(t, warnedKeys, "parallel_checkers")
	assert.Len(t, warnedKeys, 3)
}

func TestWarnDeprecatedKeys_NoOldKeys(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	rawMap := map[string]any{
		"transfer_workers": 8,
		"check_workers":    4,
	}

	WarnDeprecatedKeys(rawMap, logger)

	assert.Empty(t, strings.TrimSpace(logBuf.String()), "no warnings should be logged for new keys")
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
