package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/logfile"
)

// version is set at build time via ldflags.
var version = "dev"

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

// skipConfigAnnotation marks commands that handle config loading themselves.
// Commands annotated with this key skip the Phase 2 config resolution in
// PersistentPreRunE but still get Phase 1 (flags + bootstrap logger).
const skipConfigAnnotation = "skipConfig"

// CLIFlags contains parsed CLI flag values. Populated in Phase 1 of
// PersistentPreRunE from Cobra flag bindings. All commands (including auth)
// access flags through this struct — no global mutable state.
type CLIFlags struct {
	ConfigPath string
	Account    string
	Drive      []string
	JSON       bool
	Verbose    bool
	Debug      bool
	Quiet      bool
}

// SingleDrive returns the single --drive selector, or "" if none was provided.
// Returns an error if multiple --drive values were provided — callers that
// allow multiple values should use Drive directly.
func (f CLIFlags) SingleDrive() (string, error) {
	if len(f.Drive) == 0 {
		return "", nil
	}

	if len(f.Drive) > 1 {
		return "", fmt.Errorf("multiple --drive values not allowed for this command (use 'sync' for multi-drive)")
	}

	return f.Drive[0], nil
}

// CLIContext bundles resolved config, flags, and logger. Created in
// PersistentPreRunE with two-phase initialization:
//   - Phase 1 (always): Flags + Logger + CfgPath + Env populated for every command.
//   - Phase 2 (data commands): Cfg + Provider populated after config resolution.
//
// Auth commands get CLIContext with Flags + Logger + CfgPath + Env but nil Cfg/Provider.
type CLIContext struct {
	Flags        CLIFlags
	Logger       *slog.Logger
	StatusWriter io.Writer                 // destination for Statusf output (default: os.Stderr)
	CfgPath      string                    // resolved config file path (always set)
	Env          config.EnvOverrides       // env overrides (always set in Phase 1)
	Cfg          *config.ResolvedDrive     // nil for auth/account commands
	Provider     *driveops.SessionProvider // nil for auth/account commands; created in Phase 2
	logCloser    io.Closer                 // log file closer; nil when no log file is configured
	statusMu     sync.Mutex                // guards statusErr for concurrent progress callbacks
	statusErr    error
}

// cliContextKey is the context key for CLIContext.
type cliContextKey struct{}

// cliContextFrom extracts the CLIContext from the command's context.
// Returns nil if PersistentPreRunE hasn't run yet.
func cliContextFrom(ctx context.Context) *CLIContext {
	cc, ok := ctx.Value(cliContextKey{}).(*CLIContext)
	if !ok {
		return nil
	}

	return cc
}

// mustCLIContext extracts the CLIContext or panics with an actionable message.
// Use in RunE handlers for commands that require config (no skipConfigAnnotation).
// Panics are always programmer errors — the command tree should guarantee the
// context is populated by PersistentPreRunE before RunE executes.
func mustCLIContext(ctx context.Context) *CLIContext {
	cc := cliContextFrom(ctx)
	if cc == nil {
		panic("BUG: CLIContext not found in context — ensure the command " +
			"does not skip config loading (no skipConfigAnnotation) or " +
			"explicitly loads config in its RunE")
	}

	return cc
}

// Session is a shorthand for cc.Provider.Session(ctx, cc.Cfg).
// Eliminates 7 identical boilerplate blocks across file operation commands.
func (cc *CLIContext) Session(ctx context.Context) (*driveops.Session, error) {
	session, err := cc.Provider.Session(ctx, cc.Cfg)
	if err != nil {
		return nil, fmt.Errorf("create drive session: %w", err)
	}

	return session, nil
}

// newGraphClient creates a graph.Client with the standard HTTP client,
// user-agent, and base URL. Eliminates boilerplate repeated across commands.
func newGraphClient(ts graph.TokenSource, logger *slog.Logger) (*graph.Client, error) {
	client, err := graph.NewClient(graph.DefaultBaseURL, defaultHTTPClient(logger), ts, logger, "onedrive-go/"+version)
	if err != nil {
		return nil, fmt.Errorf("creating graph client: %w", err)
	}

	return client, nil
}

