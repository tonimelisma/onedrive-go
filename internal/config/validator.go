package config

import "errors"

// collectValidationErrors accumulates all config validation errors into a
// slice. Used by both the strict path (Validate) and the lenient path
// (LoadLenient, which converts these to warnings).
func collectValidationErrors(cfg *Config) []error {
	return newConfigValidator().collect(cfg)
}

// Validate checks all configuration values and returns all errors found.
// It accumulates every error rather than stopping at the first, so users
// see a complete report and can fix all issues in one pass.
func Validate(cfg *Config) error {
	return newConfigValidator().validate(cfg)
}

// configValidator centralizes whole-config validation so strict loading,
// lenient loading, and direct validation calls stay on the same rule set.
type configValidator struct{}

func newConfigValidator() configValidator {
	return configValidator{}
}

func (configValidator) validate(cfg *Config) error {
	return errors.Join(collectValidationErrors(cfg)...)
}

func (configValidator) collect(cfg *Config) []error {
	var errs []error

	errs = append(errs, validateDrives(cfg)...)
	errs = append(errs, validateFilter(&cfg.FilterConfig)...)
	errs = append(errs, validateTransfers(&cfg.TransfersConfig)...)
	errs = append(errs, validateSafety(&cfg.SafetyConfig)...)
	errs = append(errs, validateSync(&cfg.SyncConfig)...)
	errs = append(errs, validateLogging(&cfg.LoggingConfig)...)

	return errs
}
