package config

import "os"

// Environment variable names for overrides. These form the third layer in the
// four-layer override chain (defaults -> file -> environment -> CLI flags).
const (
	EnvConfig = "ONEDRIVE_GO_CONFIG"
	EnvDrive  = "ONEDRIVE_GO_DRIVE"
)

// EnvOverrides holds values derived from environment variables.
// These are resolved by ReadEnvOverrides and passed to ResolveDrive() for
// application in the correct precedence order.
type EnvOverrides struct {
	ConfigPath string // ONEDRIVE_GO_CONFIG: override config file path
	Drive      string // ONEDRIVE_GO_DRIVE: drive selector (canonical ID, alias, or partial match)
}

// ReadEnvOverrides reads environment variables and returns any overrides found.
// This does not modify the Config; callers pass the result to ResolveDrive().
func ReadEnvOverrides() EnvOverrides {
	return EnvOverrides{
		ConfigPath: os.Getenv(EnvConfig),
		Drive:      os.Getenv(EnvDrive),
	}
}
