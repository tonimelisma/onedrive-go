package config

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestValidate_ParallelWorkers_BelowMin(t *testing.T) {
	cfg := validConfig()
	cfg.ParallelDownloads = 0
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parallel_downloads")
}

func TestValidate_ParallelWorkers_AboveMax(t *testing.T) {
	cfg := validConfig()
	cfg.ParallelUploads = 17
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parallel_uploads")
}

func TestValidate_ParallelCheckers_BelowMin(t *testing.T) {
	cfg := validConfig()
	cfg.ParallelCheckers = 0
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parallel_checkers")
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

func TestValidate_BigDeletePercentage_OutOfRange(t *testing.T) {
	cfg := validConfig()
	cfg.BigDeletePercentage = 0
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "big_delete_percentage")

	cfg.BigDeletePercentage = 101
	err = Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "big_delete_percentage")
}

func TestValidate_BigDeleteThreshold_BelowMin(t *testing.T) {
	cfg := validConfig()
	cfg.BigDeleteThreshold = 0
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "big_delete_threshold")
}

func TestValidate_BigDeleteMinItems_BelowMin(t *testing.T) {
	cfg := validConfig()
	cfg.BigDeleteMinItems = 0
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "big_delete_min_items")
}

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
	cfg.PollInterval = "1m"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "poll_interval")
}

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

func TestValidate_MaxFileSize_Invalid(t *testing.T) {
	cfg := validConfig()
	cfg.MaxFileSize = invalidSizeStr
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_file_size")
}

func TestValidate_MinFreeSpace_Invalid(t *testing.T) {
	cfg := validConfig()
	cfg.MinFreeSpace = invalidSizeStr
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "min_free_space")
}

func TestValidate_MultipleErrors(t *testing.T) {
	cfg := validConfig()
	cfg.ParallelDownloads = 0
	cfg.ParallelUploads = 0
	cfg.ConflictStrategy = invalidEnumStr
	cfg.LogLevel = invalidEnumStr

	err := Validate(cfg)
	require.Error(t, err)

	errStr := err.Error()
	assert.Contains(t, errStr, "parallel_downloads")
	assert.Contains(t, errStr, "parallel_uploads")
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
	assert.True(t, strings.Contains(err.Error(), "time"))
}

// --- ValidateResolved tests ---

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

// ---------------------------------------------------------------------------
// WarnUnimplemented tests (B-141)
// ---------------------------------------------------------------------------

// testLogHandler captures slog records for assertion.
type testLogHandler struct {
	records []slog.Record
}

func (h *testLogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *testLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}

func (h *testLogHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *testLogHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *testLogHandler) warnedFields() []string {
	var fields []string

	for _, r := range h.records {
		if r.Level == slog.LevelWarn {
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == "field" {
					fields = append(fields, a.Value.String())
				}
				return true
			})
		}
	}

	return fields
}

func TestWarnUnimplemented_Defaults_NoWarnings(t *testing.T) {
	t.Parallel()

	h := &testLogHandler{}
	logger := slog.New(h)

	cfg := DefaultConfig()
	rd := &ResolvedDrive{
		FilterConfig:    cfg.FilterConfig,
		TransfersConfig: cfg.TransfersConfig,
		SyncConfig:      cfg.SyncConfig,
		NetworkConfig:   cfg.NetworkConfig,
	}

	WarnUnimplemented(rd, logger)

	assert.Empty(t, h.warnedFields(), "default config should not produce warnings")
}

func TestWarnUnimplemented_NonDefaults_WarnsAll(t *testing.T) {
	t.Parallel()

	h := &testLogHandler{}
	logger := slog.New(h)

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
	rd.MaxFileSize = "1GB"
	rd.BandwidthLimit = "10MB"
	rd.BandwidthSchedule = []BandwidthScheduleEntry{{Time: "08:00", Limit: "5MB"}}
	rd.Websocket = true
	rd.UserAgent = "custom"

	WarnUnimplemented(rd, logger)

	warned := h.warnedFields()

	expected := []string{
		"sync_paths", "skip_files", "skip_dirs", "max_file_size",
		"bandwidth_limit", "bandwidth_schedule", "websocket", "user_agent",
	}
	for _, f := range expected {
		assert.Contains(t, warned, f, "expected warning for %q", f)
	}
}
