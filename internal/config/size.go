package config

import (
	"fmt"
	"strconv"
	"strings"
)

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

// parseSize converts a human-readable size string to bytes.
// Supports both SI (KB, MB, GB, TB) and IEC (KiB, MiB, GiB, TiB) suffixes.
// Empty string and "0" return 0. A bare number is treated as raw bytes.
func parseSize(s string) (int64, error) {
	if s == "" || s == "0" {
		return 0, nil
	}

	s = strings.TrimSpace(s)

	return parseSizeWithSuffix(s)
}

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
