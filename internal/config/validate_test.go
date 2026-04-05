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

func TestValidate_ChunkSize_TooSmall(t *testing.T) {
	cfg := validConfig()
	cfg.ChunkSize = "1MB"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chunk_size")
}

func TestValidate_ChunkSize_TooLarge(t *testing.T) {
	cfg := validConfig()
	cfg.ChunkSize = "100MB"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chunk_size")
}

// Validates: R-4.8.2
func TestValidate_ChunkSize_NotAligned(t *testing.T) {
	cfg := validConfig()
	// 11 MiB = 11,534,336 bytes. 11,534,336 / 327,680 = 35.2 — not aligned.
	cfg.ChunkSize = "11MiB"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple of 320 KiB")
}

func TestValidate_ChunkSize_Valid(t *testing.T) {
	// Valid chunk sizes must be multiples of 320 KiB and between 10-60 MiB.
	for _, size := range []string{"10MiB", "20MiB", "40MiB", "60MiB"} {
		cfg := validConfig()
		cfg.ChunkSize = size
		err := Validate(cfg)
		assert.NoError(t, err, "expected %s to be valid", size)
	}
}

func TestValidate_TransferOrder_Invalid(t *testing.T) {
	cfg := validConfig()
	cfg.TransferOrder = "random"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transfer_order")
}

func TestValidate_TransferOrder_AllValid(t *testing.T) {
	for _, order := range []string{"default", "size_asc", "size_desc", "name_asc", "name_desc"} {
		cfg := validConfig()
		cfg.TransferOrder = order
		err := Validate(cfg)
		assert.NoError(t, err, "expected %s to be valid", order)
	}
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

func TestValidate_ConnectTimeout_TooShort(t *testing.T) {
	cfg := validConfig()
	cfg.ConnectTimeout = "500ms"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connect_timeout")
}

func TestValidate_DataTimeout_TooShort(t *testing.T) {
	cfg := validConfig()
	cfg.DataTimeout = "2s"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "data_timeout")
}

func TestValidate_ConflictStrategy_Invalid(t *testing.T) {
	cfg := validConfig()
	cfg.ConflictStrategy = "keep_remote"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflict_strategy")
}

func TestValidate_FullscanFrequency_InvalidNonZero(t *testing.T) {
	cfg := validConfig()
	cfg.FullscanFrequency = 1
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fullscan_frequency")
}

func TestValidate_FullscanFrequency_Zero(t *testing.T) {
	cfg := validConfig()
	cfg.FullscanFrequency = 0
	err := Validate(cfg)
	assert.NoError(t, err)
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

func TestValidate_BandwidthSchedule_InvalidTime(t *testing.T) {
	cfg := validConfig()
	cfg.BandwidthSchedule = []BandwidthScheduleEntry{
		{Time: "25:00", Limit: "5MB/s"},
	}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bandwidth_schedule")
}

func TestValidate_BandwidthSchedule_NotSorted(t *testing.T) {
	cfg := validConfig()
	cfg.BandwidthSchedule = []BandwidthScheduleEntry{
		{Time: "18:00", Limit: "50MB/s"},
		{Time: "08:00", Limit: "5MB/s"},
	}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sorted")
}

func TestValidate_BandwidthSchedule_Valid(t *testing.T) {
	cfg := validConfig()
	cfg.BandwidthSchedule = []BandwidthScheduleEntry{
		{Time: "08:00", Limit: "5MB/s"},
		{Time: "18:00", Limit: "50MB/s"},
		{Time: "23:00", Limit: "0"},
	}
	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidate_VerifyInterval_Valid(t *testing.T) {
	cfg := validConfig()
	cfg.VerifyInterval = "168h"
	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidate_ConflictReminderInterval_Zero(t *testing.T) {
	cfg := validConfig()
	cfg.ConflictReminderInterval = "0s"
	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestParseScheduleTime_Valid(t *testing.T) {
	minutes, err := parseScheduleTime("08:30")
	require.NoError(t, err)
	assert.Equal(t, 8*60+30, minutes)

	minutes, err = parseScheduleTime("23:59")
	require.NoError(t, err)
	assert.Equal(t, 23*60+59, minutes)

	minutes, err = parseScheduleTime("00:00")
	require.NoError(t, err)
	assert.Equal(t, 0, minutes)
}

func TestParseScheduleTime_Invalid(t *testing.T) {
	for _, input := range []string{"25:00", "08:60", "abc", "8:30:00", ""} {
		t.Run(input, func(t *testing.T) {
			_, err := parseScheduleTime(input)
			assert.Error(t, err)
		})
	}
}

func TestValidate_BandwidthSchedule_BadTimeFormat(t *testing.T) {
	cfg := validConfig()
	cfg.BandwidthSchedule = []BandwidthScheduleEntry{
		{Time: "noon", Limit: "5MB/s"},
	}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "time")
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

// ---------------------------------------------------------------------------
// WarnUnimplemented tests (B-141)
// ---------------------------------------------------------------------------

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

func TestWarnUnimplemented_Defaults_NoWarnings(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	cfg := DefaultConfig()
	rd := &ResolvedDrive{
		FilterConfig:    cfg.FilterConfig,
		TransfersConfig: cfg.TransfersConfig,
		SyncConfig:      cfg.SyncConfig,
		NetworkConfig:   cfg.NetworkConfig,
	}

	WarnUnimplemented(rd, logger)

	assert.Empty(t, loggedAttrValues(t, &logBuf, "field"), "default config should not produce warnings")
}

func TestWarnUnimplemented_NonDefaults_WarnsAll(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	cfg := DefaultConfig()
	rd := &ResolvedDrive{
		FilterConfig:    cfg.FilterConfig,
		TransfersConfig: cfg.TransfersConfig,
		SyncConfig:      cfg.SyncConfig,
		NetworkConfig:   cfg.NetworkConfig,
	}

	// Set non-default values for all unimplemented fields.
	rd.SyncPaths = []string{"/docs"}
	rd.SkipFiles = []string{"*.tmp"}
	rd.SkipDirs = []string{".git"}
	rd.BandwidthLimit = "10MB"
	rd.BandwidthSchedule = []BandwidthScheduleEntry{{Time: "08:00", Limit: "5MB"}}
	rd.Websocket = true
	rd.UserAgent = "custom"

	WarnUnimplemented(rd, logger)

	warned := loggedAttrValues(t, &logBuf, "field")

	expected := []string{
		"sync_paths",
		"bandwidth_limit", "bandwidth_schedule", "user_agent",
	}
	for _, f := range expected {
		assert.Contains(t, warned, f, "expected warning for %q", f)
	}

	assert.NotContains(t, warned, "skip_files", "implemented file filters should not warn")
	assert.NotContains(t, warned, "skip_dirs", "implemented directory filters should not warn")
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
