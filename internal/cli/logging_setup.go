// The command logger's construction: console and file handlers, level
// resolution, and swapping the logger once the config is loaded. The writers
// it logs to come from root.go, which owns the process's stdout and stderr.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/logfile"
	"github.com/tonimelisma/onedrive-go/internal/loglevel"
)

// Log format constants for the log_format config setting.
const (
	logFormatText = "text"
	logFormatJSON = "json"
	logFormatAuto = "auto"
)

// parseLogLevel maps a config log_level string to the corresponding
// slog.Level. Returns (level, true) for recognized values, (0, false)
// otherwise. Uses a switch instead of a map to avoid package-level
// mutable state — only 4 entries, so a switch is clearer and faster.
func parseLogLevel(s string) (slog.Level, bool) {
	switch s {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return 0, false
	}
}

func (cc *CLIContext) replaceCommandLogger(logger *slog.Logger, closer io.Closer) error {
	if cc == nil {
		if closer != nil {
			if closeErr := closer.Close(); closeErr != nil {
				return fmt.Errorf("close replacement command logger: %w", closeErr)
			}
		}

		return nil
	}

	if err := cc.closeCommandLogger(); err != nil {
		if closer != nil {
			if closeErr := closer.Close(); closeErr != nil {
				return errors.Join(err, fmt.Errorf("close replacement command logger: %w", closeErr))
			}
		}

		return err
	}

	if logger != nil {
		cc.Logger = logger
	}
	cc.logCloser = closer
	if cc.PerfSession != nil && cc.Logger != nil {
		cc.PerfSession.SetLogger(cc.Logger)
	}

	return nil
}

// useLoggingConfig builds the command logger from logging: the console half
// follows the CLI flags, the log-file half follows logging. Called again with
// the settings it already uses, it keeps the current logger, so a command that
// re-reads its config does not reopen the log file or repeat its warnings.
func (cc *CLIContext) useLoggingConfig(logging config.LoggingConfig) error {
	if cc.hasLogging && cc.logging == logging {
		return nil
	}

	drive := &config.ResolvedDrive{LoggingConfig: logging}
	logger, closer, openErr := buildLoggerDualWithStatusWriter(drive, cc.Flags, cc.Status())
	cc.warnAboutLogging(drive, closer != nil, openErr)
	if err := cc.replaceCommandLogger(logger, closer); err != nil {
		return err
	}

	cc.fileLogLevel = nil
	if handle, ok := closer.(*logFileHandle); ok {
		cc.fileLogLevel = handle.level
	}
	cc.logging = logging
	cc.hasLogging = true

	return nil
}

// warnAboutLogging tells the user which logging settings the command logger
// built from drive could not follow: a log file it could not open (openErr;
// the console logs alone), an unknown log_format (the console uses auto) and,
// when the log file is open, an unknown log_level (the file logs at info).
func (cc *CLIContext) warnAboutLogging(drive *config.ResolvedDrive, logFileOpen bool, openErr error) {
	if openErr != nil {
		cc.warnf("Warning: cannot open log file %q: %v\n", drive.LogFile, openErr)
	}
	if _, known := consoleLogFormat(drive); !known {
		cc.warnf("Warning: unknown log_format %q in the config; using auto.\n", drive.LogFormat)
	}
	if _, known := parseLogLevel(drive.LogLevel); logFileOpen && drive.LogLevel != "" && !known {
		cc.warnf("Warning: unknown log_level %q in the config; logging to the log file at info.\n", drive.LogLevel)
	}
}

// applyFileLogLevel moves the log file's level to the config's log_level,
// which a running `sync --watch` reads again on every config reload
// (R-2.8.1). An unknown value keeps the current level and is reported once
// per value, the way the startup warning reports it.
func (cc *CLIContext) applyFileLogLevel(level string) {
	if cc == nil || cc.fileLogLevel == nil {
		return
	}
	if level == "" {
		cc.fileLogLevel.Set(slog.LevelInfo)
		return
	}
	mapped, ok := parseLogLevel(level)
	if !ok {
		if cc.Logger != nil && cc.lastBadLogLevel != level {
			cc.lastBadLogLevel = level
			cc.Logger.Warn("unknown log_level in the config; keeping the log file's current level",
				slog.String("log_level", level),
			)
		}
		return
	}
	cc.fileLogLevel.Set(mapped)
}

