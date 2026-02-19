package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Validation range constants.
const (
	minParallelWorkers = 1
	maxParallelWorkers = 16
	minPercentage      = 1
	maxPercentage      = 100
	minBigDelete       = 1
	minTombstoneDays   = 0
	minLogRetention    = 1
	minFullscanNonZero = 2
	chunkAlignBytes    = 327680     // 320 KiB alignment for upload chunks
	minChunkBytes      = 10_485_760 // 10 MiB
	maxChunkBytes      = 62_914_560 // 60 MiB
	minPollInterval    = 5 * time.Minute
	minShutdownTimeout = 5 * time.Second
	minConnectTimeout  = 1 * time.Second
	minDataTimeout     = 5 * time.Second
	octalBase          = 8
	minOctalDigits     = 3
	maxOctalDigits     = 4
	maxOctalValue      = 0o777
	schedulePartCount  = 2
	maxScheduleHour    = 23
	maxScheduleMinute  = 59
)

// Validate checks all configuration values and returns all errors found.
// It does not stop at the first error, collecting them all for a single
// comprehensive report.
func Validate(cfg *Config) error {
	var errs []error

	errs = append(errs, validateProfiles(cfg.Profiles)...)
	errs = append(errs, validateFilter(&cfg.Filter)...)
	errs = append(errs, validateTransfers(&cfg.Transfers)...)
	errs = append(errs, validateSafety(&cfg.Safety)...)
	errs = append(errs, validateSync(&cfg.Sync)...)
	errs = append(errs, validateLogging(&cfg.Logging)...)
	errs = append(errs, validateNetwork(&cfg.Network)...)

	return errors.Join(errs...)
}

func validateFilter(f *FilterConfig) []error {
	var errs []error

	if f.MaxFileSize != "" && f.MaxFileSize != "0" {
		if _, err := parseSize(f.MaxFileSize); err != nil {
			errs = append(errs, fmt.Errorf("filter.max_file_size: %w", err))
		}
	}

	for _, p := range f.SyncPaths {
		if !strings.HasPrefix(p, "/") {
			errs = append(errs, fmt.Errorf("filter.sync_paths: path %q must start with /", p))
		}
	}

	if f.IgnoreMarker == "" {
		errs = append(errs, errors.New("filter.ignore_marker: must not be empty"))
	}

	return errs
}

func validateTransfers(t *TransfersConfig) []error {
	var errs []error

	errs = append(errs, validateWorkerCount("transfers.parallel_downloads", t.ParallelDownloads)...)
	errs = append(errs, validateWorkerCount("transfers.parallel_uploads", t.ParallelUploads)...)
	errs = append(errs, validateWorkerCount("transfers.parallel_checkers", t.ParallelCheckers)...)
	errs = append(errs, validateChunkSize(t.ChunkSize)...)
	errs = append(errs, validateTransferOrder(t.TransferOrder)...)
	errs = append(errs, validateBandwidthSchedule(t.BandwidthSchedule)...)

	return errs
}

func validateWorkerCount(field string, n int) []error {
	if n < minParallelWorkers || n > maxParallelWorkers {
		return []error{fmt.Errorf("%s: must be between %d and %d, got %d",
			field, minParallelWorkers, maxParallelWorkers, n)}
	}

	return nil
}

func validateChunkSize(s string) []error {
	bytes, err := parseSize(s)
	if err != nil {
		return []error{fmt.Errorf("transfers.chunk_size: %w", err)}
	}

	if bytes < minChunkBytes || bytes > maxChunkBytes {
		return []error{fmt.Errorf("transfers.chunk_size: must be between 10MiB and 60MiB, got %s", s)}
	}

	if bytes%chunkAlignBytes != 0 {
		return []error{fmt.Errorf(
			"transfers.chunk_size: must be a multiple of 320 KiB (%d bytes), got %s (%d bytes)",
			chunkAlignBytes, s, bytes)}
	}

	return nil
}

var validTransferOrders = map[string]bool{
	"default":   true,
	"size_asc":  true,
	"size_desc": true,
	"name_asc":  true,
	"name_desc": true,
}

func validateTransferOrder(order string) []error {
	if !validTransferOrders[order] {
		return []error{fmt.Errorf(
			"transfers.transfer_order: must be one of default, size_asc, size_desc, name_asc, name_desc; got %q", order)}
	}

	return nil
}

