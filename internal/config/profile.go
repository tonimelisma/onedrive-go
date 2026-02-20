package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Valid account types for profiles.
const (
	AccountTypePersonal   = "personal"
	AccountTypeBusiness   = "business"
	AccountTypeSharePoint = "sharepoint"
)

// Valid Azure AD endpoints for national clouds.
const (
	AzureEndpointUSL4 = "USL4"
	AzureEndpointUSL5 = "USL5"
	AzureEndpointDE   = "DE"
	AzureEndpointCN   = "CN"
)

// Default remote path when none is specified.
const defaultRemotePath = "/"

// Default profile name when --profile is omitted.
const defaultProfileName = "default"

// Profile represents a single OneDrive account configuration within a TOML
// config file. Per-profile section overrides (e.g. [profile.work.filter])
// completely replace the corresponding global section — individual fields
// are not merged.
type Profile struct {
	AccountType     string `toml:"account_type"`
	SyncDir         string `toml:"sync_dir"`
	RemotePath      string `toml:"remote_path"`
	DriveID         string `toml:"drive_id"`
	ApplicationID   string `toml:"application_id"`
	AzureADEndpoint string `toml:"azure_ad_endpoint"`
	AzureTenantID   string `toml:"azure_tenant_id"`

	// Per-profile section overrides (completely replace global sections).
	Filter    *FilterConfig    `toml:"filter,omitempty"`
	Transfers *TransfersConfig `toml:"transfers,omitempty"`
	Safety    *SafetyConfig    `toml:"safety,omitempty"`
	Sync      *SyncConfig      `toml:"sync,omitempty"`
	Logging   *LoggingConfig   `toml:"logging,omitempty"`
	Network   *NetworkConfig   `toml:"network,omitempty"`
}

// ResolvedProfile contains profile fields plus effective config sections
// after merging global defaults with per-profile overrides. This is the
// final product consumed by the CLI and sync engine.
type ResolvedProfile struct {
	Name            string
	AccountType     string
	SyncDir         string
	RemotePath      string
	DriveID         string
	ApplicationID   string
	AzureADEndpoint string
	AzureTenantID   string

	Filter    FilterConfig
	Transfers TransfersConfig
	Safety    SafetyConfig
	Sync      SyncConfig
	Logging   LoggingConfig
	Network   NetworkConfig
}

// ResolveProfile merges global defaults with profile-specific overrides.
// If profileName is empty, the default profile is selected. Section-level
// override semantics are "replace, not merge" — if a profile defines
// [profile.work.filter], that entire FilterConfig replaces the global one.
func ResolveProfile(cfg *Config, profileName string) (*ResolvedProfile, error) {
	name, err := resolveProfileName(cfg, profileName)
	if err != nil {
		return nil, err
	}

	profile := cfg.Profiles[name]

	resolved := &ResolvedProfile{
		Name:            name,
		AccountType:     profile.AccountType,
		SyncDir:         expandTilde(profile.SyncDir),
		RemotePath:      profile.RemotePath,
		DriveID:         profile.DriveID,
		ApplicationID:   profile.ApplicationID,
		AzureADEndpoint: profile.AzureADEndpoint,
		AzureTenantID:   profile.AzureTenantID,
	}

	if resolved.RemotePath == "" {
		resolved.RemotePath = defaultRemotePath
	}

	resolveProfileSections(resolved, &profile, cfg)

	return resolved, nil
}

// resolveProfileSections fills effective config sections on the resolved profile.
func resolveProfileSections(resolved *ResolvedProfile, profile *Profile, cfg *Config) {
	resolved.Filter = resolveSection(profile.Filter, cfg.Filter)
	resolved.Transfers = resolveSection(profile.Transfers, cfg.Transfers)
	resolved.Safety = resolveSection(profile.Safety, cfg.Safety)
	resolved.Sync = resolveSection(profile.Sync, cfg.Sync)
	resolved.Logging = resolveSection(profile.Logging, cfg.Logging)
	resolved.Network = resolveSection(profile.Network, cfg.Network)
}

// resolveSection returns the profile override if present, otherwise the global value.
func resolveSection[T any](profileOverride *T, global T) T {
	if profileOverride != nil {
		return *profileOverride
	}

	return global
}

// resolveProfileName determines which profile to use.
func resolveProfileName(cfg *Config, profileName string) (string, error) {
	if len(cfg.Profiles) == 0 {
		return "", fmt.Errorf("no profiles defined in config")
	}

	if profileName != "" {
		return lookupExplicitProfile(cfg, profileName)
	}

	return lookupDefaultProfile(cfg)
}

// lookupExplicitProfile validates that the named profile exists.
func lookupExplicitProfile(cfg *Config, name string) (string, error) {
	if _, ok := cfg.Profiles[name]; !ok {
		return "", fmt.Errorf("profile %q not found in config", name)
	}

	return name, nil
}

// lookupDefaultProfile finds the default profile when no name is given.
func lookupDefaultProfile(cfg *Config) (string, error) {
	if _, ok := cfg.Profiles[defaultProfileName]; ok {
		return defaultProfileName, nil
	}

	if len(cfg.Profiles) == 1 {
		for name := range cfg.Profiles {
			return name, nil
		}
	}

	return "", fmt.Errorf(
		"multiple profiles defined but none named %q; use --profile to select one",
		defaultProfileName)
}

// expandTilde replaces a leading "~/" with the user's home directory.
func expandTilde(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}

	return filepath.Join(home, path[2:])
}

// ProfileDBPath returns the state database path for a profile.
// Format: {dataDir}/state/{profile}.db
func ProfileDBPath(profileName string) string {
	dataDir := DefaultDataDir()
	if dataDir == "" {
		return ""
	}

	return filepath.Join(dataDir, "state", profileName+".db")
}

// ProfileTokenPath returns the OAuth token file path for a profile.
// Format: {configDir}/tokens/{profile}.json
func ProfileTokenPath(profileName string) string {
	configDir := DefaultConfigDir()
	if configDir == "" {
		return ""
	}

	return filepath.Join(configDir, "tokens", profileName+".json")
}
