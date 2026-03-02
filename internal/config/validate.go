package config

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Validation range constants.
const (
	minTransferWorkers = 4
	maxTransferWorkers = 64
	minCheckWorkers    = 1
	maxCheckWorkers    = 16
	minPercentage      = 1
	maxPercentage      = 100
	minBigDelete       = 1
	minLogRetention    = 1
	minFullscanNonZero = 2
	chunkAlignBytes    = 327680     // 320 KiB alignment for upload chunks
	minChunkBytes      = 10_485_760 // 10 MiB
	maxChunkBytes      = 62_914_560 // 60 MiB
	minPollInterval    = 5 * time.Minute
	minShutdownTimeout = 5 * time.Second
	minConnectTimeout  = 1 * time.Second
	minDataTimeout     = 5 * time.Second
	octalBase          = 8
	minOctalDigits     = 3
	maxOctalDigits     = 4
	maxOctalValue      = 0o777
	schedulePartCount  = 2
	maxScheduleHour    = 23
	maxScheduleMinute  = 59
)

// Validate checks all configuration values and returns all errors found.
// It accumulates every error rather than stopping at the first, so users
// see a complete report and can fix all issues in one pass.
func Validate(cfg *Config) error {
	var errs []error

	errs = append(errs, validateDrives(cfg)...)
	errs = append(errs, validateFilter(&cfg.FilterConfig)...)
	errs = append(errs, validateTransfers(&cfg.TransfersConfig)...)
	errs = append(errs, validateSafety(&cfg.SafetyConfig)...)
	errs = append(errs, validateSync(&cfg.SyncConfig)...)
	errs = append(errs, validateLogging(&cfg.LoggingConfig)...)
	errs = append(errs, validateNetwork(&cfg.NetworkConfig)...)

	return errors.Join(errs...)
}

// ValidateResolved checks cross-field constraints on a fully resolved drive.
// Unlike Validate(), which checks raw config file values, this runs after the
// four-layer override chain (defaults -> file -> env -> CLI) has been applied.
// It catches constraints that only make sense on the final merged result.
func ValidateResolved(rd *ResolvedDrive) error {
	var errs []error

	// SyncDir must be absolute after tilde expansion and env/CLI overrides.
	// Relative paths would resolve differently depending on cwd.
	if rd.SyncDir != "" && !filepath.IsAbs(rd.SyncDir) {
		errs = append(errs, fmt.Errorf("sync_dir: must be absolute after expansion, got %q", rd.SyncDir))
	}

	return errors.Join(errs...)
}

func validateFilter(f *FilterConfig) []error {
	var errs []error

	if f.MaxFileSize != "" && f.MaxFileSize != "0" {
		if _, err := ParseSize(f.MaxFileSize); err != nil {
			errs = append(errs, fmt.Errorf("max_file_size: %w", err))
		}
	}

	for _, p := range f.SyncPaths {
		if !strings.HasPrefix(p, "/") {
			errs = append(errs, fmt.Errorf("sync_paths: path %q must start with /", p))
		}
	}

	if f.IgnoreMarker == "" {
		errs = append(errs, errors.New("ignore_marker: must not be empty"))
	}

	return errs
}

func validateTransfers(t *TransfersConfig) []error {
	var errs []error

	if t.TransferWorkers < minTransferWorkers || t.TransferWorkers > maxTransferWorkers {
		errs = append(errs, fmt.Errorf("transfer_workers: must be between %d and %d, got %d",
			minTransferWorkers, maxTransferWorkers, t.TransferWorkers))
	}

	if t.CheckWorkers < minCheckWorkers || t.CheckWorkers > maxCheckWorkers {
		errs = append(errs, fmt.Errorf("check_workers: must be between %d and %d, got %d",
			minCheckWorkers, maxCheckWorkers, t.CheckWorkers))
	}

	errs = append(errs, validateChunkSize(t.ChunkSize)...)
	errs = append(errs, validateTransferOrder(t.TransferOrder)...)
	errs = append(errs, validateBandwidthSchedule(t.BandwidthSchedule)...)

	return errs
}

func validateChunkSize(s string) []error {
	bytes, err := ParseSize(s)
	if err != nil {
		return []error{fmt.Errorf("chunk_size: %w", err)}
	}

	if bytes < minChunkBytes || bytes > maxChunkBytes {
		return []error{fmt.Errorf("chunk_size: must be between 10MiB and 60MiB, got %s", s)}
	}

	if bytes%chunkAlignBytes != 0 {
		return []error{fmt.Errorf(
			"chunk_size: must be a multiple of 320 KiB (%d bytes), got %s (%d bytes)",
			chunkAlignBytes, s, bytes)}
	}

	return nil
}

