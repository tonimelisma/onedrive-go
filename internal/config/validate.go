package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Validation range constants.
const (
	minTransferWorkers    = 4
	maxTransferWorkers    = 64
	minCheckWorkers       = 1
	maxCheckWorkers       = 16
	minBigDelete          = 1
	minLogRetention       = 1
	minPollInterval       = 30 * time.Second
	minShutdownTimeout    = 5 * time.Second
	minSafetyScanInterval = 10 * time.Second
	octalBase             = 8
	minOctalDigits        = 3
	maxOctalDigits        = 4
	maxOctalValue         = 0o777
)

func validateFilter(f *FilterConfig) []error {
	var errs []error

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

	return errs
}

func validateSafety(s *SafetyConfig) []error {
	var errs []error

	if s.BigDeleteThreshold < minBigDelete {
		errs = append(errs, fmt.Errorf("big_delete_threshold: must be >= %d, got %d",
			minBigDelete, s.BigDeleteThreshold))
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
	errs = append(errs, validateConflictStrategy(s.ConflictStrategy)...)
	errs = append(errs, validateDurationMin("shutdown_timeout", s.ShutdownTimeout, minShutdownTimeout)...)
	errs = append(errs, validateDurationMin("safety_scan_interval", s.SafetyScanInterval, minSafetyScanInterval)...)

	return errs
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

const (
	logLevelDebug = "debug"
	logLevelInfo  = "info"
	logLevelWarn  = "warn"
	logLevelError = "error"
	logFormatAuto = "auto"
	logFormatText = "text"
	logFormatJSON = "json"
)

func validateLogLevel(level string) []error {
	if !isValidLogLevel(level) {
		return []error{fmt.Errorf("log_level: must be one of debug, info, warn, error; got %q", level)}
	}

	return nil
}

func validateLogFormat(format string) []error {
	if !isValidLogFormat(format) {
		return []error{fmt.Errorf("log_format: must be one of auto, text, json; got %q", format)}
	}

	return nil
}

// WarnDeprecatedKeys checks raw TOML metadata for deprecated config keys and
// logs a warning for each one found. The deprecated keys still parse without
// error (they're in knownGlobalKeys) but their values are silently ignored.
func WarnDeprecatedKeys(md map[string]any, logger *slog.Logger) {
	for oldKey, newKey := range deprecatedTransferKeys() {
		if _, ok := md[oldKey]; ok {
			logger.Warn("deprecated config key (value ignored)",
				slog.String("key", oldKey),
				slog.String("replacement", newKey),
			)
		}
	}
}

func isValidLogLevel(level string) bool {
	switch level {
	case logLevelDebug, logLevelInfo, logLevelWarn, logLevelError:
		return true
	default:
		return false
	}
}

func isValidLogFormat(format string) bool {
	switch format {
	case logFormatAuto, logFormatText, logFormatJSON:
		return true
	default:
		return false
	}
}

func deprecatedTransferKeys() map[string]string {
	return map[string]string{
		"parallel_downloads": "transfer_workers",
		"parallel_uploads":   "transfer_workers",
		"parallel_checkers":  "check_workers",
	}
}