// newRootCmd builds and returns the fully-assembled root command with all
// subcommands registered. Called once from main().
func newRootCmd() *cobra.Command {
	// Local flag variables — NOT globals. Cobra binds to these; Phase 1 of
	// PersistentPreRunE copies them into CLIFlags on CLIContext.
	var (
		flagConfigPath string
		flagAccount    string
		flagDrive      []string
		flagJSON       bool
		flagVerbose    bool
		flagDebug      bool
		flagQuiet      bool
	)

	cmd := &cobra.Command{
		Use:     "onedrive-go",
		Short:   "OneDrive CLI client",
		Long:    "A fast, safe OneDrive CLI and sync client for Linux and macOS.",
		Version: version,
		// Silence Cobra's default error/usage printing — we handle it ourselves.
		SilenceErrors: true,
		SilenceUsage:  true,
		// PersistentPreRunE runs in two phases for ALL commands:
		//   Phase 1 (always): read Cobra flags → build CLIFlags → read env → build bootstrap logger
		//   Phase 2 (data commands only): load config → resolve drive → build final logger
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if version == "dev" {
				config.AssertDevSafe() // prevent go run . from touching production data
			}
			// Phase 1: always populate flags + bootstrap logger + env + config path.
			flags := CLIFlags{
				ConfigPath: flagConfigPath,
				Account:    flagAccount,
				Drive:      flagDrive,
				JSON:       flagJSON,
				Verbose:    flagVerbose,
				Debug:      flagDebug,
				Quiet:      flagQuiet,
			}
			logger := buildLogger(nil, flags)
			env := config.ReadEnvOverrides(logger)
			cc := &CLIContext{
				Flags:        flags,
				Logger:       logger,
				StatusWriter: os.Stderr,
				CfgPath:      config.ResolveConfigPath(env, config.CLIOverrides{ConfigPath: flags.ConfigPath}, logger),
				Env:          env,
			}

			// Phase 2: load config for data commands.
			if cmd.Annotations[skipConfigAnnotation] != "true" {
				resolved, rawCfg, err := loadAndResolve(cmd, flags, env, logger)
				if err != nil {
					return err
				}

				cc.Cfg = resolved

				dualLogger, closer := buildLoggerDual(resolved, flags)
				cc.Logger = dualLogger
				cc.logCloser = closer

				holder := config.NewHolder(rawCfg, cc.CfgPath)
				cc.Provider = driveops.NewSessionProvider(holder,
					defaultHTTPClient(cc.Logger), transferHTTPClient(cc.Logger), "onedrive-go/"+version, cc.Logger)
			}

			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			cmd.SetContext(context.WithValue(ctx, cliContextKey{}, cc))

			if cc.Cfg != nil {
				config.WarnUnimplemented(cc.Cfg, cc.Logger)
			}

			return nil
		},
		PersistentPostRunE: func(cmd *cobra.Command, _ []string) error {
			cc := cliContextFrom(cmd.Context())
			if cc != nil && cc.logCloser != nil {
				return cc.logCloser.Close()
			}

			return nil
		},
	}

	cmd.PersistentFlags().StringVar(&flagConfigPath, "config", "", "config file path")
	cmd.PersistentFlags().StringVar(&flagAccount, "account", "", "account for auth commands (e.g., user@example.com)")
	cmd.PersistentFlags().StringArrayVar(&flagDrive, "drive", nil,
		"drive selector (canonical ID, display name, or partial match); repeatable for sync")
	cmd.PersistentFlags().BoolVar(&flagJSON, "json", false, "output in JSON format")
	cmd.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "show detailed output")
	cmd.PersistentFlags().BoolVar(&flagDebug, "debug", false, "enable debug logging (HTTP requests, config resolution)")
	cmd.PersistentFlags().BoolVarP(&flagQuiet, "quiet", "q", false, "suppress informational output")

	cmd.MarkFlagsMutuallyExclusive("verbose", "debug", "quiet")

	// Register subcommands.
	cmd.AddCommand(
		newLoginCmd(), newLogoutCmd(), newWhoamiCmd(), newStatusCmd(),
		newDriveCmd(), newLsCmd(), newGetCmd(), newPutCmd(),
		newRmCmd(), newMkdirCmd(), newStatCmd(), newSyncCmd(),
		newPauseCmd(), newResumeCmd(), newIssuesCmd(),
		newVerifyCmd(), newMvCmd(), newCpCmd(),
		newRecycleBinCmd(),
	)

	return cmd
}

// loadAndResolve resolves the effective configuration from the four-layer
// override chain. Returns the resolved drive config and the raw parsed config
// (needed by SessionProvider for shared drive token resolution).
func loadAndResolve(
	cmd *cobra.Command, flags CLIFlags, env config.EnvOverrides, logger *slog.Logger,
) (*config.ResolvedDrive, *config.Config, error) {
	cli := config.CLIOverrides{
		ConfigPath: flags.ConfigPath,
	}

	// Only pass --drive to the resolver if the user explicitly set it.
	// Phase 2 resolves a single drive; multiple --drive values are only
	// valid for the sync command (which handles resolution itself).
	if cmd.Flags().Changed("drive") {
		if len(flags.Drive) > 1 {
			return nil, nil, fmt.Errorf("--drive can only be specified once for this command (use 'sync' for multi-drive)")
		}

		if len(flags.Drive) == 1 {
			cli.Drive = flags.Drive[0]
		}
	}

	logger.Debug("resolving config",
		slog.String("config_path", cli.ConfigPath),
		slog.String("cli_drive", cli.Drive),
		slog.String("env_config", env.ConfigPath),
		slog.String("env_drive", env.Drive),
	)

	resolved, rawCfg, err := config.ResolveDrive(env, cli, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve drive config: %w", err)
	}

	logger.Debug("config resolved",
		slog.String("canonical_id", resolved.CanonicalID.String()),
		slog.String("sync_dir", resolved.SyncDir),
		slog.String("drive_id", resolved.DriveID.String()),
	)

	return resolved, rawCfg, nil
}