var validTransferOrders = map[string]bool{
	"default":   true,
	"size_asc":  true,
	"size_desc": true,
	"name_asc":  true,
	"name_desc": true,
}

func validateTransferOrder(order string) []error {
	if !validTransferOrders[order] {
		return []error{fmt.Errorf(
			"transfer_order: must be one of default, size_asc, size_desc, name_asc, name_desc; got %q", order)}
	}

	return nil
}

func validateBandwidthSchedule(entries []BandwidthScheduleEntry) []error {
	var errs []error

	prevMinutes := -1

	for i := range entries {
		minutes, err := parseScheduleTime(entries[i].Time)
		if err != nil {
			errs = append(errs, fmt.Errorf("bandwidth_schedule[%d].time: %w", i, err))

			continue
		}

		if prevMinutes >= 0 && minutes <= prevMinutes {
			errs = append(errs, fmt.Errorf("bandwidth_schedule: entries must be sorted by time; %q is not after %q",
				entries[i].Time, entries[max(0, i-1)].Time))
		}

		prevMinutes = minutes
	}

	return errs
}

// parseScheduleTime parses "HH:MM" and returns total minutes since midnight.
func parseScheduleTime(s string) (int, error) {
	parts := strings.SplitN(s, ":", schedulePartCount)
	if len(parts) != schedulePartCount {
		return 0, fmt.Errorf("invalid time format %q: expected HH:MM", s)
	}

	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > maxScheduleHour {
		return 0, fmt.Errorf("invalid hour in %q: must be 00-23", s)
	}

	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > maxScheduleMinute {
		return 0, fmt.Errorf("invalid minute in %q: must be 00-59", s)
	}

	return hour*int(time.Hour/time.Minute) + minute, nil
}

func validateSafety(s *SafetyConfig) []error {
	var errs []error

	if s.BigDeleteThreshold < minBigDelete {
		errs = append(errs, fmt.Errorf("big_delete_threshold: must be >= %d, got %d",
			minBigDelete, s.BigDeleteThreshold))
	}

	if s.BigDeletePercentage < minPercentage || s.BigDeletePercentage > maxPercentage {
		errs = append(errs, fmt.Errorf("big_delete_percentage: must be between %d and %d, got %d",
			minPercentage, maxPercentage, s.BigDeletePercentage))
	}

	if s.BigDeleteMinItems < minBigDelete {
		errs = append(errs, fmt.Errorf("big_delete_min_items: must be >= %d, got %d",
			minBigDelete, s.BigDeleteMinItems))
	}

	errs = append(errs, validateSafetyRemaining(s)...)

	return errs
}

func validateSafetyRemaining(s *SafetyConfig) []error {
	var errs []error

	if s.MinFreeSpace != "" && s.MinFreeSpace != "0" {
		if _, err := ParseSize(s.MinFreeSpace); err != nil {
			errs = append(errs, fmt.Errorf("min_free_space: %w", err))
		}
	}

	errs = append(errs, validateOctalPermission("sync_dir_permissions", s.SyncDirPermissions)...)
	errs = append(errs, validateOctalPermission("sync_file_permissions", s.SyncFilePermissions)...)

	return errs
}

func validateOctalPermission(field, value string) []error {
	if value == "" {
		return []error{fmt.Errorf("%s: must not be empty", field)}
	}

	if len(value) < minOctalDigits || len(value) > maxOctalDigits {
		return []error{fmt.Errorf("%s: must be 3 or 4 octal digits, got %q", field, value)}
	}

	n, err := strconv.ParseInt(value, octalBase, 32)
	if err != nil {
		return []error{fmt.Errorf("%s: invalid octal value %q", field, value)}
	}

	if n < 0 || n > maxOctalValue {
		return []error{fmt.Errorf("%s: octal value out of range %q", field, value)}
	}

	return nil
}

func validateSync(s *SyncConfig) []error {
	var errs []error

	errs = append(errs, validateDurationMin("poll_interval", s.PollInterval, minPollInterval)...)
	errs = append(errs, validateFullscanFrequency(s.FullscanFrequency)...)
	errs = append(errs, validateConflictStrategy(s.ConflictStrategy)...)
	errs = append(errs, validateDurationNonNeg("conflict_reminder_interval", s.ConflictReminderInterval)...)
	errs = append(errs, validateDurationNonNeg("verify_interval", s.VerifyInterval)...)
	errs = append(errs, validateDurationMin("shutdown_timeout", s.ShutdownTimeout, minShutdownTimeout)...)

	return errs
}

