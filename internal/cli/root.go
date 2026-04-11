package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/failures"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/graphhttp"
	"github.com/tonimelisma/onedrive-go/internal/logfile"
	"github.com/tonimelisma/onedrive-go/internal/perf"
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
const (
	skipConfigAnnotation = "skipConfig"
	skipConfigValue      = "true"
)

const (
	commandPerfUpdateInterval      = 30 * time.Second
	watchCommandPerfUpdateInterval = 5 * time.Minute
)

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

type rootFlagBindings struct {
	configPath string
	account    string
	drive      []string
	json       bool
	verbose    bool
	debug      bool
	quiet      bool
}

func (b rootFlagBindings) cliFlags() CLIFlags {
	return CLIFlags{
		ConfigPath: b.configPath,
		Account:    b.account,
		Drive:      b.drive,
		JSON:       b.json,
		Verbose:    b.verbose,
		Debug:      b.debug,
		Quiet:      b.quiet,
	}
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
	Flags                         CLIFlags
	Logger                        *slog.Logger
	OutputWriter                  io.Writer                     // destination for primary command output (default: os.Stdout)
	StatusWriter                  io.Writer                     // destination for Statusf output (default: os.Stderr)
	CfgPath                       string                        // resolved config file path (always set)
	Env                           config.EnvOverrides           // env overrides (always set in Phase 1)
	GraphBaseURL                  string                        // internal test seam for live Graph command coverage
	SharedTarget                  *sharedTarget                 // nil for ordinary drive/path commands
	HTTPProvider                  *graphhttp.Provider           // Graph-facing HTTP runtime policy
	Cfg                           *config.ResolvedDrive         // nil for auth/account commands
	Provider                      *driveops.SessionProvider     // nil for auth/account commands; created in Phase 2
	PerfSession                   *perf.Session                 // command-scoped performance collector/logging session
	syncWatchRunner               syncWatchRunner               // test-only seam for sync --watch command coverage
	syncDaemonOrchestratorFactory syncDaemonOrchestratorFactory // test-only seam below syncWatchRunner for real daemon-path coverage
	logCloser                     io.Closer                     // log file closer; nil when no log file is configured
	statusMu                      sync.Mutex                    // guards statusErr for concurrent progress callbacks
	statusErr                     error
	reconcileMu                   sync.Mutex // guards reconcileNotices and selector mutation
	reconcileNotices              map[string]struct{}
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

func (cc *CLIContext) httpProvider() *graphhttp.Provider {
	if cc == nil {
		return graphhttp.NewProvider(slog.Default())
	}

	if cc.HTTPProvider == nil {
		cc.HTTPProvider = graphhttp.NewProvider(cc.Logger)
	}

	return cc.HTTPProvider
}

func (cc *CLIContext) interactiveHTTPClients(rd *config.ResolvedDrive) driveops.HTTPClients {
	provider := cc.httpProvider()
	if rd == nil {
		syncClients := provider.Sync()

		return driveops.HTTPClients{
			Meta:     provider.BootstrapMeta(),
			Transfer: syncClients.Transfer,
		}
	}

	account := rd.CanonicalID.Email()
	var clients graphhttp.ClientSet
	if rd.RootItemID != "" {
		clients = provider.InteractiveForSharedTarget(account, rd.DriveID.String(), rd.RootItemID)
	} else {
		clients = provider.InteractiveForDrive(account, rd.DriveID)
	}

	return driveops.HTTPClients{
		Meta:     clients.Meta,
		Transfer: clients.Transfer,
	}
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
	if cc.Provider != nil && cc.GraphBaseURL != "" {
		cc.Provider.GraphBaseURL = cc.GraphBaseURL
	}

	accountCID, err := config.TokenAccountCanonicalID(cc.Cfg.CanonicalID)
	if err != nil {
		return nil, fmt.Errorf("resolve token owner for drive %s: %w", cc.Cfg.CanonicalID, err)
	}
	if !accountCID.IsZero() {
		result, probeErr := cc.probeAccountIdentity(ctx, accountCID, "drive-session")
		if probeErr != nil {
			return nil, fmt.Errorf("probe account identity: %w", probeErr)
		}
		if result.Changed() {
			cc.Provider.FlushTokenCache()
			if reloadErr := cc.reloadResolvedDriveFromFlags(); reloadErr != nil {
				return nil, reloadErr
			}
		}
	}

	session, err := cc.Provider.Session(ctx, cc.Cfg)
	if err != nil {
		return nil, fmt.Errorf("create drive session: %w", err)
	}

	// Successful authenticated Graph calls prove the saved login still works.
	// Attach a best-effort hook so live CLI commands clear stale auth:account
	// scope blocks without treating session construction itself as proof.
	attachDriveAuthProof(session, newAuthProofRecorder(cc.Logger), "drive-session")

	return session, nil
}

// newGraphClient creates a graph.Client with the standard HTTP client,
// user-agent, and base URL. Eliminates boilerplate repeated across commands.
func newGraphClient(ts graph.TokenSource, logger *slog.Logger) (*graph.Client, error) {
	return newGraphClientWithHTTP("", graphhttp.NewProvider(logger).BootstrapMeta(), ts, logger)
}

func newGraphClientWithHTTP(
	baseURL string,
	httpClient *http.Client,
	ts graph.TokenSource,
	logger *slog.Logger,
) (*graph.Client, error) {
	if baseURL == "" {
		baseURL = graph.DefaultBaseURL
	}

	client, err := graph.NewClient(baseURL, httpClient, ts, logger, "onedrive-go/"+version)
	if err != nil {
		return nil, fmt.Errorf("creating graph client: %w", err)
	}

	return client, nil
}

// newRootCmd builds and returns the fully-assembled root command with all
// subcommands registered. Called once from main().
func newRootCmd() *cobra.Command {
	return newRootCmdWithWriters(nil, nil)
}

func initializeCLIContext(
	cmd *cobra.Command,
	args []string,
	bindings rootFlagBindings,
	outputWriter io.Writer,
	statusWriter io.Writer,
) error {
	if version == "dev" {
		config.AssertDevSafe() // prevent go run . from touching production data
	}

	flags := bindings.cliFlags()
	logger := buildLoggerWithStatusWriter(nil, flags, statusWriter)
	env := config.ReadEnvOverrides(logger)
	cc := &CLIContext{
		OutputWriter: outputWriter,
		Flags:        flags,
		Logger:       logger,
		StatusWriter: statusWriter,
		CfgPath:      config.ResolveConfigPath(env, config.CLIOverrides{ConfigPath: flags.ConfigPath}, logger),
		Env:          env,
		HTTPProvider: graphhttp.NewProvider(logger),
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	perfSession, perfCtx := newCommandPerfSession(ctx, cmd, logger)
	cc.PerfSession = perfSession
	if perfCtx != nil {
		ctx = perfCtx
	}
	ctx = context.WithValue(ctx, cliContextKey{}, cc)
	cmd.SetContext(ctx)
	root := cmd.Root()
	if root != nil && root != cmd {
		root.SetContext(ctx)
	}

	sharedTarget, found, err := resolveSharedTargetBootstrap(ctx, cmd, args, cc)
	if err != nil {
		return err
	}
	if found {
		cc.SharedTarget = sharedTarget
	}

	if cc.SharedTarget == nil && cmd.Annotations[skipConfigAnnotation] != skipConfigValue {
		if err := initializeResolvedCLIContext(cmd, cc); err != nil {
			return err
		}
	}

	return nil
}

func initializeResolvedCLIContext(cmd *cobra.Command, cc *CLIContext) error {
	resolved, rawCfg, err := loadAndResolve(cmd, cc.Flags, cc.Env, cc.Logger)
	if err != nil {
		return err
	}

	cc.Cfg = resolved

	dualLogger, closer := buildLoggerDualWithStatusWriter(resolved, cc.Flags, cc.StatusWriter)
	if err := cc.replaceCommandLogger(dualLogger, closer); err != nil {
		return err
	}
	cc.HTTPProvider = graphhttp.NewProvider(cc.Logger)

	holder := config.NewHolder(rawCfg, cc.CfgPath)
	cc.Provider = driveops.NewSessionProvider(
		holder,
		cc.interactiveHTTPClients,
		"onedrive-go/"+version,
		cc.Logger,
	)

	return nil
}

func bindRootFlags(cmd *cobra.Command, bindings *rootFlagBindings) {
	cmd.PersistentFlags().StringVar(&bindings.configPath, "config", "", "config file path")
	cmd.PersistentFlags().StringVar(&bindings.account, "account", "", "account for auth commands (e.g., user@example.com)")
	cmd.PersistentFlags().StringArrayVar(&bindings.drive, "drive", nil,
		"drive selector (canonical ID, display name, or partial match); repeatable for sync")
	cmd.PersistentFlags().BoolVar(&bindings.json, "json", false, "output in JSON format")
	cmd.PersistentFlags().BoolVarP(&bindings.verbose, "verbose", "v", false, "show detailed output")
	cmd.PersistentFlags().BoolVar(&bindings.debug, "debug", false, "enable debug logging (HTTP requests, config resolution)")
	cmd.PersistentFlags().BoolVarP(&bindings.quiet, "quiet", "q", false, "suppress informational output")
	cmd.MarkFlagsMutuallyExclusive("verbose", "debug", "quiet")
}

func newRootCmdWithWriters(outputWriter, statusWriter io.Writer) *cobra.Command {
	outputWriter = outputWriterOrDefault(outputWriter)
	statusWriter = statusWriterOrDefault(statusWriter)

	var bindings rootFlagBindings

	cmd := &cobra.Command{
		Use:           "onedrive-go",
		Short:         "OneDrive CLI client",
		Long:          "A fast, safe OneDrive CLI and sync client for Linux and macOS.",
		Version:       version,
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return initializeCLIContext(cmd, args, bindings, outputWriter, statusWriter)
		},
	}

	bindRootFlags(cmd, &bindings)

	addRootSubcommands(cmd)

	return cmd
}

func addRootSubcommands(cmd *cobra.Command) {
	cmd.AddCommand(
		newLoginCmd(), newLogoutCmd(), newWhoamiCmd(), newStatusCmd(),
		newPerfCmd(),
		newSharedCmd(),
		newDriveCmd(), newLsCmd(), newGetCmd(), newPutCmd(),
		newRmCmd(), newMkdirCmd(), newStatCmd(), newSyncCmd(),
		newPauseCmd(), newResumeCmd(), newResolveCmd(), newRecoverCmd(),
		newMvCmd(), newCpCmd(),
		newRecycleBinCmd(),
	)
}

// Main executes the CLI with the provided args and returns the desired
// process exit code. Keeping exit behavior here leaves the root package as
// a thin entrypoint while the command layer owns user-facing presentation.
func Main(args []string) int {
	return mainWithWriters(args, nil, nil)
}

func closeRootCommandLogger(cmd *cobra.Command) error {
	if cmd == nil {
		return nil
	}

	root := cmd.Root()
	if root == nil {
		root = cmd
	}

	cc := cliContextFrom(root.Context())
	if cc == nil {
		return nil
	}

	return cc.closeCommandLogger()
}

func mainWithWriters(args []string, outputWriter, statusWriter io.Writer) int {
	cmd := newRootCmdWithWriters(outputWriter, statusWriter)
	cmd.SetArgs(args)

	err := cmd.Execute()
	completeRootCommandPerf(cmd, err)
	closeErr := closeRootCommandLogger(cmd)
	if closeErr != nil {
		if err == nil {
			err = closeErr
		} else {
			writeWarningf(statusWriter, "warning: close command log: %v\n", closeErr)
		}
	}

	if err != nil {
		class := classifyCommandError(err)
		presentation := commandFailurePresentationForClass(class)
		if class != failures.ClassShutdown {
			if authMessage := authErrorMessage(err); authMessage != "" {
				writeWarningf(statusWriter, "%s\n", authMessage)
			} else {
				writeWarningf(statusWriter, "Error: %v\n", err)
			}
		}

		return presentation.ExitCode
	}

	return 0
}

func newCommandPerfSession(
	ctx context.Context,
	cmd *cobra.Command,
	logger *slog.Logger,
) (*perf.Session, context.Context) {
	if cmd == nil {
		return nil, ctx
	}

	interval := commandPerfUpdateInterval
	if cmd.Name() == "sync" {
		if watch, err := cmd.Flags().GetBool("watch"); err == nil && watch {
			interval = watchCommandPerfUpdateInterval
		}
	}

	return perf.NewSession(ctx, logger, "command", cmd.CommandPath(), interval, nil)
}

func completeRootCommandPerf(cmd *cobra.Command, err error) {
	if cmd == nil {
		return
	}

	root := cmd.Root()
	if root == nil {
		root = cmd
	}

	cc := cliContextFrom(root.Context())
	if cc == nil || cc.PerfSession == nil {
		return
	}

	cc.PerfSession.Complete(err)
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
	return buildLoggerWithStatusWriter(cfg, flags, nil)
}

func buildLoggerWithStatusWriter(cfg *config.ResolvedDrive, flags CLIFlags, statusWriter io.Writer) *slog.Logger {
	level := resolveLogLevelWithStatusWriter(cfg, flags, statusWriter)
	opts := &slog.HandlerOptions{Level: level}

	return slog.New(buildHandlerWithStatusWriter(statusWriterOrDefault(statusWriter), cfg, opts, statusWriter))
}

// resolveLogLevel determines the effective slog.Level from config and CLI flags.
func resolveLogLevel(cfg *config.ResolvedDrive, flags CLIFlags) slog.Level {
	return resolveLogLevelWithStatusWriter(cfg, flags, nil)
}

func resolveLogLevelWithStatusWriter(cfg *config.ResolvedDrive, flags CLIFlags, statusWriter io.Writer) slog.Level {
	level := slog.LevelWarn

	// Config-based log level (lower priority than CLI flags).
	if cfg != nil && cfg.LogLevel != "" {
		if mapped, ok := parseLogLevel(cfg.LogLevel); ok {
			level = mapped
		} else {
			writeWarningf(statusWriter, "warning: unknown log level %q, using warn\n", cfg.LogLevel)
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
	return buildHandlerWithStatusWriter(w, cfg, opts, w)
}

func buildHandlerWithStatusWriter(
	w io.Writer,
	cfg *config.ResolvedDrive,
	opts *slog.HandlerOptions,
	statusWriter io.Writer,
) slog.Handler {
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
		writeWarningf(statusWriter, "warning: unknown log format %q, using text\n", format)

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
	return buildLoggerDualWithStatusWriter(cfg, flags, nil)
}

func buildLoggerDualWithStatusWriter(
	cfg *config.ResolvedDrive, flags CLIFlags, statusWriter io.Writer,
) (*slog.Logger, io.Closer) {
	if cfg == nil || cfg.LogFile == "" {
		return buildLoggerWithStatusWriter(cfg, flags, statusWriter), nil
	}

	f, err := logfile.Open(cfg.LogFile, cfg.LogRetentionDays)
	if err != nil {
		writeWarningf(statusWriter, "warning: cannot open log file %q: %v\n", cfg.LogFile, err)

		return buildLoggerWithStatusWriter(cfg, flags, statusWriter), nil
	}

	// Console handler: CLI-flag-driven level, config-driven format.
	consoleLevel := resolveLogLevelWithStatusWriter(cfg, flags, statusWriter)
	consoleHandler := buildHandlerWithStatusWriter(
		statusWriterOrDefault(statusWriter),
		cfg,
		&slog.HandlerOptions{Level: consoleLevel},
		statusWriter,
	)

	// File handler: config-driven level, always JSON for machine parsing.
	fileLevel := resolveLogLevelWithStatusWriter(cfg, CLIFlags{}, statusWriter)
	fileHandler := slog.NewJSONHandler(f, &slog.HandlerOptions{Level: fileLevel})

	return slog.New(&multiHandler{handlers: []slog.Handler{consoleHandler, fileHandler}}), f
}
