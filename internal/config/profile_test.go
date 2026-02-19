package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- TOML Parsing ---

func TestLoad_SingleProfile(t *testing.T) {
	path := writeTestConfig(t, `
[profile.default]
account_type = "personal"
sync_dir = "~/OneDrive"
remote_path = "/"
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Profiles, 1)

	p := cfg.Profiles["default"]
	assert.Equal(t, "personal", p.AccountType)
	assert.Equal(t, "~/OneDrive", p.SyncDir)
	assert.Equal(t, "/", p.RemotePath)
}

func TestLoad_MultiProfile(t *testing.T) {
	path := writeTestConfig(t, `
[profile.personal]
account_type = "personal"
sync_dir = "~/OneDrive"

[profile.work]
account_type = "business"
sync_dir = "~/OneDrive-Work"
azure_tenant_id = "contoso.onmicrosoft.com"

[profile.sharepoint]
account_type = "sharepoint"
sync_dir = "~/SharePoint"
drive_id = "b!abc123"
remote_path = "/Shared Documents"
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Profiles, 3)

	assert.Equal(t, "personal", cfg.Profiles["personal"].AccountType)
	assert.Equal(t, "business", cfg.Profiles["work"].AccountType)
	assert.Equal(t, "sharepoint", cfg.Profiles["sharepoint"].AccountType)
	assert.Equal(t, "b!abc123", cfg.Profiles["sharepoint"].DriveID)
	assert.Equal(t, "/Shared Documents", cfg.Profiles["sharepoint"].RemotePath)
	assert.Equal(t, "contoso.onmicrosoft.com", cfg.Profiles["work"].AzureTenantID)
}

func TestLoad_ProfileAllFields(t *testing.T) {
	path := writeTestConfig(t, `
[profile.full]
account_type = "business"
sync_dir = "~/OneDrive-Full"
remote_path = "/Documents"
drive_id = "abc123"
application_id = "custom-app-id"
azure_ad_endpoint = "USL4"
azure_tenant_id = "tenant-guid"
`)
	cfg, err := Load(path)
	require.NoError(t, err)

	p := cfg.Profiles["full"]
	assert.Equal(t, "business", p.AccountType)
	assert.Equal(t, "~/OneDrive-Full", p.SyncDir)
	assert.Equal(t, "/Documents", p.RemotePath)
	assert.Equal(t, "abc123", p.DriveID)
	assert.Equal(t, "custom-app-id", p.ApplicationID)
	assert.Equal(t, "USL4", p.AzureADEndpoint)
	assert.Equal(t, "tenant-guid", p.AzureTenantID)
}

func TestLoad_ProfileWithSectionOverride(t *testing.T) {
	path := writeTestConfig(t, `
[filter]
skip_dotfiles = false
skip_files = ["*.tmp"]
ignore_marker = ".odignore"

[profile.default]
account_type = "personal"
sync_dir = "~/OneDrive"

[profile.default.filter]
skip_dotfiles = true
skip_files = ["*.log", "*.bak"]
ignore_marker = ".syncignore"
`)
	cfg, err := Load(path)
	require.NoError(t, err)

	p := cfg.Profiles["default"]
	require.NotNil(t, p.Filter)
	assert.True(t, p.Filter.SkipDotfiles)
	assert.Equal(t, []string{"*.log", "*.bak"}, p.Filter.SkipFiles)
	assert.Equal(t, ".syncignore", p.Filter.IgnoreMarker)

	// Global filter should be unchanged
	assert.False(t, cfg.Filter.SkipDotfiles)
	assert.Equal(t, []string{"*.tmp"}, cfg.Filter.SkipFiles)
}

