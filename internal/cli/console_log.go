package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
)

// consoleLogStyle chooses how log records look on the console. The log file is
// always JSON; this only concerns what a person (or the journal) reads.
type consoleLogStyle int

const (
	// consoleLogHuman is the default-verbosity style: "Warning: <message>"
	// with details in parentheses, no timestamps or level keys.
	consoleLogHuman consoleLogStyle = iota + 1
	// consoleLogJournal writes one "<N>message key=value" line per record so
	// journald files it at the right priority and adds its own timestamp.
	consoleLogJournal
)

// consoleLogHandler renders records as single human-readable lines.
type consoleLogHandler struct {
	w io.Writer
	// mu guards writes to w so concurrent records never interleave; it is
	// shared by every handler derived through WithAttrs/WithGroup.
	mu     *sync.Mutex
	level  slog.Leveler
	style  consoleLogStyle
	attrs  []slog.Attr
	groups []string
	// showInternalFields keeps the sync log schema fields (isInternalLogField)
	// on the line. Only --verbose/--debug set it: at default verbosity those
	// identifiers are noise to a person, and the log file keeps them.
	showInternalFields bool
}

// isInternalLogField reports the R-6.6.13 sync log schema fields other than path:
// identifiers and classifications that make the log file joinable and
// greppable, but mean nothing to the person reading the console. The path
// stays, because it names the file the line is about. condition_total is not a
// schema field but belongs here too: the watch condition summary spells the
// counts out in its message, so the console would only repeat them.
func isInternalLogField(key string) bool {
	switch key {
	case "run_id", "action_id", "action_type", "condition_key", "condition_total", "failure_class",
		"log_owner", "scope_key", "trial_scope_key", "drive_id", "http_status":
		return true
	default:
		return false
	}
}

func newConsoleLogHandler(w io.Writer, level slog.Leveler, style consoleLogStyle) *consoleLogHandler {
	return &consoleLogHandler{w: w, mu: &sync.Mutex{}, level: level, style: style}
}

func (h *consoleLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	minLevel := slog.LevelInfo
	if h.level != nil {
		minLevel = h.level.Level()
	}

	return level >= minLevel
}

//nolint:gocritic // slog.Handler interface requires value receiver for Record
func (h *consoleLogHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder

	switch h.style {
	case consoleLogJournal:
		b.WriteString("<")
		b.WriteString(strconv.Itoa(journalPriorityFor(r.Level)))
		b.WriteString(">")
		b.WriteString(r.Message)
	case consoleLogHuman:
		b.WriteString(humanLevelPrefix(r.Level))
		b.WriteString(r.Message)
	}

	details := make([]string, 0, len(h.attrs)+r.NumAttrs())
	for _, attr := range h.attrs {
		details = h.appendAttr(details, "", attr)
	}
	prefix := strings.Join(h.groups, ".")
	r.Attrs(func(attr slog.Attr) bool {
		details = h.appendAttr(details, prefix, attr)
		return true
	})

	if len(details) > 0 {
		switch h.style {
		case consoleLogJournal:
			b.WriteString(" ")
			b.WriteString(strings.Join(details, " "))
		case consoleLogHuman:
			b.WriteString(" (")
			b.WriteString(strings.Join(details, ", "))
			b.WriteString(")")
		}
	}
	b.WriteString("\n")

	h.mu.Lock()
	defer h.mu.Unlock()

	if _, err := io.WriteString(h.w, b.String()); err != nil {
		return fmt.Errorf("write console log line: %w", err)
	}

	return nil
}

func (h *consoleLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	prefix := strings.Join(h.groups, ".")
	next.attrs = append([]slog.Attr(nil), h.attrs...)
	for _, attr := range attrs {
		if prefix != "" {
			attr.Key = prefix + "." + attr.Key
		}
		next.attrs = append(next.attrs, attr)
	}

	return &next
}

func (h *consoleLogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	next := *h
	next.groups = append(append([]string(nil), h.groups...), name)

	return &next
}

func humanLevelPrefix(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "Error: "
	case level >= slog.LevelWarn:
		return "Warning: "
	default:
		return ""
	}
}

func journalPriorityFor(level slog.Level) int {
	switch {
	case level >= slog.LevelError:
		return journalPriorityErr
	case level >= slog.LevelWarn:
		return journalPriorityWarning
	case level >= slog.LevelInfo:
		return journalPriorityInfo
	default:
		return journalPriorityDebug
	}
}

func (h *consoleLogHandler) appendAttr(details []string, prefix string, attr slog.Attr) []string {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return details
	}
	if !h.showInternalFields && attr.Value.Kind() != slog.KindGroup {
		// WithAttrs stores group-qualified keys, so match the last segment.
		key := attr.Key
		if i := strings.LastIndexByte(key, '.'); i >= 0 {
			key = key[i+1:]
		}
		if isInternalLogField(key) {
			return details
		}
	}

	key := attr.Key
	if prefix != "" {
		key = prefix + "." + key
	}

	if attr.Value.Kind() == slog.KindGroup {
		for _, member := range attr.Value.Group() {
			details = h.appendAttr(details, key, member)
		}
		return details
	}

	if !h.showInternalFields && attr.Value.Kind() == slog.KindAny {
		// An error value is a full chain ("sync drive: observe remote: GET
		// ...: dial tcp ...: connection refused"). A person at default
		// verbosity gets the plain reason (plainErrorReason, as the sync
		// console uses for per-file failures); the chain stays in the log file
		// and under --verbose/--debug.
		if err, ok := attr.Value.Any().(error); ok && err != nil {
			return append(details, plainErrorReason(err))
		}
	}

	value := attr.Value.String()
	if value == "" || strings.ContainsAny(value, " \t\"=") {
		value = strconv.Quote(value)
	}

	return append(details, key+"="+value)
}
