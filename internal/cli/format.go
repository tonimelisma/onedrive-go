package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func outputWriterOrDefault(w io.Writer) io.Writer {
	if w == nil {
		return os.Stdout
	}

	return w
}

func statusWriterOrDefault(w io.Writer) io.Writer {
	if w == nil {
		return os.Stderr
	}

	return w
}

func writeWarningf(w io.Writer, format string, args ...any) {
	if err := writef(statusWriterOrDefault(w), format, args...); err != nil {
		return
	}
}

func writef(w io.Writer, format string, args ...any) error {
	_, err := fmt.Fprintf(w, format, args...)
	if err != nil {
		return fmt.Errorf("write formatted output: %w", err)
	}

	return nil
}

func writeln(w io.Writer, args ...any) error {
	_, err := fmt.Fprintln(w, args...)
	if err != nil {
		return fmt.Errorf("write line output: %w", err)
	}

	return nil
}

// Statusf prints a status message to the StatusWriter (default: stderr)
// unless quiet mode is set.
func (cc *CLIContext) Statusf(format string, args ...any) {
	if cc.StatusWriter == nil || cc.Flags.Quiet {
		return
	}

	if err := writef(cc.StatusWriter, format, args...); err != nil {
		cc.recordStatusError(fmt.Errorf("write status output: %w", err))
	}
}

func (cc *CLIContext) recordStatusError(err error) {
	if err == nil {
		return
	}

	cc.statusMu.Lock()
	defer cc.statusMu.Unlock()

	if cc.statusErr == nil {
		cc.statusErr = err
	}
}

func (cc *CLIContext) StatusError() error {
	cc.statusMu.Lock()
	defer cc.statusMu.Unlock()

	return cc.statusErr
}

// Output returns the configured primary output writer, defaulting to stdout.
func (cc *CLIContext) Output() io.Writer {
	if cc == nil {
		return outputWriterOrDefault(nil)
	}

	return outputWriterOrDefault(cc.OutputWriter)
}

// Status returns the configured status/progress writer, defaulting to stderr.
func (cc *CLIContext) Status() io.Writer {
	if cc == nil {
		return statusWriterOrDefault(nil)
	}

	return statusWriterOrDefault(cc.StatusWriter)
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
func printTable(w io.Writer, headers []string, rows [][]string) error {
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
	if err := printRow(w, headers, widths); err != nil {
		return err
	}

	// Print rows.
	for _, row := range rows {
		if err := printRow(w, row, widths); err != nil {
			return err
		}
	}

	return nil
}

// printRow writes a single padded row.
func printRow(w io.Writer, cells []string, widths []int) error {
	parts := make([]string, len(cells))
	for i, cell := range cells {
		parts[i] = fmt.Sprintf("%-*s", widths[i], cell)
	}

	return writeln(w, strings.Join(parts, "  "))
}