func TestLoad_ProfileWithMultipleSectionOverrides(t *testing.T) {
	path := writeTestConfig(t, `
[profile.work]
account_type = "business"
sync_dir = "~/Work"

[profile.work.transfers]
parallel_downloads = 4
parallel_uploads = 4
parallel_checkers = 4
chunk_size = "20MiB"
bandwidth_limit = "0"
transfer_order = "default"

[profile.work.logging]
log_level = "debug"
log_format = "json"
log_retention_days = 7
`)
	cfg, err := Load(path)
	require.NoError(t, err)

	p := cfg.Profiles["work"]
	require.NotNil(t, p.Transfers)
	assert.Equal(t, 4, p.Transfers.ParallelDownloads)

	require.NotNil(t, p.Logging)
	assert.Equal(t, "debug", p.Logging.LogLevel)
	assert.Equal(t, "json", p.Logging.LogFormat)
}

// --- Profile Resolution ---

func TestResolveProfile_DefaultName(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType: "personal",
			SyncDir:     "~/OneDrive",
		},
	}

	resolved, err := ResolveProfile(cfg, "")
	require.NoError(t, err)
	assert.Equal(t, "default", resolved.Name)
	assert.Equal(t, "personal", resolved.AccountType)
}

func TestResolveProfile_ExplicitName(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"work": {
			AccountType: "business",
			SyncDir:     "~/Work",
		},
	}

	resolved, err := ResolveProfile(cfg, "work")
	require.NoError(t, err)
	assert.Equal(t, "work", resolved.Name)
	assert.Equal(t, "business", resolved.AccountType)
}

func TestResolveProfile_SingleProfileNoDefault(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"myprofile": {
			AccountType: "personal",
			SyncDir:     "~/OneDrive",
		},
	}

	resolved, err := ResolveProfile(cfg, "")
	require.NoError(t, err)
	assert.Equal(t, "myprofile", resolved.Name)
}

func TestResolveProfile_MultipleProfilesNoDefault_Error(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"work": {
			AccountType: "business",
			SyncDir:     "~/Work",
		},
		"personal": {
			AccountType: "personal",
			SyncDir:     "~/Personal",
		},
	}

	_, err := ResolveProfile(cfg, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple profiles")
	assert.Contains(t, err.Error(), "default")
}

func TestResolveProfile_NotFound(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"work": {
			AccountType: "business",
			SyncDir:     "~/Work",
		},
	}

	_, err := ResolveProfile(cfg, "nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestResolveProfile_NoProfiles(t *testing.T) {
	cfg := DefaultConfig()

	_, err := ResolveProfile(cfg, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no profiles defined")
}

func TestResolveProfile_GlobalSectionUsedWhenNoOverride(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Filter.SkipDotfiles = true
	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType: "personal",
			SyncDir:     "~/OneDrive",
		},
	}

	resolved, err := ResolveProfile(cfg, "")
	require.NoError(t, err)
	assert.True(t, resolved.Filter.SkipDotfiles)
}

func TestResolveProfile_PerProfileOverrideReplacesGlobal(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Filter.SkipDotfiles = true
	cfg.Filter.SkipFiles = []string{"*.tmp", "*.swp"}

	overrideFilter := FilterConfig{
		SkipDotfiles: false,
		SkipFiles:    []string{"*.log"},
		IgnoreMarker: ".odignore",
		MaxFileSize:  "50GB",
	}

	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType: "personal",
			SyncDir:     "~/OneDrive",
			Filter:      &overrideFilter,
		},
	}

	resolved, err := ResolveProfile(cfg, "")
	require.NoError(t, err)

	// Profile override completely replaces global
	assert.False(t, resolved.Filter.SkipDotfiles)
	assert.Equal(t, []string{"*.log"}, resolved.Filter.SkipFiles)
}

func TestResolveProfile_RemotePathDefaultsToSlash(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType: "personal",
			SyncDir:     "~/OneDrive",
			// RemotePath is empty
		},
	}

	resolved, err := ResolveProfile(cfg, "")
	require.NoError(t, err)
	assert.Equal(t, "/", resolved.RemotePath)
}

func TestResolveProfile_TildeExpanded(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType: "personal",
			SyncDir:     "~/OneDrive",
		},
	}

	resolved, err := ResolveProfile(cfg, "")
	require.NoError(t, err)

	home, homeErr := os.UserHomeDir()
	require.NoError(t, homeErr)
	assert.Equal(t, filepath.Join(home, "OneDrive"), resolved.SyncDir)
	assert.False(t, strings.HasPrefix(resolved.SyncDir, "~"))
}