func validateFullscanFrequency(n int) []error {
	if n != 0 && n < minFullscanNonZero {
		return []error{fmt.Errorf("fullscan_frequency: must be 0 (disabled) or >= %d, got %d",
			minFullscanNonZero, n)}
	}

	return nil
}

func validateConflictStrategy(s string) []error {
	if s != "keep_both" {
		return []error{fmt.Errorf("conflict_strategy: must be \"keep_both\", got %q", s)}
	}

	return nil
}

// validateDuration checks that a duration string is valid and meets a minimum.
// Used for per-drive poll_interval validation where the field name is contextual.
func validateDuration(field, value string, minimum time.Duration) error {
	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s: invalid duration %q: %w", field, value, err)
	}

	if d < minimum {
		return fmt.Errorf("%s: must be >= %s, got %s", field, minimum, d)
	}

	return nil
}

func validateDurationMin(field, value string, minimum time.Duration) []error {
	if err := validateDuration(field, value, minimum); err != nil {
		return []error{err}
	}

	return nil
}

func validateDurationNonNeg(field, value string) []error {
	d, err := time.ParseDuration(value)
	if err != nil {
		return []error{fmt.Errorf("%s: invalid duration %q: %w", field, value, err)}
	}

	if d < 0 {
		return []error{fmt.Errorf("%s: must be >= 0, got %s", field, d)}
	}

	return nil
}

func validateLogging(l *LoggingConfig) []error {
	var errs []error

	errs = append(errs, validateLogLevel(l.LogLevel)...)
	errs = append(errs, validateLogFormat(l.LogFormat)...)

	if l.LogRetentionDays < minLogRetention {
		errs = append(errs, fmt.Errorf("log_retention_days: must be >= %d, got %d",
			minLogRetention, l.LogRetentionDays))
	}

	return errs
}

var validLogLevels = map[string]bool{
	"debug": true,
	"info":  true,
	"warn":  true,
	"error": true,
}

func validateLogLevel(level string) []error {
	if !validLogLevels[level] {
		return []error{fmt.Errorf("log_level: must be one of debug, info, warn, error; got %q", level)}
	}

	return nil
}

var validLogFormats = map[string]bool{
	"auto": true,
	"text": true,
	"json": true,
}

func validateLogFormat(format string) []error {
	if !validLogFormats[format] {
		return []error{fmt.Errorf("log_format: must be one of auto, text, json; got %q", format)}
	}

	return nil
}

func validateNetwork(n *NetworkConfig) []error {
	var errs []error

	errs = append(errs, validateDurationMin("connect_timeout", n.ConnectTimeout, minConnectTimeout)...)
	errs = append(errs, validateDurationMin("data_timeout", n.DataTimeout, minDataTimeout)...)

	return errs
}

// deprecatedTransferKeys maps old config key names to their replacements.
var deprecatedTransferKeys = map[string]string{
	"parallel_downloads": "transfer_workers",
	"parallel_uploads":   "transfer_workers",
	"parallel_checkers":  "check_workers",
}

// WarnDeprecatedKeys checks raw TOML metadata for deprecated config keys and
// logs a warning for each one found. The deprecated keys still parse without
// error (they're in knownGlobalKeys) but their values are silently ignored.
func WarnDeprecatedKeys(md map[string]any, logger *slog.Logger) {
	for oldKey, newKey := range deprecatedTransferKeys {
		if _, ok := md[oldKey]; ok {
			logger.Warn("deprecated config key (value ignored)",
				slog.String("key", oldKey),
				slog.String("replacement", newKey),
			)
		}
	}
}

// WarnUnimplemented logs a warning for each config field that is set to a
// non-default value but is not yet implemented. This prevents users from
// thinking their settings take effect when they silently don't (B-141).
func WarnUnimplemented(rd *ResolvedDrive, logger *slog.Logger) {
	warn := func(field string) {
		logger.Warn("config field not yet implemented; value will be ignored",
			slog.String("field", field))
	}

	if len(rd.SyncPaths) > 0 {
		warn("sync_paths")
	}

	if len(rd.SkipFiles) > 0 {
		warn("skip_files")
	}

	if len(rd.SkipDirs) > 0 {
		warn("skip_dirs")
	}

	if rd.MaxFileSize != "0" && rd.MaxFileSize != defaultMaxFileSize {
		warn("max_file_size")
	}

	if rd.BandwidthLimit != "0" && rd.BandwidthLimit != defaultBandwidthLimit {
		warn("bandwidth_limit")
	}

	if len(rd.BandwidthSchedule) > 0 {
		warn("bandwidth_schedule")
	}

	if rd.Websocket {
		warn("websocket")
	}

	if rd.UserAgent != "" {
		warn("user_agent")
	}
}
