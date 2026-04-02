package config

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ConfigWarning represents a non-fatal issue found during lenient config
// loading. Used by informational commands (drive list, status, whoami) that
// want to show what they can even when the config has errors.
type ConfigWarning struct {
	Message string // human-readable description of the issue
}

// LogWarnings logs each warning at Warn level. Used by informational commands
// (drive list, status, whoami) to surface config issues without failing.
func LogWarnings(warnings []ConfigWarning, logger *slog.Logger) {
	for _, w := range warnings {
		logger.Warn("config issue", "message", w.Message)
	}
}

// Load reads and parses a TOML config file using a two-pass decode, validates
// it, and returns the resulting Config. Pass 1 decodes flat global settings
// into embedded structs. Pass 2 extracts drive sections (keys containing ":").
// Unknown keys are treated as fatal errors with "did you mean?" suggestions.
func Load(path string, logger *slog.Logger) (*Config, error) {
	return loadWithIO(path, logger, defaultConfigIO())
}

func loadWithIO(path string, logger *slog.Logger, io configIO) (*Config, error) {
	logger.Debug("loading config file", "path", path)

	cfg := DefaultConfig()

	data, err := io.readManagedFile(path)
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

	// Warn about deprecated keys (parallel_downloads/uploads/checkers).
	var rawMap map[string]any
	if _, decodeErr := toml.Decode(string(data), &rawMap); decodeErr == nil {
		WarnDeprecatedKeys(rawMap, logger)
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

// decodeMode controls whether drive section parsing is strict (fail on first
// error) or lenient (collect errors as warnings, skip bad drives).
type decodeMode int

const (
	decodeModeStrict decodeMode = iota
	decodeModeLenient
)

// decodeDriveSectionsInternal is the shared implementation for both strict and
// lenient drive section parsing. Drive sections have canonical IDs containing
// ":" as their key. In strict mode, the first error is returned immediately.
// In lenient mode, errors are collected as warnings and bad drives are skipped.
func decodeDriveSectionsInternal(data []byte, cfg *Config, mode decodeMode) ([]ConfigWarning, error) {
	var rawMap map[string]any
	if _, err := toml.Decode(string(data), &rawMap); err != nil {
		if mode == decodeModeStrict {
			return nil, fmt.Errorf("drive sections: %w", err)
		}
		// Lenient: TOML re-decode failed — but Load already succeeded on the
		// first decode, so this shouldn't happen. Return empty warnings.
		return nil, nil
	}

	var warnings []ConfigWarning

	for key, val := range rawMap {
		if !strings.Contains(key, ":") {
			continue // not a drive section
		}

		// Validate canonical ID at parse time.
		cid, cidErr := driveid.NewCanonicalID(key)
		if cidErr != nil {
			if mode == decodeModeStrict {
				return nil, fmt.Errorf("drive section [%q]: invalid canonical ID: %w", key, cidErr)
			}

			warnings = append(warnings, ConfigWarning{
				Message: fmt.Sprintf("drive section [%q]: invalid canonical ID: %s", key, cidErr),
			})

			continue
		}

		driveMap, ok := val.(map[string]any)
		if !ok {
			if mode == decodeModeStrict {
				return nil, fmt.Errorf("drive section [%q] must be a table", key)
			}

			warnings = append(warnings, ConfigWarning{
				Message: fmt.Sprintf("drive section [%q] must be a table", key),
			})

			continue
		}

		// Check for unknown drive keys.
		unknownKeyErrs := collectDriveUnknownKeyErrors(driveMap, key)
		if mode == decodeModeStrict {
			if err := errors.Join(unknownKeyErrs...); err != nil {
				return nil, err
			}
		} else {
			for _, e := range unknownKeyErrs {
				warnings = append(warnings, ConfigWarning{
					Message: e.Error(),
				})
			}
		}

		var drive Drive
		if err := mapToDrive(driveMap, &drive); err != nil {
			if mode == decodeModeStrict {
				return nil, fmt.Errorf("drive section [%q]: %w", key, err)
			}

			warnings = append(warnings, ConfigWarning{
				Message: fmt.Sprintf("drive section [%q]: %s", key, err),
			})

			continue
		}

		cfg.Drives[cid] = drive
	}

	return warnings, nil
}

// decodeDriveSections performs the strict second TOML decode pass to extract
// drive sections. Returns an error on the first invalid section.
func decodeDriveSections(data []byte, cfg *Config) error {
	_, err := decodeDriveSectionsInternal(data, cfg, decodeModeStrict)
	return err
}

// decodeDriveSectionsLenient is the lenient variant that collects errors as
// warnings instead of failing. Drives with structural issues are skipped.
// In lenient mode, decodeDriveSectionsInternal never returns an error — it
// collects all issues as warnings instead.
func decodeDriveSectionsLenient(data []byte, cfg *Config) []ConfigWarning {
	warnings, err := decodeDriveSectionsInternal(data, cfg, decodeModeLenient)
	if err != nil {
		warnings = append(warnings, ConfigWarning{
			Message: fmt.Sprintf("decoding drive sections: %v", err),
		})
	}

	return warnings
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
// a Config populated with all default values. Used by EnsureDriveInConfig
// (config creation path) and callers that want smart error messages when
// config is missing rather than raw file-not-found errors.
func LoadOrDefault(path string, logger *slog.Logger) (*Config, error) {
	return loadOrDefaultWithIO(path, logger, defaultConfigIO())
}

func loadOrDefaultWithIO(path string, logger *slog.Logger, io configIO) (*Config, error) {
	if _, err := io.statManagedPath(path); errors.Is(err, os.ErrNotExist) {
		logger.Debug("config file not found, using defaults", "path", path)

		return DefaultConfig(), nil
	} else if err != nil {
		return nil, fmt.Errorf("stating config file %s: %w", path, err)
	}

	return loadWithIO(path, logger, io)
}

// LoadLenient reads and parses a TOML config file, collecting unknown keys and
// validation errors as warnings instead of fatal errors. TOML syntax errors and
// file read errors remain fatal. Used by informational commands (drive list,
// status, whoami) that need to show what they can even when config has errors.
func LoadLenient(path string, logger *slog.Logger) (*Config, []ConfigWarning, error) {
	return loadLenientWithIO(path, logger, defaultConfigIO())
}

func loadLenientWithIO(path string, logger *slog.Logger, io configIO) (*Config, []ConfigWarning, error) {
	logger.Debug("loading config file (lenient)", "path", path)

	cfg := DefaultConfig()

	data, err := io.readManagedFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading config file %s: %w", path, err)
	}

	// TOML syntax errors are fatal — can't produce a Config at all.
	md, err := toml.Decode(string(data), cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}

	// Drive section parsing — structural errors become warnings, drives with
	// issues are skipped rather than failing the whole load.
	warnings := decodeDriveSectionsLenient(data, cfg)

	// Unknown global keys → warnings.
	for _, e := range collectUnknownGlobalKeyErrors(&md) {
		warnings = append(warnings, ConfigWarning{
			Message: e.Error(),
		})
	}

	// Deprecated key warnings (always logged, not collected).
	var rawMap map[string]any
	if _, decodeErr := toml.Decode(string(data), &rawMap); decodeErr == nil {
		WarnDeprecatedKeys(rawMap, logger)
	}

	// Validation errors → warnings.
	for _, e := range collectValidationErrors(cfg) {
		warnings = append(warnings, ConfigWarning{
			Message: e.Error(),
		})
	}

	logger.Debug("config file parsed (lenient)",
		"path", path,
		"drive_count", len(cfg.Drives),
		"warning_count", len(warnings),
	)

	return cfg, warnings, nil
}

// LoadOrDefaultLenient reads a TOML config file leniently if it exists,
// otherwise returns default Config with no warnings.
func LoadOrDefaultLenient(path string, logger *slog.Logger) (*Config, []ConfigWarning, error) {
	return loadOrDefaultLenientWithIO(path, logger, defaultConfigIO())
}

func loadOrDefaultLenientWithIO(path string, logger *slog.Logger, io configIO) (*Config, []ConfigWarning, error) {
	if _, err := io.statManagedPath(path); errors.Is(err, os.ErrNotExist) {
		logger.Debug("config file not found, using defaults", "path", path)

		return DefaultConfig(), nil, nil
	} else if err != nil {
		return nil, nil, fmt.Errorf("stating config file %s: %w", path, err)
	}

	return loadLenientWithIO(path, logger, io)
}

// ResolveDrive loads configuration and applies the four-layer override chain:
// defaults -> config file -> environment variables -> CLI flags.
// It returns the fully resolved drive configuration and the raw parsed config
// (needed by driveops.SessionProvider for shared drive token resolution).
func ResolveDrive(env EnvOverrides, cli CLIOverrides, logger *slog.Logger) (*ResolvedDrive, *Config, error) {
	// Step 1: resolve config path (CLI > env > default).
	cfgPath := ResolveConfigPath(env, cli, logger)

	// Step 2: load config file (returns defaults if no file exists).
	cfg, err := LoadOrDefault(cfgPath, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
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
	canonicalID, drive, err := MatchDrive(cfg, selector, logger)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, fmt.Errorf("config validation: %w", err)
	}

	return resolved, cfg, nil
}

// ResolveDrives resolves multiple drives from the config, applying global
// defaults and per-drive overrides. When selectors is non-empty, only drives
// matching those selectors (via MatchDrive) are included. When includePaused
// is false, paused drives are excluded. Results are sorted by canonical ID
// for deterministic ordering.
func ResolveDrives(cfg *Config, selectors []string, includePaused bool, logger *slog.Logger) ([]*ResolvedDrive, error) {
	if len(cfg.Drives) == 0 {
		return nil, nil
	}

	// Determine which drives to resolve.
	type candidate struct {
		cid   driveid.CanonicalID
		drive Drive
	}

	var candidates []candidate

	if len(selectors) > 0 {
		// Filter by selectors — each selector matches one drive.
		for _, sel := range selectors {
			cid, drive, err := MatchDrive(cfg, sel, logger)
			if err != nil {
				return nil, fmt.Errorf("resolving selector %q: %w", sel, err)
			}

			candidates = append(candidates, candidate{cid: cid, drive: drive})
		}
	} else {
		// All drives.
		for id := range cfg.Drives {
			candidates = append(candidates, candidate{cid: id, drive: cfg.Drives[id]})
		}
	}

	var resolved []*ResolvedDrive

	for i := range candidates {
		rd := buildResolvedDrive(cfg, candidates[i].cid, &candidates[i].drive, logger)

		// Skip paused drives unless explicitly included.
		if !includePaused && rd.Paused {
			logger.Debug("skipping paused drive", "canonical_id", candidates[i].cid.String())

			continue
		}

		resolved = append(resolved, rd)
	}

	// Sort by canonical ID for deterministic ordering.
	slices.SortFunc(resolved, func(a, b *ResolvedDrive) int {
		return cmp.Compare(a.CanonicalID.String(), b.CanonicalID.String())
	})

	logger.Debug("resolved drives", "count", len(resolved), "total", len(cfg.Drives))

	return resolved, nil
}

// ClearExpiredPauses removes paused/paused_until keys from drives whose timed
// pause has expired. This allows the drive to participate in the next resolve.
// Operates on both the in-memory config (so ResolveDrives sees the change
// immediately) and the on-disk config file (so the stale keys don't persist).
func ClearExpiredPauses(cfgPath string, cfg *Config, now time.Time, logger *slog.Logger) {
	for cid := range cfg.Drives {
		d := cfg.Drives[cid]

		// Skip drives that aren't paused or are still actively paused.
		// IsPaused handles all the expiry logic — if it returns true, the
		// pause is still valid and should not be cleared.
		rawPaused := d.Paused != nil && *d.Paused
		if !rawPaused || d.IsPaused(now) {
			continue
		}

		// Timed pause has expired — clean up config file and in-memory state.
		logger.Info("clearing expired timed pause",
			slog.String("drive", cid.String()),
		)

		if delErr := DeleteDriveKey(cfgPath, cid, "paused"); delErr != nil {
			logger.Warn("could not clear paused key", slog.String("error", delErr.Error()))
		}

		if delErr := DeleteDriveKey(cfgPath, cid, "paused_until"); delErr != nil {
			logger.Warn("could not clear paused_until key", slog.String("error", delErr.Error()))
		}

		// Update in-memory config so ResolveDrives sees the unpaused state
		// without requiring a re-load.
		d.Paused = nil
		d.PausedUntil = nil
		cfg.Drives[cid] = d
	}
}

// ResolveConfigPath determines the config file path using the three-layer
// priority: CLI flag > environment variable > platform default. This is the
// single correct implementation of config path resolution — all callers
// (PersistentPreRunE, ResolveDrive, auth commands) should use this.
func ResolveConfigPath(env EnvOverrides, cli CLIOverrides, logger *slog.Logger) string {
	cfgPath := DefaultConfigPath()
	source := defaultTransferOrder

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
