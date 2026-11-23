package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

const unknownTimeLabel = "unknown"

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

// writeWarningf writes a warning to w (stderr when w is nil) and returns the
// write's error. It serves main's own last lines, written after the command
// returned, and the watch's startup warnings, whose hook records a failure in
// the command context itself. A command warns through CLIContext.warnf.
func writeWarningf(w io.Writer, format string, args ...any) error {
	return writef(statusWriterOrDefault(w), format, args...)
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

	cc.ProgressDone()
	if err := writef(cc.StatusWriter, format, args...); err != nil {
		cc.recordStatusError(fmt.Errorf("write status output: %w", err))
	}
}

// warnf writes a warning to the status stream (stderr by default). Unlike
// Statusf it writes under --quiet too, which keeps warnings. A write that
// fails is recorded like a failed status line.
func (cc *CLIContext) warnf(format string, args ...any) {
	cc.ProgressDone()
	if err := writeWarningf(cc.Status(), format, args...); err != nil {
		cc.recordStatusError(fmt.Errorf("write warning: %w", err))
	}
}

// recordStatusError keeps the first write the status stream refused, for
// main to read once the command has returned (StatusError). It takes
// statusMu, so a caller that holds the mutex releases it first.
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

// StatusError returns the first write the status stream refused, or nil.
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

// The item types the CLI prints, in text and in --json alike.
const (
	typeFile   = "file"
	typeFolder = "folder"
)

// itemType returns the printed type of a drive item: folder or file.
func itemType(item *graph.Item) string {
	return folderOrFile(item.IsFolder)
}

// folderOrFile returns the printed type of anything known only as a folder or
// not one.
func folderOrFile(isFolder bool) string {
	if isFolder {
		return typeFolder
	}

	return typeFile
}

// formatTime returns a compact timestamp for display.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return unknownTimeLabel
	}

	// Show the user's local time, as ls(1) does. Graph timestamps arrive in
	// UTC, so printing them unconverted was hours off with no zone label.
	t = t.Local()
	now := time.Now()

	// Same calendar year: show "Jan  2 15:04"
	if t.Year() == now.Year() {
		return t.Format("Jan _2 15:04")
	}

	// Different year: show "Jan  2  2006"
	return t.Format("Jan _2  2006")
}

// formatAPITime returns an RFC3339 timestamp for machine-readable output.
// Unknown timestamps stay empty rather than serializing as year 0001.
func formatAPITime(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// formatExactTime returns the full timestamp for text output, in the user's
// local time zone like every other time the CLI prints (ls, status), with the
// zone named so the exact value is unambiguous. --json output keeps UTC
// through formatAPITime.
func formatExactTime(t time.Time) string {
	if t.IsZero() {
		return unknownTimeLabel
	}

	return t.Local().Format("2006-01-02 15:04:05 MST")
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

// folderTransferJSONOutput is the --json document of a folder get or put:
// one entry per file transferred (getJSONOutput or putJSONOutput), the
// folders created, the bytes transferred, and every item that failed.
type folderTransferJSONOutput[F any] struct {
	Files          []F      `json:"files"`
	FoldersCreated int      `json:"folders_created"`
	TotalSize      int64    `json:"total_size"`
	Errors         []string `json:"errors"`
}

func (out folderTransferJSONOutput[F]) withEmptyLists() folderTransferJSONOutput[F] {
	out.Files = emptyIfNil(out.Files)
	out.Errors = emptyIfNil(out.Errors)

	return out
}

// bulkTransferError names every item a folder get/put could not transfer
// (as warnings on stderr, so they show even with --quiet) and fails the
// command, in text and --json mode alike. With cc nil the failures are
// already in the JSON document and only the exit status is set.
func bulkTransferError(cc *CLIContext, noun string, failures []string) error {
	if len(failures) == 0 {
		return nil
	}

	if cc != nil {
		for _, failure := range failures {
			cc.warnf("Failed: %s\n", failure)
		}
	}

	return fmt.Errorf("%s failed to %s", countItems(len(failures)), noun)
}

// countOf words every count the CLI prints: n of noun, singular for one and
// plural otherwise ("1 file", "0 files", "2 retries"). The plural adds s, or
// turns a final y after a consonant into ies.
func countOf[N ~int | ~int64](n N, noun string) string {
	if n == 1 {
		return "1 " + noun
	}

	return fmt.Sprintf("%d %s", n, pluralNoun(noun))
}

// pluralNoun is the plural of a regular noun, the only kind countOf words.
func pluralNoun(noun string) string {
	stem, endsInY := strings.CutSuffix(noun, "y")
	if endsInY && stem != "" && !strings.ContainsRune("aeiou", rune(stem[len(stem)-1])) {
		return stem + "ies"
	}

	return noun + "s"
}

// countItems renders "1 item" or "N items".
func countItems(n int) string {
	return countOf(n, "item")
}

// formatAPITimeForHumans renders a formatAPITime string the way formatTime
// renders a time, for text tables built from already-serialized records.
func formatAPITimeForHumans(apiTime string) string {
	if apiTime == "" {
		return unknownTimeLabel
	}

	parsed, err := time.Parse(time.RFC3339, apiTime)
	if err != nil {
		return apiTime
	}

	return formatTime(parsed)
}

// clearLine returns a terminal to column 0 (\r) and clears the old line's
// tail (\x1b[K).
const clearLine = "\r\x1b[K"

// Progressf redraws one transient progress line on stderr. It shows only
// when a person is watching (stderr is a terminal) and the run is neither
// --quiet nor --json; piped and scripted runs get the final summary only,
// instead of one stored line per chunk, second or file. A write that fails
// is recorded like a failed status line.
func (cc *CLIContext) Progressf(format string, args ...any) {
	if cc.StatusWriter == nil || cc.Flags.Quiet || cc.Flags.JSON || !isWriterTTY(cc.StatusWriter) {
		return
	}

	cc.recordStatusError(cc.drawProgress(fmt.Sprintf(format, args...)))
}

// drawProgress replaces the progress line with line under statusMu and
// returns the write's error for the caller to record once the mutex is
// released.
func (cc *CLIContext) drawProgress(line string) error {
	cc.statusMu.Lock()
	defer cc.statusMu.Unlock()

	if _, err := io.WriteString(cc.StatusWriter, clearLine+line); err != nil {
		return fmt.Errorf("write progress line: %w", err)
	}
	cc.progressShown = true

	return nil
}

// ProgressDone clears the progress line so the next message starts clean. A
// write that fails is recorded like a failed status line.
func (cc *CLIContext) ProgressDone() {
	if cc.StatusWriter == nil {
		return
	}

	cc.recordStatusError(cc.clearProgress())
}

// clearProgress clears a progress line on screen under statusMu and returns
// the write's error for the caller to record once the mutex is released. The
// line counts as cleared either way, so the next write does not try again.
func (cc *CLIContext) clearProgress() error {
	cc.statusMu.Lock()
	defer cc.statusMu.Unlock()

	if !cc.progressShown {
		return nil
	}
	cc.progressShown = false
	if _, err := io.WriteString(cc.StatusWriter, clearLine); err != nil {
		return fmt.Errorf("clear progress line: %w", err)
	}

	return nil
}