// buildLogger creates an slog.Logger configured by the resolved config and
// CLI flags. Pass nil for pre-config bootstrap (no config-file log level).
// Config-file log level provides the baseline; --verbose, --debug, and --quiet
// override it because CLI flags always win. The flags are mutually exclusive
// (enforced by Cobra).
func buildLogger(cfg *config.ResolvedDrive, flags CLIFlags) *slog.Logger {
	level := resolveLogLevel(cfg, flags)
	opts := &slog.HandlerOptions{Level: level}

	return slog.New(buildHandler(os.Stderr, cfg, opts))
}

// resolveLogLevel determines the effective slog.Level from config and CLI flags.
func resolveLogLevel(cfg *config.ResolvedDrive, flags CLIFlags) slog.Level {
	level := slog.LevelWarn

	// Config-based log level (lower priority than CLI flags).
	if cfg != nil && cfg.LogLevel != "" {
		if mapped, ok := parseLogLevel(cfg.LogLevel); ok {
			level = mapped
		} else {
			fmt.Fprintf(os.Stderr, "warning: unknown log level %q, using warn\n", cfg.LogLevel)
		}
	}

	// CLI flags override config (highest priority).
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

// buildHandler creates the appropriate slog.Handler based on the log_format
// config setting. Defaults to text for bootstrap (nil cfg) or empty format.
// The w parameter specifies the output destination (typically os.Stderr).
// For "auto" format, TTY detection checks w itself (not hardcoded stderr),
// so log_file redirection will correctly select JSON.
func buildHandler(w io.Writer, cfg *config.ResolvedDrive, opts *slog.HandlerOptions) slog.Handler {
	format := logFormatText
	if cfg != nil && cfg.LogFormat != "" {
		format = cfg.LogFormat
	}

	switch format {
	case logFormatJSON:
		return slog.NewJSONHandler(w, opts)
	case logFormatAuto:
		if isWriterTTY(w) {
			return slog.NewTextHandler(w, opts)
		}

		return slog.NewJSONHandler(w, opts)
	case logFormatText:
		return slog.NewTextHandler(w, opts)
	default:
		if err := writef(w, "warning: unknown log format %q, using text\n", format); err != nil {
			fmt.Fprintf(os.Stderr, "warning: unknown log format %q, using text\n", format)
		}

		return slog.NewTextHandler(w, opts)
	}
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

// Handle dispatches the record to each handler that accepts its level.
//
//nolint:gocritic // slog.Handler interface requires value receiver for Record
func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil {
				return fmt.Errorf("handle log record: %w", err)
			}
		}
	}

	return nil
}

// WithAttrs returns a new multiHandler where each inner handler has the attrs.
func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithAttrs(attrs)
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

// buildLoggerDual creates an slog.Logger with optional dual output. When
// cfg.LogFile is set, logs go to both stderr (console format, CLI-driven level)
// and the file (JSON format, config-driven level). Returns an io.Closer for
// the log file (nil when no file is used).
func buildLoggerDual(cfg *config.ResolvedDrive, flags CLIFlags) (*slog.Logger, io.Closer) {
	if cfg == nil || cfg.LogFile == "" {
		return buildLogger(cfg, flags), nil
	}

	f, err := logfile.Open(cfg.LogFile, cfg.LogRetentionDays)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot open log file %q: %v\n", cfg.LogFile, err)

		return buildLogger(cfg, flags), nil
	}

	// Console handler: CLI-flag-driven level, config-driven format.
	consoleLevel := resolveLogLevel(cfg, flags)
	consoleHandler := buildHandler(os.Stderr, cfg, &slog.HandlerOptions{Level: consoleLevel})

	// File handler: config-driven level, always JSON for machine parsing.
	fileLevel := resolveLogLevel(cfg, CLIFlags{})
	fileHandler := slog.NewJSONHandler(f, &slog.HandlerOptions{Level: fileLevel})

	return slog.New(&multiHandler{handlers: []slog.Handler{consoleHandler, fileHandler}}), f
}

// exitOnError prints a user-friendly error message to stderr and exits.
func exitOnError(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}
