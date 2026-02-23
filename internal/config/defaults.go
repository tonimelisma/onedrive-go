package config

// Default values for configuration options. These represent the "layer 0"
// of the four-layer override chain and are chosen to be safe, reasonable
// starting points that work for most users without any config file.
const (
	defaultIgnoreMarker        = ".odignore"
	defaultMaxFileSize         = "50GB"
	defaultParallelDownloads   = 8
	defaultParallelUploads     = 8
	defaultParallelCheckers    = 8
	defaultChunkSize           = "10MiB"
	defaultBandwidthLimit      = "0"
	defaultTransferOrder       = "default"
	defaultBigDeleteThreshold  = 1000
	defaultBigDeletePercentage = 50
	defaultBigDeleteMinItems   = 10
	defaultMinFreeSpace        = "1GB"
	defaultSyncDirPermissions  = "0700"
	defaultSyncFilePermissions = "0600"
	defaultPollInterval        = "5m"
	defaultFullscanFrequency   = 12
	defaultConflictStrategy    = "keep_both"
	defaultConflictReminder    = "1h"
	defaultVerifyInterval      = "0"
	defaultShutdownTimeout     = "30s"
	defaultLogLevel            = "info"
	defaultLogFormat           = "auto"
	defaultLogRetentionDays    = 30
	defaultConnectTimeout      = "10s"
	defaultDataTimeout         = "60s"
)

// DefaultConfig returns a Config populated with all default values.
// This is used both as the starting point for TOML decoding (so unset
// fields retain defaults) and as the fallback when no config file exists.
func DefaultConfig() *Config {
	return &Config{
		FilterConfig:    defaultFilterConfig(),
		TransfersConfig: defaultTransfersConfig(),
		SafetyConfig:    defaultSafetyConfig(),
		SyncConfig:      defaultSyncConfig(),
		LoggingConfig:   defaultLoggingConfig(),
		NetworkConfig:   defaultNetworkConfig(),
		Drives:          make(map[string]Drive),
	}
}

func defaultFilterConfig() FilterConfig {
	return FilterConfig{
		SkipDotfiles: false,
		SkipSymlinks: false,
		MaxFileSize:  defaultMaxFileSize,
		IgnoreMarker: defaultIgnoreMarker,
	}
}

func defaultTransfersConfig() TransfersConfig {
	return TransfersConfig{
		ParallelDownloads: defaultParallelDownloads,
		ParallelUploads:   defaultParallelUploads,
		ParallelCheckers:  defaultParallelCheckers,
		ChunkSize:         defaultChunkSize,
		BandwidthLimit:    defaultBandwidthLimit,
		TransferOrder:     defaultTransferOrder,
	}
}

func defaultSafetyConfig() SafetyConfig {
	return SafetyConfig{
		BigDeleteThreshold:  defaultBigDeleteThreshold,
		BigDeletePercentage: defaultBigDeletePercentage,
		BigDeleteMinItems:   defaultBigDeleteMinItems,
		MinFreeSpace:        defaultMinFreeSpace,
		UseRecycleBin:       true,
		UseLocalTrash:       true,
		SyncDirPermissions:  defaultSyncDirPermissions,
		SyncFilePermissions: defaultSyncFilePermissions,
	}
}

func defaultSyncConfig() SyncConfig {
	return SyncConfig{
		PollInterval:             defaultPollInterval,
		FullscanFrequency:        defaultFullscanFrequency,
		Websocket:                true,
		ConflictStrategy:         defaultConflictStrategy,
		ConflictReminderInterval: defaultConflictReminder,
		VerifyInterval:           defaultVerifyInterval,
		ShutdownTimeout:          defaultShutdownTimeout,
	}
}

func defaultLoggingConfig() LoggingConfig {
	return LoggingConfig{
		LogLevel:         defaultLogLevel,
		LogFormat:        defaultLogFormat,
		LogRetentionDays: defaultLogRetentionDays,
	}
}

func defaultNetworkConfig() NetworkConfig {
	return NetworkConfig{
		ConnectTimeout: defaultConnectTimeout,
		DataTimeout:    defaultDataTimeout,
	}
}
