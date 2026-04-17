package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/BurntSushi/toml"
)

// ConfigWarning represents a non-fatal issue found during lenient config
// loading. Used by informational commands (drive list, status, shared) that
// want to show what they can even when the config has errors.
type ConfigWarning struct {
	Message string // human-readable description of the issue
}

// LogWarnings logs each warning at Warn level. Used by informational commands
// (drive list, status, shared) to surface config issues without failing.
func LogWarnings(warnings []ConfigWarning, logger *slog.Logger) {
	for _, w := range warnings {
		logger.Warn("config issue", "message", w.Message)
	}
}

// Load reads and parses a TOML config file using a two-pass decode, validates
// it, and returns the resulting Config. Unknown keys are treated as fatal
// errors with "did you mean?" suggestions.
func Load(path string, logger *slog.Logger) (*Config, error) {
	return loadWithIO(path, logger, defaultConfigIO())
}

func loadWithIO(path string, logger *slog.Logger, io configIO) (*Config, error) {
	return newConfigLoader(io).load(path, logger)
}

// LoadOrDefault reads a TOML config file if it exists, otherwise returns
// a Config populated with all default values. Used by EnsureDriveInConfig
// (config creation path) and callers that want smart error messages when
// config is missing rather than raw file-not-found errors.
func LoadOrDefault(path string, logger *slog.Logger) (*Config, error) {
	return loadOrDefaultWithIO(path, logger, defaultConfigIO())
}

func loadOrDefaultWithIO(path string, logger *slog.Logger, io configIO) (*Config, error) {
	return newConfigLoader(io).loadOrDefault(path, logger)
}

// LoadLenient reads and parses a TOML config file, collecting unknown keys and
// validation errors as warnings instead of fatal errors. TOML syntax errors and
// file read errors remain fatal. Used by informational commands (drive list,
// status, shared) that need to show what they can even when config has errors.
func LoadLenient(path string, logger *slog.Logger) (*Config, []ConfigWarning, error) {
	return loadLenientWithIO(path, logger, defaultConfigIO())
}

func loadLenientWithIO(path string, logger *slog.Logger, io configIO) (*Config, []ConfigWarning, error) {
	return newConfigLoader(io).loadLenient(path, logger)
}

// LoadOrDefaultLenient reads a TOML config file leniently if it exists,
// otherwise returns default Config with no warnings.
func LoadOrDefaultLenient(path string, logger *slog.Logger) (*Config, []ConfigWarning, error) {
	return loadOrDefaultLenientWithIO(path, logger, defaultConfigIO())
}

func loadOrDefaultLenientWithIO(path string, logger *slog.Logger, io configIO) (*Config, []ConfigWarning, error) {
	return newConfigLoader(io).loadOrDefaultLenient(path, logger)
}

// configLoader owns config-file entrypoints so strict and lenient loading share
// one decode pipeline instead of open-coding the same responsibilities in every
// exported function.
type configLoader struct {
	io      configIO
	decoder driveSectionDecoder
}

func newConfigLoader(io configIO) configLoader {
	return configLoader{
		io:      io,
		decoder: newDriveSectionDecoder(),
	}
}

func (l configLoader) load(path string, logger *slog.Logger) (*Config, error) {
	logger.Debug("loading config file", "path", path)

	data, err := l.io.readManagedFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}

	cfg, md, err := l.decodeBaseConfig(path, data)
	if err != nil {
		return nil, err
	}

	if err := l.decoder.decodeStrict(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}

	if err := checkUnknownKeys(&md); err != nil {
		return nil, err
	}

	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	logger.Debug("config file parsed successfully",
		"path", path,
		"drive_count", len(cfg.Drives),
	)

	return cfg, nil
}

func (l configLoader) loadOrDefault(path string, logger *slog.Logger) (*Config, error) {
	if _, err := l.io.statManagedPath(path); errors.Is(err, os.ErrNotExist) {
		logger.Debug("config file not found, using defaults", "path", path)

		return DefaultConfig(), nil
	} else if err != nil {
		return nil, fmt.Errorf("stating config file %s: %w", path, err)
	}

	return l.load(path, logger)
}

func (l configLoader) loadLenient(path string, logger *slog.Logger) (*Config, []ConfigWarning, error) {
	logger.Debug("loading config file (lenient)", "path", path)

	data, err := l.io.readManagedFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading config file %s: %w", path, err)
	}

	cfg, md, err := l.decodeBaseConfig(path, data)
	if err != nil {
		return nil, nil, err
	}

	warnings := l.decoder.decodeLenient(data, cfg)
	warnings = appendWarnings(warnings, collectUnknownGlobalKeyErrors(&md))

	warnings = appendWarnings(warnings, collectValidationErrors(cfg))

	logger.Debug("config file parsed (lenient)",
		"path", path,
		"drive_count", len(cfg.Drives),
		"warning_count", len(warnings),
	)

	return cfg, warnings, nil
}

func (l configLoader) loadOrDefaultLenient(path string, logger *slog.Logger) (*Config, []ConfigWarning, error) {
	if _, err := l.io.statManagedPath(path); errors.Is(err, os.ErrNotExist) {
		logger.Debug("config file not found, using defaults", "path", path)

		return DefaultConfig(), nil, nil
	} else if err != nil {
		return nil, nil, fmt.Errorf("stating config file %s: %w", path, err)
	}

	return l.loadLenient(path, logger)
}

func (l configLoader) decodeBaseConfig(path string, data []byte) (*Config, toml.MetaData, error) {
	cfg := DefaultConfig()

	md, err := toml.Decode(string(data), cfg)
	if err != nil {
		return nil, toml.MetaData{}, fmt.Errorf("parsing config file %s: %w", path, err)
	}

	return cfg, md, nil
}

func appendWarnings(warnings []ConfigWarning, errs []error) []ConfigWarning {
	for _, err := range errs {
		warnings = append(warnings, ConfigWarning{
			Message: err.Error(),
		})
	}

	return warnings
}
