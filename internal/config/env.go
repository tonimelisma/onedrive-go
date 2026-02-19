package config

import "os"

// Environment variable names for overrides.
const (
	EnvConfig  = "ONEDRIVE_GO_CONFIG"
	EnvProfile = "ONEDRIVE_GO_PROFILE"
	EnvSyncDir = "ONEDRIVE_GO_SYNC_DIR"
)

// EnvOverrides holds values derived from environment variables.
// These are resolved by ApplyEnvOverrides and made available to callers.
type EnvOverrides struct {
	ConfigPath string // ONEDRIVE_GO_CONFIG: override config file path
	Profile    string // ONEDRIVE_GO_PROFILE: active profile name
	SyncDir    string // ONEDRIVE_GO_SYNC_DIR: sync directory override
}

// ReadEnvOverrides reads environment variables and returns any overrides found.
// This does not modify the Config; callers apply the relevant fields.
func ReadEnvOverrides() EnvOverrides {
	return EnvOverrides{
		ConfigPath: os.Getenv(EnvConfig),
		Profile:    os.Getenv(EnvProfile),
		SyncDir:    os.Getenv(EnvSyncDir),
	}
}