func validateBandwidthSchedule(entries []BandwidthScheduleEntry) []error {
	var errs []error

	prevMinutes := -1

	for i := range entries {
		minutes, err := parseScheduleTime(entries[i].Time)
		if err != nil {
			errs = append(errs, fmt.Errorf("transfers.bandwidth_schedule[%d].time: %w", i, err))

			continue
		}

		if prevMinutes >= 0 && minutes <= prevMinutes {
			errs = append(errs, fmt.Errorf("transfers.bandwidth_schedule: entries must be sorted by time; %q is not after %q",
				entries[i].Time, entries[max(0, i-1)].Time))
		}

		prevMinutes = minutes
	}

	return errs
}

// parseScheduleTime parses "HH:MM" and returns total minutes since midnight.
func parseScheduleTime(s string) (int, error) {
	parts := strings.SplitN(s, ":", schedulePartCount)
	if len(parts) != schedulePartCount {
		return 0, fmt.Errorf("invalid time format %q: expected HH:MM", s)
	}

	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > maxScheduleHour {
		return 0, fmt.Errorf("invalid hour in %q: must be 00-23", s)
	}

	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > maxScheduleMinute {
		return 0, fmt.Errorf("invalid minute in %q: must be 00-59", s)
	}

	return hour*int(time.Hour/time.Minute) + minute, nil
}

func validateSafety(s *SafetyConfig) []error {
	var errs []error

	if s.BigDeleteThreshold < minBigDelete {
		errs = append(errs, fmt.Errorf("safety.big_delete_threshold: must be >= %d, got %d",
			minBigDelete, s.BigDeleteThreshold))
	}

	if s.BigDeletePercentage < minPercentage || s.BigDeletePercentage > maxPercentage {
		errs = append(errs, fmt.Errorf("safety.big_delete_percentage: must be between %d and %d, got %d",
			minPercentage, maxPercentage, s.BigDeletePercentage))
	}

	if s.BigDeleteMinItems < minBigDelete {
		errs = append(errs, fmt.Errorf("safety.big_delete_min_items: must be >= %d, got %d",
			minBigDelete, s.BigDeleteMinItems))
	}

	errs = append(errs, validateSafetyRemaining(s)...)

	return errs
}

func validateSafetyRemaining(s *SafetyConfig) []error {
	var errs []error

	if s.MinFreeSpace != "" && s.MinFreeSpace != "0" {
		if _, err := parseSize(s.MinFreeSpace); err != nil {
			errs = append(errs, fmt.Errorf("safety.min_free_space: %w", err))
		}
	}

	errs = append(errs, validateOctalPermission("safety.sync_dir_permissions", s.SyncDirPermissions)...)
	errs = append(errs, validateOctalPermission("safety.sync_file_permissions", s.SyncFilePermissions)...)

	if s.TombstoneRetentionDays < minTombstoneDays {
		errs = append(errs, fmt.Errorf("safety.tombstone_retention_days: must be >= %d, got %d",
			minTombstoneDays, s.TombstoneRetentionDays))
	}

	return errs
}

func validateOctalPermission(field, value string) []error {
	if value == "" {
		return []error{fmt.Errorf("%s: must not be empty", field)}
	}

	if len(value) < minOctalDigits || len(value) > maxOctalDigits {
		return []error{fmt.Errorf("%s: must be 3 or 4 octal digits, got %q", field, value)}
	}

	n, err := strconv.ParseInt(value, octalBase, 32)
	if err != nil {
		return []error{fmt.Errorf("%s: invalid octal value %q", field, value)}
	}

	if n < 0 || n > maxOctalValue {
		return []error{fmt.Errorf("%s: octal value out of range %q", field, value)}
	}

	return nil
}

func validateSync(s *SyncConfig) []error {
	var errs []error

	errs = append(errs, validateDurationMin("sync.poll_interval", s.PollInterval, minPollInterval)...)
	errs = append(errs, validateFullscanFrequency(s.FullscanFrequency)...)
	errs = append(errs, validateConflictStrategy(s.ConflictStrategy)...)
	errs = append(errs, validateDurationNonNeg("sync.conflict_reminder_interval", s.ConflictReminderInterval)...)
	errs = append(errs, validateDurationNonNeg("sync.verify_interval", s.VerifyInterval)...)
	errs = append(errs, validateDurationMin("sync.shutdown_timeout", s.ShutdownTimeout, minShutdownTimeout)...)

	return errs
}

func validateFullscanFrequency(n int) []error {
	if n != 0 && n < minFullscanNonZero {
		return []error{fmt.Errorf("sync.fullscan_frequency: must be 0 (disabled) or >= %d, got %d",
			minFullscanNonZero, n)}
	}

	return nil
}

