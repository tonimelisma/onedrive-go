package config

import (
	"fmt"
	"time"
)

// Validation range constants.
const (
	minTransferWorkers = 4
	maxTransferWorkers = 64
	minCheckWorkers    = 1
	maxCheckWorkers    = 16
	minLogRetention    = 1
	minPollInterval    = 30 * time.Second
)

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
	return validateSafetyRemaining(s)
}

func validateSafetyRemaining(s *SafetyConfig) []error {
	var errs []error

	if s.MinFreeSpace != "" && s.MinFreeSpace != "0" {
		if _, err := ParseSize(s.MinFreeSpace); err != nil {
			errs = append(errs, fmt.Errorf("min_free_space: %w", err))
		}
	}

	return errs
}

func validateSync(s *SyncConfig) []error {
	var errs []error

	errs = append(errs, validateDurationMin("poll_interval", s.PollInterval, minPollInterval)...)

	return errs
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