// applyLogLevelFunc returns the command's log-level setter for the watch
// orchestrator to call after a reload, or nil outside a CLI context.
func applyLogLevelFunc(ctx context.Context) func(string) {
	cc := cliContextFrom(ctx)
	if cc == nil {
		return nil
	}

	return cc.applyFileLogLevel
}

// logFileHandle is the log file plus the level variable its handler reads,
// so a reload can move the level without reopening the file.
type logFileHandle struct {
	io.Closer
	level *slog.LevelVar
}

// attachConfiguredLogging gives the command the log file the config asks for
// before any command work runs. Commands that skip the strict config load
// (auth, drive, status, sync, shared targets) used to keep the flag-only
// bootstrap logger, so their INFO records -- the performance summary above
// all -- never reached the log file. It is best effort: a config that cannot
// be read leaves the console-only logger in place, and the command reports the
// problem itself if it needs the config.
func (cc *CLIContext) attachConfiguredLogging() error {
	rawCfg, _, err := config.LoadOrDefaultLenient(cc.CfgPath, slog.New(slog.DiscardHandler))
	if err != nil {
		cc.Logger.Debug("log file not attached: config unreadable", slog.Any("error", err))

		return nil
	}

	return cc.useLoggingConfig(rawCfg.LoggingConfig)
}

func (cc *CLIContext) closeCommandLogger() error {
	if cc == nil || cc.logCloser == nil {
		return nil
	}

	closer := cc.logCloser
	cc.logCloser = nil
	if err := closer.Close(); err != nil {
		return fmt.Errorf("close command logger: %w", err)
	}

	return nil
}

// buildLoggerWithStatusWriter creates an slog.Logger configured by CLI flags
// for console output on statusWriter (stderr when nil). Configured log_level
// applies only to file logging so ordinary command output stays quiet unless
// the user asks for verbosity with --verbose or --debug.
func buildLoggerWithStatusWriter(cfg *config.ResolvedDrive, flags CLIFlags, statusWriter io.Writer) *slog.Logger {
	level := resolveConsoleLogLevel(flags)
	opts := &slog.HandlerOptions{Level: level}

	return slog.New(buildHandlerWithStatusWriter(statusWriterOrDefault(statusWriter), cfg, opts))
}

// resolveConsoleLogLevel sets the console half of the command logger. The
// console and the log file have separate levels by design (R-6.6.2, R-6.6.3):
// INFO records are operational history -- performance summaries (R-6.6.14),
// plan counts, runner lifecycle -- that the log file keeps at its default
// level, while the default console starts at NOTICE and shows them only with
// --verbose/--debug. NOTICE is the all-clear after a warning (R-6.6.30), so a
// console that showed the warning also shows its recovery. What a person must
// see at default verbosity (activity, summaries, startup lines) is written by
// the command itself, not logged.
func resolveConsoleLogLevel(flags CLIFlags) slog.Level {
	level := loglevel.Notice

	if flags.Verbose {
		level = slog.LevelInfo
	}

	if flags.Debug {
		level = slog.LevelDebug
	}

	if flags.Quiet {
		level = slog.LevelError
	}

	return level
}

// resolveFileLogLevel is the log file's level: the config's log_level, or
// info when it is empty or names no level (useLoggingConfig warns about it).
func resolveFileLogLevel(cfg *config.ResolvedDrive) slog.Level {
	if cfg != nil && cfg.LogLevel != "" {
		if mapped, ok := parseLogLevel(cfg.LogLevel); ok {
			return mapped
		}
	}

	return slog.LevelInfo
}