// --- Validation ---

func TestValidate_Profile_MissingAccountType(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"default": {
			SyncDir: "~/OneDrive",
		},
	}

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "account_type")
}

func TestValidate_Profile_InvalidAccountType(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType: "enterprise",
			SyncDir:     "~/OneDrive",
		},
	}

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "account_type")
	assert.Contains(t, err.Error(), "enterprise")
}

func TestValidate_Profile_MissingSyncDir(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType: "personal",
		},
	}

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sync_dir")
}

func TestValidate_Profile_SharePointRequiresDriveID(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"sp": {
			AccountType: "sharepoint",
			SyncDir:     "~/SharePoint",
			// DriveID is empty
		},
	}

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drive_id")
	assert.Contains(t, err.Error(), "sharepoint")
}

func TestValidate_Profile_SharePointWithDriveID(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"sp": {
			AccountType: "sharepoint",
			SyncDir:     "~/SharePoint",
			DriveID:     "b!abc123",
		},
	}

	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidate_Profile_InvalidAzureEndpoint(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType:     "business",
			SyncDir:         "~/OneDrive",
			AzureADEndpoint: "INVALID",
		},
	}

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "azure_ad_endpoint")
}

func TestValidate_Profile_ValidAzureEndpoints(t *testing.T) {
	for _, endpoint := range []string{"", "USL4", "USL5", "DE", "CN"} {
		cfg := DefaultConfig()
		cfg.Profiles = map[string]Profile{
			"default": {
				AccountType:     "business",
				SyncDir:         "~/OneDrive",
				AzureADEndpoint: endpoint,
			},
		}

		err := Validate(cfg)
		assert.NoError(t, err, "expected endpoint %q to be valid", endpoint)
	}
}

func TestValidate_Profile_DuplicateSyncDir(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"one": {
			AccountType: "personal",
			SyncDir:     "~/OneDrive",
		},
		"two": {
			AccountType: "business",
			SyncDir:     "~/OneDrive",
		},
	}

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflicts with")
}

func TestValidate_Profile_DuplicateSyncDirTildeExpanded(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"one": {
			AccountType: "personal",
			SyncDir:     "~/OneDrive",
		},
		"two": {
			AccountType: "business",
			SyncDir:     filepath.Join(home, "OneDrive"),
		},
	}

	err = Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflicts with")
}

func TestValidate_Profile_OverrideValidationError(t *testing.T) {
	badFilter := FilterConfig{
		IgnoreMarker: "", // Must not be empty
		MaxFileSize:  "50GB",
	}

	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType: "personal",
			SyncDir:     "~/OneDrive",
			Filter:      &badFilter,
		},
	}

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ignore_marker")
}

func TestValidate_Profile_ValidAccountTypes(t *testing.T) {
	for _, at := range []string{"personal", "business", "sharepoint"} {
		cfg := DefaultConfig()
		p := Profile{
			AccountType: at,
			SyncDir:     "~/OneDrive",
		}

		if at == "sharepoint" {
			p.DriveID = "b!abc123"
		}

		cfg.Profiles = map[string]Profile{"default": p}
		err := Validate(cfg)
		assert.NoError(t, err, "expected account_type %q to be valid", at)
	}
}

func TestValidate_NoProfiles_StillValid(t *testing.T) {
	cfg := DefaultConfig()
	err := Validate(cfg)
	assert.NoError(t, err)
}

// --- Path Derivation ---

func TestProfileDBPath(t *testing.T) {
	path := ProfileDBPath("work")
	assert.NotEmpty(t, path)
	assert.True(t, strings.HasSuffix(path, "work.db"))
	assert.Contains(t, path, "state")
}

func TestProfileTokenPath(t *testing.T) {
	path := ProfileTokenPath("work")
	assert.NotEmpty(t, path)
	assert.True(t, strings.HasSuffix(path, "work.json"))
	assert.Contains(t, path, "tokens")
}

