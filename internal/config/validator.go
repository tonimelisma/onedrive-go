package config

import "errors"

// collectValidationErrors accumulates all config validation errors into a
// slice. The strict path (Validate) and the lenient path
// (LoadOrDefaultLenient, which converts these to warnings) both call it, so
// they stay on one rule set.
func collectValidationErrors(cfg *Config) []error {
	var errs []error

	errs = append(errs, validateDrives(cfg)...)
	errs = append(errs, validateTransfers(&cfg.TransfersConfig)...)
	errs = append(errs, validateSafety(&cfg.SafetyConfig)...)
	errs = append(errs, validateSync(&cfg.SyncConfig)...)
	errs = append(errs, validateLogging(&cfg.LoggingConfig)...)

	return errs
}

// Validate checks all configuration values and returns all errors found.
// It accumulates every error rather than stopping at the first, so users
// see a complete report and can fix all issues in one pass.
func Validate(cfg *Config) error {
	return markInvalidConfig(errors.Join(collectValidationErrors(cfg)...))
}