// buildHandlerWithStatusWriter creates the console's slog.Handler on w, the
// status stream (typically os.Stderr), in the format consoleLogFormat reads
// from cfg, so the bootstrap logger (a nil cfg) and the configured logger
// render the same way.
func buildHandlerWithStatusWriter(w io.Writer, cfg *config.ResolvedDrive, opts *slog.HandlerOptions) slog.Handler {
	format, _ := consoleLogFormat(cfg)
	switch format {
	case logFormatJSON:
		return slog.NewJSONHandler(w, withLevelNames(opts))
	case logFormatText:
		return slog.NewTextHandler(w, withLevelNames(opts))
	default:
		return autoConsoleHandler(w, opts)
	}
}

// consoleLogFormat is the console format cfg's log_format names: json, text
// or auto, with auto for a nil config or an empty value. known is false for a
// value that names no format, which means auto too (useLoggingConfig warns
// about it).
func consoleLogFormat(cfg *config.ResolvedDrive) (format string, known bool) {
	if cfg == nil || cfg.LogFormat == "" {
		return logFormatAuto, true
	}

	switch cfg.LogFormat {
	case logFormatJSON, logFormatText, logFormatAuto:
		return cfg.LogFormat, true
	default:
		return logFormatAuto, false
	}
}

// autoConsoleHandler picks the console log style for the default "auto"
// format. The console is for people, so it is never JSON here (structured
// history belongs in log_file): the journal gets priority-prefixed lines
// without timestamps, default verbosity gets plain "Warning: ..." lines, and
// --verbose/--debug get full timestamped key=value detail. At default
// verbosity both styles also drop the sync log schema identifiers
// (isInternalLogField); the log file and --verbose/--debug keep them.
func autoConsoleHandler(w io.Writer, opts *slog.HandlerOptions) slog.Handler {
	if isWriterJournal(w) {
		journal := newConsoleLogHandler(w, opts.Level, consoleLogJournal)
		journal.showInternalFields = opts.Level != nil && journalShowsInternalFields(opts.Level.Level())

		return journal
	}

	if opts.Level != nil && opts.Level.Level() >= loglevel.Notice {
		return newConsoleLogHandler(w, opts.Level, consoleLogHuman)
	}

	return slog.NewTextHandler(w, withLevelNames(opts))
}

// withLevelNames returns opts with NOTICE printed by name instead of slog's
// "INFO+2". The caller's options are not modified.
func withLevelNames(opts *slog.HandlerOptions) *slog.HandlerOptions {
	named := slog.HandlerOptions{ReplaceAttr: loglevel.ReplaceLevelName}
	if opts != nil {
		named = *opts
		if named.ReplaceAttr == nil {
			named.ReplaceAttr = loglevel.ReplaceLevelName
		}
	}

	return &named
}

// journalShowsInternalFields reports whether journal lines at consoleLevel
// keep the sync log schema fields: only --verbose/--debug (below WARN) do.
func journalShowsInternalFields(consoleLevel slog.Level) bool {
	return consoleLevel < loglevel.Notice
}

// isWriterJournal reports whether w is a file that systemd connected to the
// journal.
func isWriterJournal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}

	return fileIsJournalStream(f, os.Getenv("JOURNAL_STREAM"))
}

// fdProvider is implemented by types that expose a file descriptor (e.g., *os.File).
type fdProvider interface {
	Fd() uintptr
}

// isWriterTTY checks whether w is a terminal. Returns false for non-file
// writers (buffers, pipes, log files), ensuring "auto" format selects JSON
// when output is redirected.
func isWriterTTY(w io.Writer) bool {
	f, ok := w.(fdProvider)
	if !ok {
		return false
	}

	return isatty.IsTerminal(f.Fd())
}

// multiHandler fans out log records to multiple slog.Handler instances.
// Each handler independently filters by level, so a console handler at Error
// and a file handler at Debug can coexist without the console being flooded.
type multiHandler struct {
	handlers []slog.Handler
}

// Enabled returns true if any handler accepts the level (OR semantics).
func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}

	return false
}

