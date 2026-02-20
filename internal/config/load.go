package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// Load reads and parses a TOML config file, validates it, and returns the
// resulting Config. Unknown keys are treated as fatal errors with "did you
// mean?" suggestions — this strictness is deliberate because silently
// ignoring a typo in a config file leads to hard-to-debug behavior.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	md, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}

	if err := checkUnknownKeys(&md); err != nil {
		return nil, err
	}

	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return cfg, nil
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

// Resolve loads configuration and applies the four-layer override chain:
// defaults -> config file -> environment variables -> CLI flags.
// It returns a fully resolved and validated profile ready for use.
// The precedence order ensures CLI flags always win, matching user expectations
// for one-off overrides without editing the config file.
func Resolve(env EnvOverrides, cli CLIOverrides) (*ResolvedProfile, error) {
	// 1. Resolve config path: CLI > env > default
	cfgPath := DefaultConfigPath()
	if env.ConfigPath != "" {
		cfgPath = env.ConfigPath
	}

	if cli.ConfigPath != "" {
		cfgPath = cli.ConfigPath
	}

	// 2. Load config file (returns defaults if no file exists)
	cfg, err := LoadOrDefault(cfgPath)
	if err != nil {
		return nil, err
	}

	// 3. Resolve profile name: CLI > env > "default"
	profileName := cli.Profile
	if profileName == "" {
		profileName = env.Profile
	}

	// 4. If no profiles defined, create a synthetic profile for out-of-box use.
	// Use the requested profile name (or "default") so that --profile works
	// without a config file — important for CI and first-run experience.
	if len(cfg.Profiles) == 0 {
		syntheticName := defaultProfileName
		if profileName != "" {
			syntheticName = profileName
		}

		cfg.Profiles = map[string]Profile{
			syntheticName: {
				AccountType: AccountTypePersonal,
				SyncDir:     "~/OneDrive",
			},
		}
	}

	// 5. Merge global + profile
	resolved, err := ResolveProfile(cfg, profileName)
	if err != nil {
		return nil, err
	}

	// 6. Apply env overrides
	if env.SyncDir != "" {
		resolved.SyncDir = env.SyncDir
	}

	// 7. Apply CLI overrides (pointer fields: nil = not specified)
	if cli.SyncDir != nil {
		resolved.SyncDir = *cli.SyncDir
	}

	if cli.DryRun != nil {
		resolved.Sync.DryRun = *cli.DryRun
	}

	// 8. Validate the final resolved profile
	if err := ValidateResolved(resolved); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	return resolved, nil
}
