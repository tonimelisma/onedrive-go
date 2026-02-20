package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// Load reads and parses a TOML config file using a two-pass decode, validates
// it, and returns the resulting Config. Pass 1 decodes flat global settings
// into embedded structs. Pass 2 extracts drive sections (keys containing ":").
// Unknown keys are treated as fatal errors with "did you mean?" suggestions.
func Load(path string) (*Config, error) {
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

		cfg.Drives[key] = drive
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
func LoadOrDefault(path string) (*Config, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return DefaultConfig(), nil
	}

	return Load(path)
}

// ResolveDrive loads configuration and applies the four-layer override chain:
// defaults -> config file -> environment variables -> CLI flags.
// It returns a fully resolved and validated drive configuration ready for use.
func ResolveDrive(env EnvOverrides, cli CLIOverrides) (*ResolvedDrive, error) {
	// Step 1: resolve config path (CLI > env > default).
	cfgPath := resolveConfigPath(env, cli)

	// Step 2: load config file (returns defaults if no file exists).
	cfg, err := LoadOrDefault(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	// Step 3: determine drive selector (CLI > env).
	selector := env.Drive
	if cli.Drive != "" {
		selector = cli.Drive
	}

	// Step 4: match drive.
	canonicalID, drive, err := matchDrive(cfg, selector)
	if err != nil {
		return nil, err
	}

	// Step 5: build resolved drive (global + per-drive overrides).
	resolved := buildResolvedDrive(cfg, canonicalID, &drive)

	// Step 6: apply CLI overrides.
	if cli.DryRun != nil {
		resolved.DryRun = *cli.DryRun
	}

	// Step 7: validate the final resolved drive.
	if err := ValidateResolved(resolved); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	return resolved, nil
}

// resolveConfigPath determines the config file path from CLI flags,
// environment variables, or the platform default.
func resolveConfigPath(env EnvOverrides, cli CLIOverrides) string {
	cfgPath := DefaultConfigPath()

	if env.ConfigPath != "" {
		cfgPath = env.ConfigPath
	}

	if cli.ConfigPath != "" {
		cfgPath = cli.ConfigPath
	}

	return cfgPath
}