func TestProfileDBPath_PlatformSpecific(t *testing.T) {
	path := ProfileDBPath("default")

	switch runtime.GOOS {
	case platformDarwin:
		assert.Contains(t, path, "Library/Application Support")
	case platformLinux:
		assert.Contains(t, path, ".local/share")
	}
}

func TestProfileTokenPath_PlatformSpecific(t *testing.T) {
	path := ProfileTokenPath("default")

	switch runtime.GOOS {
	case platformDarwin:
		assert.Contains(t, path, "Library/Application Support")
	case platformLinux:
		assert.Contains(t, path, ".config")
	}
}

// --- Tilde Expansion ---

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(home, "OneDrive"), expandTilde("~/OneDrive"))
	assert.Equal(t, "/absolute/path", expandTilde("/absolute/path"))
	assert.Equal(t, "relative/path", expandTilde("relative/path"))
	assert.Equal(t, "", expandTilde(""))
}

// --- Env Override Integration ---

func TestResolveProfile_EnvProfileOverride(t *testing.T) {
	// ONEDRIVE_GO_PROFILE selects the active profile
	t.Setenv(EnvProfile, "work")

	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType: "personal",
			SyncDir:     "~/OneDrive",
		},
		"work": {
			AccountType: "business",
			SyncDir:     "~/Work",
		},
	}

	overrides := ReadEnvOverrides()
	profileName := overrides.Profile

	resolved, err := ResolveProfile(cfg, profileName)
	require.NoError(t, err)
	assert.Equal(t, "work", resolved.Name)
	assert.Equal(t, "business", resolved.AccountType)
}

func TestResolveProfile_EnvSyncDirOverride(t *testing.T) {
	// ONEDRIVE_GO_SYNC_DIR overrides sync_dir on the active profile
	t.Setenv(EnvSyncDir, "/custom/sync")

	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType: "personal",
			SyncDir:     "~/OneDrive",
		},
	}

	overrides := ReadEnvOverrides()
	resolved, err := ResolveProfile(cfg, "")
	require.NoError(t, err)

	// Apply env override
	if overrides.SyncDir != "" {
		resolved.SyncDir = overrides.SyncDir
	}

	assert.Equal(t, "/custom/sync", resolved.SyncDir)
}

// --- Unknown Keys in Profile Sections ---