// Handle dispatches the record to every handler that accepts its level.
//
// One handler failing must not deprive the others of the record. The console
// and file handlers exist precisely because they fail independently: a
// terminal that goes away when output is piped into a command that exits
// early, or a log file on a full disk. Returning at the first error would let
// a broken console silently stop file logging, which is the half most worth
// keeping.
//
//nolint:gocritic // slog.Handler interface requires value receiver for Record
func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error

	for _, h := range m.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}

		if err := h.Handle(ctx, r); err != nil {
			errs = append(errs, fmt.Errorf("handle log record: %w", err))
		}
	}

	return errors.Join(errs...)
}

// WithAttrs returns a new multiHandler where each inner handler has the attrs.
func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithAttrs(attrs)
	}

	return &multiHandler{handlers: handlers}
}

// withTimestamps returns a new multiHandler whose human console handlers
// (consoleTimestamper) stamp each line with the time of day now returns; the
// other handlers are kept as they are.
func (m *multiHandler) withTimestamps(now func() time.Time) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h
		if stamper, ok := h.(consoleTimestamper); ok {
			handlers[i] = stamper.withTimestamps(now)
		}
	}

	return &multiHandler{handlers: handlers}
}

// WithGroup returns a new multiHandler where each inner handler has the group.
func (m *multiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithGroup(name)
	}

	return &multiHandler{handlers: handlers}
}

// buildLoggerDualWithStatusWriter creates an slog.Logger with optional dual
// output. When cfg.LogFile is set, logs go to both statusWriter (stderr when
// nil; console format, CLI-flag-driven level) and the file (JSON format,
// config log_level-driven level). Returns an io.Closer for the log file (nil
// when no file is used) and, when the file could not be opened, why: the
// logger then writes to the console alone. It writes no warning itself; the
// command context does (useLoggingConfig).
func buildLoggerDualWithStatusWriter(
	cfg *config.ResolvedDrive, flags CLIFlags, statusWriter io.Writer,
) (*slog.Logger, io.Closer, error) {
	if cfg == nil || cfg.LogFile == "" {
		return buildLoggerWithStatusWriter(cfg, flags, statusWriter), nil, nil
	}

	// Retention runs before the open, so an expired current log is removed and
	// the open starts it afresh. What it could not do is reported once, through
	// the logger built here; it never keeps the log from opening.
	retentionErr := logfile.RemoveExpired(cfg.LogFile, cfg.LogRetentionDays)
	logger, closer, openErr := buildFileLogger(cfg, flags, statusWriter)
	if retentionErr != nil {
		logger.Warn("could not remove expired log files",
			slog.String("log_file", cfg.LogFile),
			slog.Any("error", retentionErr),
		)
	}

	return logger, closer, openErr
}

// buildFileLogger opens cfg.LogFile and builds the dual console and file
// logger on it, falling back to the console alone, and returning why, when
// the file cannot be opened.
func buildFileLogger(cfg *config.ResolvedDrive, flags CLIFlags, statusWriter io.Writer) (*slog.Logger, io.Closer, error) {
	f, err := logfile.Open(cfg.LogFile)
	if err != nil {
		// The warning names the log file and prints this as its reason.
		return buildLoggerWithStatusWriter(cfg, flags, statusWriter), nil, err
	}

	// Console handler: CLI-flag-driven level, config-driven format.
	consoleLevel := resolveConsoleLogLevel(flags)
	consoleHandler := buildHandlerWithStatusWriter(
		statusWriterOrDefault(statusWriter),
		cfg,
		&slog.HandlerOptions{Level: consoleLevel},
	)

	// File handler: config-driven level, always JSON for machine parsing.
	// The level is a variable so a config reload can move it (R-2.8.1).
	fileLevel := new(slog.LevelVar)
	fileLevel.Set(resolveFileLogLevel(cfg))
	fileHandler := slog.NewJSONHandler(f, &slog.HandlerOptions{Level: fileLevel, ReplaceAttr: loglevel.ReplaceLevelName})

	return slog.New(&multiHandler{handlers: []slog.Handler{consoleHandler, fileHandler}}), &logFileHandle{Closer: f, level: fileLevel}, nil
}
