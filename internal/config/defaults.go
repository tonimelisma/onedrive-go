package config

import (
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Default values for configuration options. These represent the "layer 0"
// of the four-layer override chain and are chosen to be safe, reasonable
// starting points that work for most users without any config file.
const (
	defaultTransferWorkers    = 8
	defaultCheckWorkers       = 4
	defaultMinFreeSpace       = "1GB"
	defaultPollInterval       = "5m"
	defaultSafetyScanInterval = "5m"
	defaultLogLevel           = "info"
	defaultLogFormat          = "auto"
	defaultLogRetentionDays   = 30
)

// DefaultConfig returns a Config populated with all default values.
// This is used both as the starting point for TOML decoding (so unset
// fields retain defaults) and as the fallback when no config file exists.
func DefaultConfig() *Config {
	return &Config{
		TransfersConfig: defaultTransfersConfig(),
		SafetyConfig:    defaultSafetyConfig(),
		SyncConfig:      defaultSyncConfig(),
		LoggingConfig:   defaultLoggingConfig(),
		Drives:          make(map[driveid.CanonicalID]Drive),
	}
}

func defaultTransfersConfig() TransfersConfig {
	return TransfersConfig{
		TransferWorkers: defaultTransferWorkers,
		CheckWorkers:    defaultCheckWorkers,
	}
}

func defaultSafetyConfig() SafetyConfig {
	return SafetyConfig{
		MinFreeSpace: defaultMinFreeSpace,
	}
}

func defaultSyncConfig() SyncConfig {
	return SyncConfig{
		PollInterval:       defaultPollInterval,
		Websocket:          false,
		SafetyScanInterval: defaultSafetyScanInterval,
	}
}

func defaultLoggingConfig() LoggingConfig {
	return LoggingConfig{
		LogLevel:         defaultLogLevel,
		LogFormat:        defaultLogFormat,
		LogRetentionDays: defaultLogRetentionDays,
	}
}