func TestLoad_UnknownKeyInProfile(t *testing.T) {
	path := writeTestConfig(t, `
[profile.default]
account_type = "personal"
sync_dir = "~/OneDrive"
unknown_field = "value"
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown config key")
}

func TestLoad_UnknownKeyInProfileSubsection(t *testing.T) {
	path := writeTestConfig(t, `
[profile.default]
account_type = "personal"
sync_dir = "~/OneDrive"

[profile.default.filter]
skip_dotfiles = true
unknown_option = true
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown config key")
}

func TestLoad_TypoInProfileSubsection_Suggestion(t *testing.T) {
	path := writeTestConfig(t, `
[profile.default]
account_type = "personal"
sync_dir = "~/OneDrive"

[profile.default.filter]
skip_dotfile = true
ignore_marker = ".odignore"
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skip_dotfiles")
}

func TestLoad_TypoInProfileDirectField_Suggestion(t *testing.T) {
	path := writeTestConfig(t, "[profile.default]\nacount_type = \"personal\"\nsync_dir = \"~/OneDrive\"\n")
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "account_type")
}

// --- Integration: Full Config with Profiles ---

func TestLoad_FullConfigWithProfiles(t *testing.T) {
	path := writeTestConfig(t, `
[profile.default]
account_type = "personal"
sync_dir = "~/OneDrive"

[profile.work]
account_type = "business"
sync_dir = "~/OneDrive-Work"

[profile.work.filter]
skip_dotfiles = true
max_file_size = "10GB"
ignore_marker = ".odignore"

[filter]
skip_dotfiles = false
max_file_size = "50GB"
ignore_marker = ".odignore"

[logging]
log_level = "debug"
`)
	cfg, err := Load(path)
	require.NoError(t, err)

	// Global filter
	assert.False(t, cfg.Filter.SkipDotfiles)
	assert.Equal(t, "50GB", cfg.Filter.MaxFileSize)

	// Profile override
	require.NotNil(t, cfg.Profiles["work"].Filter)
	assert.True(t, cfg.Profiles["work"].Filter.SkipDotfiles)
	assert.Equal(t, "10GB", cfg.Profiles["work"].Filter.MaxFileSize)

	// Resolve work profile: override replaces global
	resolved, resolveErr := ResolveProfile(cfg, "work")
	require.NoError(t, resolveErr)
	assert.True(t, resolved.Filter.SkipDotfiles)
	assert.Equal(t, "10GB", resolved.Filter.MaxFileSize)

	// Resolve default profile: uses global
	resolved, resolveErr = ResolveProfile(cfg, "default")
	require.NoError(t, resolveErr)
	assert.False(t, resolved.Filter.SkipDotfiles)
	assert.Equal(t, "50GB", resolved.Filter.MaxFileSize)
	assert.Equal(t, "debug", resolved.Logging.LogLevel)
}

func TestLoad_ProfileWithNoGlobalSections(t *testing.T) {
	path := writeTestConfig(t, `
[profile.default]
account_type = "personal"
sync_dir = "~/OneDrive"
`)
	cfg, err := Load(path)
	require.NoError(t, err)

	resolved, resolveErr := ResolveProfile(cfg, "")
	require.NoError(t, resolveErr)

	// Should get built-in defaults for all sections
	assert.Equal(t, "info", resolved.Logging.LogLevel)
	assert.Equal(t, 8, resolved.Transfers.ParallelDownloads)
	assert.Equal(t, "5m", resolved.Sync.PollInterval)
}

// --- Edge Cases ---

func TestResolveProfile_PreservesNonTildePaths(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType: "personal",
			SyncDir:     "/absolute/path/OneDrive",
		},
	}

	resolved, err := ResolveProfile(cfg, "")
	require.NoError(t, err)
	assert.Equal(t, "/absolute/path/OneDrive", resolved.SyncDir)
}

func TestResolveProfile_AllOverrideSections(t *testing.T) {
	transfers := TransfersConfig{
		ParallelDownloads: 2,
		ParallelUploads:   2,
		ParallelCheckers:  2,
		ChunkSize:         "20MiB",
		BandwidthLimit:    "0",
		TransferOrder:     "default",
	}
	safety := SafetyConfig{
		BigDeleteThreshold:     500,
		BigDeletePercentage:    25,
		BigDeleteMinItems:      5,
		MinFreeSpace:           "2GB",
		UseRecycleBin:          false,
		UseLocalTrash:          false,
		SyncDirPermissions:     "0700",
		SyncFilePermissions:    "0600",
		TombstoneRetentionDays: 15,
	}
	syncCfg := SyncConfig{
		PollInterval:             "10m",
		FullscanFrequency:        6,
		Websocket:                false,
		ConflictStrategy:         "keep_both",
		ConflictReminderInterval: "2h",
		VerifyInterval:           "0",
		ShutdownTimeout:          "30s",
	}
	logging := LoggingConfig{
		LogLevel:         "debug",
		LogFormat:        "json",
		LogRetentionDays: 7,
	}
	network := NetworkConfig{
		ConnectTimeout: "30s",
		DataTimeout:    "120s",
		ForceHTTP11:    true,
	}

	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType: "personal",
			SyncDir:     "~/OneDrive",
			Transfers:   &transfers,
			Safety:      &safety,
			Sync:        &syncCfg,
			Logging:     &logging,
			Network:     &network,
		},
	}

	resolved, err := ResolveProfile(cfg, "")
	require.NoError(t, err)

	assert.Equal(t, 2, resolved.Transfers.ParallelDownloads)
	assert.Equal(t, 500, resolved.Safety.BigDeleteThreshold)
	assert.False(t, resolved.Sync.Websocket)
	assert.Equal(t, "debug", resolved.Logging.LogLevel)
	assert.True(t, resolved.Network.ForceHTTP11)
}
