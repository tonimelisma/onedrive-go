package config

import (
	"cmp"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ResolveDrive loads configuration and applies the four-layer override chain:
// defaults -> config file -> environment variables -> CLI flags.
// It returns the fully resolved drive configuration and the raw parsed config
// (needed by driveops.SessionProvider for shared drive token resolution).
func ResolveDrive(env EnvOverrides, cli CLIOverrides, logger *slog.Logger) (*ResolvedDrive, *Config, error) {
	return newDriveResolver(logger).resolveDrive(env, cli)
}

// ResolveDrives resolves multiple drives from the config, applying global
// defaults and per-drive overrides. When selectors is non-empty, only drives
// matching those selectors (via MatchDrive) are included. When includePaused
// is false, paused drives are excluded. Results are sorted by canonical ID
// for deterministic ordering.
func ResolveDrives(cfg *Config, selectors []string, includePaused bool, logger *slog.Logger) ([]*ResolvedDrive, error) {
	return newDriveResolver(logger).resolveDrives(cfg, selectors, includePaused)
}

// ResolveConfigPath determines the config file path using the three-layer
// priority: CLI flag > environment variable > platform default.
func ResolveConfigPath(env EnvOverrides, cli CLIOverrides, logger *slog.Logger) string {
	return newDriveResolver(logger).resolveConfigPath(env, cli)
}

// driveResolver keeps the override-chain logic in one place so single-drive and
// multi-drive resolution share the same selection and logging semantics.
type driveResolver struct {
	logger *slog.Logger
}

func newDriveResolver(logger *slog.Logger) driveResolver {
	return driveResolver{logger: logger}
}

func (r driveResolver) resolveDrive(env EnvOverrides, cli CLIOverrides) (*ResolvedDrive, *Config, error) {
	cfgPath := r.resolveConfigPath(env, cli)

	cfg, err := LoadOrDefault(cfgPath, r.logger)
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}

	selector := env.Drive
	if cli.Drive != "" {
		selector = cli.Drive
	}

	r.logger.Debug("drive selector resolved",
		"selector", selector,
		"source_env", env.Drive,
		"source_cli", cli.Drive,
	)

	canonicalID, drive, err := MatchDrive(cfg, selector, r.logger)
	if err != nil {
		return nil, nil, err
	}

	resolved := buildResolvedDrive(cfg, canonicalID, &drive, r.logger)

	if cli.DryRun != nil {
		resolved.DryRun = *cli.DryRun
		r.logger.Debug("CLI override applied", "dry_run", resolved.DryRun)
	}

	if err := ValidateResolved(resolved); err != nil {
		return nil, nil, fmt.Errorf("config validation: %w", err)
	}

	return resolved, cfg, nil
}

func (r driveResolver) resolveDrives(cfg *Config, selectors []string, includePaused bool) ([]*ResolvedDrive, error) {
	if len(cfg.Drives) == 0 {
		return nil, nil
	}

	type candidate struct {
		cid   driveid.CanonicalID
		drive Drive
	}

	var candidates []candidate

	if len(selectors) > 0 {
		for _, selector := range selectors {
			canonicalID, drive, err := MatchDrive(cfg, selector, r.logger)
			if err != nil {
				return nil, fmt.Errorf("resolving selector %q: %w", selector, err)
			}

			candidates = append(candidates, candidate{
				cid:   canonicalID,
				drive: drive,
			})
		}
	} else {
		for canonicalID := range cfg.Drives {
			candidates = append(candidates, candidate{
				cid:   canonicalID,
				drive: cfg.Drives[canonicalID],
			})
		}
	}

	var resolved []*ResolvedDrive

	for i := range candidates {
		rd := buildResolvedDrive(cfg, candidates[i].cid, &candidates[i].drive, r.logger)
		if !includePaused && rd.Paused {
			r.logger.Debug("skipping paused drive", "canonical_id", candidates[i].cid.String())

			continue
		}

		resolved = append(resolved, rd)
	}

	slices.SortFunc(resolved, func(a, b *ResolvedDrive) int {
		return cmp.Compare(a.CanonicalID.String(), b.CanonicalID.String())
	})

	r.logger.Debug("resolved drives", "count", len(resolved), "total", len(cfg.Drives))

	return resolved, nil
}

func (r driveResolver) resolveConfigPath(env EnvOverrides, cli CLIOverrides) string {
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

	r.logger.Debug("config path resolved", "path", cfgPath, "source", source)

	return cfgPath
}

// ClearExpiredPauses removes paused/paused_until keys from drives whose timed
// pause has expired. This allows the drive to participate in the next resolve.
// Operates on both the in-memory config (so ResolveDrives sees the change
// immediately) and the on-disk config file (so the stale keys don't persist).
func ClearExpiredPauses(cfgPath string, cfg *Config, now time.Time, logger *slog.Logger) {
	for cid := range cfg.Drives {
		d := cfg.Drives[cid]

		rawPaused := d.Paused != nil && *d.Paused
		if !rawPaused || d.IsPaused(now) {
			continue
		}

		logger.Info("clearing expired timed pause",
			slog.String("drive", cid.String()),
		)

		if delErr := DeleteDriveKey(cfgPath, cid, "paused"); delErr != nil {
			logger.Warn("could not clear paused key", slog.String("error", delErr.Error()))
		}

		if delErr := DeleteDriveKey(cfgPath, cid, "paused_until"); delErr != nil {
			logger.Warn("could not clear paused_until key", slog.String("error", delErr.Error()))
		}

		d.Paused = nil
		d.PausedUntil = nil
		cfg.Drives[cid] = d
	}
}
