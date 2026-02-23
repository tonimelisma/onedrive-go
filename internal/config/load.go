package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Load reads and parses a TOML config file using a two-pass decode, validates
// it, and returns the resulting Config. Pass 1 decodes flat global settings
// into embedded structs. Pass 2 extracts drive sections (keys containing ":").
// Unknown keys are treated as fatal errors with "did you mean?" suggestions.
func Load(path string, logger *slog.Logger) (*Config, error) {
	logger.Debug("loading config file", "path", path)

	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}

	// Pass 1: decode flat global settings into embedded structs.
	md, err := toml.Decode(string(data), cfg)
	if err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}

	// Pass 2: extract drive sections (keys containing ":") from raw map.
	if err := decodeDriveSections(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}

	// Check for unknown global keys (drive sections are validated separately).
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

// decodeDriveSections performs the second TOML decode pass to extract drive
// sections. Drive sections have canonical IDs containing ":" as their key.
func decodeDriveSections(data []byte, cfg *Config) error {
	var rawMap map[string]any
	if _, err := toml.Decode(string(data), &rawMap); err != nil {
		return fmt.Errorf("drive sections: %w", err)
	}

	for key, val := range rawMap {
		if !strings.Contains(key, ":") {
			continue // not a drive section
		}

		// Validate canonical ID at parse time (fail fast).
		cid, cidErr := driveid.NewCanonicalID(key)
		if cidErr != nil {
			return fmt.Errorf("drive section [%q]: invalid canonical ID: %w", key, cidErr)
		}

		driveMap, ok := val.(map[string]any)
		if !ok {
			return fmt.Errorf("drive section [%q] must be a table", key)
		}

		if err := checkDriveUnknownKeys(driveMap, key); err != nil {
			return err
		}

		var drive Drive
		if err := mapToDrive(driveMap, &drive); err != nil {
			return fmt.Errorf("drive section [%q]: %w", key, err)
		}

		cfg.Drives[cid] = drive
	}

	return nil
}

// mapToDrive converts a raw map to a Drive struct by re-encoding as TOML
// and decoding into the typed struct. This reuses the TOML library's type
// coercion rather than hand-writing map extraction for each field.
func mapToDrive(m map[string]any, d *Drive) error {
	var buf bytes.Buffer

	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(m); err != nil {
		return fmt.Errorf("encoding drive data: %w", err)
	}

	if _, err := toml.Decode(buf.String(), d); err != nil {
		return fmt.Errorf("decoding drive data: %w", err)
	}

	return nil
}

// LoadOrDefault reads a TOML config file if it exists, otherwise returns
// a Config populated with all default values. This supports the zero-config
// first-run experience: users can start without creating a config file.
func LoadOrDefault(path string, logger *slog.Logger) (*Config, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		logger.Debug("config file not found, using defaults", "path", path)

		return DefaultConfig(), nil
	}

	return Load(path, logger)
}

// ResolveDrive loads configuration and applies the four-layer override chain:
// defaults -> config file -> environment variables -> CLI flags.
// It returns a fully resolved and validated drive configuration ready for use.
func ResolveDrive(env EnvOverrides, cli CLIOverrides, logger *slog.Logger) (*ResolvedDrive, error) {
	// Step 1: resolve config path (CLI > env > default).
	cfgPath := resolveConfigPath(env, cli, logger)

	// Step 2: load config file (returns defaults if no file exists).
	cfg, err := LoadOrDefault(cfgPath, logger)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	// Step 3: determine drive selector (CLI > env).
	selector := env.Drive
	if cli.Drive != "" {
		selector = cli.Drive
	}

	logger.Debug("drive selector resolved",
		"selector", selector,
		"source_env", env.Drive,
		"source_cli", cli.Drive,
	)

	// Step 4: match drive.
	canonicalID, drive, err := matchDrive(cfg, selector, logger)
	if err != nil {
		return nil, err
	}

	// Step 5: build resolved drive (global + per-drive overrides).
	resolved := buildResolvedDrive(cfg, canonicalID, &drive, logger)

	// Step 6: apply CLI overrides.
	if cli.DryRun != nil {
		resolved.DryRun = *cli.DryRun
		logger.Debug("CLI override applied", "dry_run", resolved.DryRun)
	}

	// Step 7: validate the final resolved drive.
	if err := ValidateResolved(resolved); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	return resolved, nil
}

// resolveConfigPath determines the config file path from CLI flags,
// environment variables, or the platform default.
func resolveConfigPath(env EnvOverrides, cli CLIOverrides, logger *slog.Logger) string {
	cfgPath := DefaultConfigPath()
	source := "default"

	if env.ConfigPath != "" {
		cfgPath = env.ConfigPath
		source = "env"
	}

	if cli.ConfigPath != "" {
		cfgPath = cli.ConfigPath
		source = "cli"
	}

	logger.Debug("config path resolved", "path", cfgPath, "source", source)

	return cfgPath
}
