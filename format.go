package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// statusf prints a status message to stderr unless quiet mode is set.
func statusf(quiet bool, format string, args ...any) {
	if !quiet {
		fmt.Fprintf(os.Stderr, format, args...)
	}
}

// Statusf prints a status message to stderr unless quiet mode is set.
// Method form of statusf — avoids threading `quiet bool` through call chains.
func (cc *CLIContext) Statusf(format string, args ...any) {
	statusf(cc.Flags.Quiet, format, args...)
}

// Size unit constants for human-readable formatting.
const (
	sizeKB = 1024
	sizeMB = 1024 * 1024
	sizeGB = 1024 * 1024 * 1024
	sizeTB = 1024 * 1024 * 1024 * 1024
)

// formatSize returns a human-readable size string (e.g. "1.2 MB").
func formatSize(bytes int64) string {
	switch {
	case bytes >= sizeTB:
		return fmt.Sprintf("%.1f TB", float64(bytes)/float64(sizeTB))
	case bytes >= sizeGB:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(sizeGB))
	case bytes >= sizeMB:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(sizeMB))
	case bytes >= sizeKB:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(sizeKB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// formatTime returns a compact timestamp for display.
func formatTime(t time.Time) string {
	now := time.Now()

	// Same calendar year: show "Jan  2 15:04"
	if t.Year() == now.Year() {
		return t.Format("Jan _2 15:04")
	}

	// Different year: show "Jan  2  2006"
	return t.Format("Jan _2  2006")
}

// printTable writes aligned columns to the given writer.
// headers and each row must have the same length.
func printTable(w io.Writer, headers []string, rows [][]string) {
	// Compute column widths.
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}

	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	// Print header.
	printRow(w, headers, widths)

	// Print rows.
	for _, row := range rows {
		printRow(w, row, widths)
	}
}

// printRow writes a single padded row.
func printRow(w io.Writer, cells []string, widths []int) {
	parts := make([]string, len(cells))
	for i, cell := range cells {
		parts[i] = fmt.Sprintf("%-*s", widths[i], cell)
	}

	fmt.Fprintln(w, strings.Join(parts, "  "))
}