func validateConflictStrategy(s string) []error {
	if s != "keep_both" {
		return []error{fmt.Errorf("sync.conflict_strategy: must be \"keep_both\", got %q", s)}
	}

	return nil
}

func validateDurationMin(field, value string, minimum time.Duration) []error {
	d, err := time.ParseDuration(value)
	if err != nil {
		return []error{fmt.Errorf("%s: invalid duration %q: %w", field, value, err)}
	}

	if d < minimum {
		return []error{fmt.Errorf("%s: must be >= %s, got %s", field, minimum, d)}
	}

	return nil
}

func validateDurationNonNeg(field, value string) []error {
	d, err := time.ParseDuration(value)
	if err != nil {
		return []error{fmt.Errorf("%s: invalid duration %q: %w", field, value, err)}
	}

	if d < 0 {
		return []error{fmt.Errorf("%s: must be >= 0, got %s", field, d)}
	}

	return nil
}

func validateLogging(l *LoggingConfig) []error {
	var errs []error

	errs = append(errs, validateLogLevel(l.LogLevel)...)
	errs = append(errs, validateLogFormat(l.LogFormat)...)

	if l.LogRetentionDays < minLogRetention {
		errs = append(errs, fmt.Errorf("logging.log_retention_days: must be >= %d, got %d",
			minLogRetention, l.LogRetentionDays))
	}

	return errs
}

var validLogLevels = map[string]bool{
	"debug": true,
	"info":  true,
	"warn":  true,
	"error": true,
}

func validateLogLevel(level string) []error {
	if !validLogLevels[level] {
		return []error{fmt.Errorf("logging.log_level: must be one of debug, info, warn, error; got %q", level)}
	}

	return nil
}

var validLogFormats = map[string]bool{
	"auto": true,
	"text": true,
	"json": true,
}

func validateLogFormat(format string) []error {
	if !validLogFormats[format] {
		return []error{fmt.Errorf("logging.log_format: must be one of auto, text, json; got %q", format)}
	}

	return nil
}

func validateNetwork(n *NetworkConfig) []error {
	var errs []error

	errs = append(errs, validateDurationMin("network.connect_timeout", n.ConnectTimeout, minConnectTimeout)...)
	errs = append(errs, validateDurationMin("network.data_timeout", n.DataTimeout, minDataTimeout)...)

	return errs
}

// parseSize converts a human-readable size string to bytes using the same
// library as internal/filter. This is a local wrapper to avoid cross-package
// imports within internal/.
func parseSize(s string) (int64, error) {
	if s == "" || s == "0" {
		return 0, nil
	}

	// Simple parser for size strings: number + optional suffix.
	// Supports: KB, MB, GB, TB (decimal), KiB, MiB, GiB, TiB (binary).
	s = strings.TrimSpace(s)

	return parseSizeWithSuffix(s)
}

// Size multiplier constants (decimal / SI).
const (
	kilobyte = 1000
	megabyte = 1000 * kilobyte
	gigabyte = 1000 * megabyte
	terabyte = 1000 * gigabyte
)

// Size multiplier constants (binary / IEC).
const (
	kibibyte = 1024
	mebibyte = 1024 * kibibyte
	gibibyte = 1024 * mebibyte
	tebibyte = 1024 * gibibyte
)

// parseSizeWithSuffix extracts a numeric prefix and size suffix, returning bytes.
func parseSizeWithSuffix(s string) (int64, error) {
	upper := strings.ToUpper(s)

	suffixes := []struct {
		suffix     string
		multiplier int64
	}{
		{"TIB", tebibyte},
		{"GIB", gibibyte},
		{"MIB", mebibyte},
		{"KIB", kibibyte},
		{"TB", terabyte},
		{"GB", gigabyte},
		{"MB", megabyte},
		{"KB", kilobyte},
		{"B", 1},
	}

	for _, sf := range suffixes {
		if strings.HasSuffix(upper, sf.suffix) {
			numStr := strings.TrimSpace(s[:len(s)-len(sf.suffix)])

			return parseSizeNumber(numStr, sf.multiplier, s)
		}
	}

	// No suffix: treat as raw bytes.
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}

	if n < 0 {
		return 0, fmt.Errorf("invalid size %q: must be non-negative", s)
	}

	return n, nil
}

func parseSizeNumber(numStr string, multiplier int64, original string) (int64, error) {
	n, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", original, err)
	}

	return int64(n * float64(multiplier)), nil
}
