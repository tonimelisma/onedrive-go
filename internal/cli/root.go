package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/perf"
)

// version is set at build time via ldflags.
var version = "dev"

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
		return "", errors.New("multiple --drive values not allowed for this command (use 'sync' for multi-drive)")
	}

	return f.Drive[0], nil
}

// cliSeams are the injection points command tests use to drive real command
// flows without a live account or a running daemon.
//
// They are grouped rather than listed among the command context because the
// two are read very differently: every field on CLIContext proper is something
// a command legitimately consults, and every field in here exists so a test can
// stand in for a subsystem. Interleaved, the distinction was carried only by a
// trailing comment on each line.
type cliSeams struct {
	syncWatch              syncWatchRunner
	syncRunOnce            syncRunOnceRunner
	syncDaemonOrchestrator syncDaemonOrchestratorFactory
	// notifyGetenv replaces os.Getenv for the systemd notify environment
	// ($NOTIFY_SOCKET, $WATCHDOG_USEC) of `sync --watch`.
	notifyGetenv      func(string) string
	statusLiveOverlay func(context.Context, *CLIContext, accountViewSnapshot) map[string]statusAccountLiveOverlay
	// statusLiveBudgetContext replaces the context that ends when the live
	// overlay's budget (statusLiveBudget) runs out, so a test ends the budget
	// itself instead of waiting 10 s or racing a shorter clock.
	statusLiveBudgetContext func(context.Context) (context.Context, context.CancelFunc)
	// identityCheckBudgetContext replaces the context that ends when the
	// account identity check's budget (identityCheckBudget) runs out.
	identityCheckBudgetContext func(context.Context) (context.Context, context.CancelFunc)
	// now replaces time.Now for when an account's identity check is due.
	now         func() time.Time
	cpAwaitCopy func(context.Context, *CLIContext, *graph.Client, string) (string, error)
	// asideRestoreTimeout replaces restoreAsideTimeout when non-zero.
	asideRestoreTimeout time.Duration
	// effectiveUID replaces os.Geteuid for the sync root refusal.
	effectiveUID func() int
}

// CLIContext bundles resolved config, flags, and logger. PersistentPreRunE
// builds it in two phases:
//   - Phase 1 (every account command): Flags, Logger, CfgPath, Env, and a
//     Runtime without config.
//   - Phase 2 (data commands): Cfg, and a Runtime holding the resolved config.
//
// Auth and account commands stop after Phase 1, so their Cfg is nil.
type CLIContext struct {
	Flags            CLIFlags
	Logger           *slog.Logger
	OutputWriter     io.Writer                  // destination for primary command output (default: os.Stdout)
	StatusWriter     io.Writer                  // destination for status, warning and progress lines (default: os.Stderr)
	CfgPath          string                     // resolved config file path (set for every account command)
	Env              config.EnvOverrides        // environment overrides read at startup (zero when no account is needed)
	GraphBaseURL     string                     // Graph root of the command's clients; empty means graph.DefaultBaseURL
	SharedTarget     *sharedTarget              // nil for ordinary drive/path commands
	Cfg              *config.ResolvedDrive      // the resolved drive; nil until a data command resolves one
	Runtime          *driveops.SessionRuntime   // every account command's runtime, with the config once a drive resolves
	PerfSession      *perf.Session              // command-scoped performance collector/logging session
	runtimes         []*driveops.SessionRuntime // every runtime newSessionRuntime built for the command, in order
	seams            cliSeams                   // test injection points; see cliSeams
	logCloser        io.Closer                  // log file closer; nil when no log file is configured
	fileLogLevel     *slog.LevelVar             // the log file's level; nil when no log file is configured
	logging          config.LoggingConfig       // logging settings the command logger was last built from
	hasLogging       bool                       // logging holds settings from a config file (not just the bootstrap flags)
	statusMu         sync.Mutex                 // guards statusErr and progressShown for concurrent progress callbacks
	statusErr        error                      // the first write StatusWriter refused; main reads it once the command returned
	progressShown    bool                       // a redrawn progress line is on screen and must be cleared before other output
	reconcileMu      sync.Mutex                 // guards reconcileNotices and selector mutation
	reconcileNotices map[string]struct{}
	lastBadLogLevel  string // the unknown log_level last warned about on a reload
	// isWatchCommand marks `sync --watch` itself, which an account email
	// change it applies or finishes does not tell: it is the running watch
	// such a change tells, and it reads the renamed config as it starts.
	isWatchCommand bool
}

// syncCommandName is the name of the `sync` command.
const syncCommandName = "sync"

// isSyncWatchCommand reports `sync --watch`.
func isSyncWatchCommand(cmd *cobra.Command) bool {
	if cmd == nil || cmd.Name() != syncCommandName {
		return false
	}
	watch, err := cmd.Flags().GetBool("watch")

	return err == nil && watch
}

// runtime returns the command's session runtime, the one owner of its Graph
// clients and of one token source per sign-in file (R-6.8.27). It is also
// the one place that points the runtime at a test's Graph server
// (GraphBaseURL), so every client the command builds uses the same base URL.
// Call it from the command goroutine: it builds the runtime lazily, and
// goroutines that fan out (drive discovery) receive the runtime it returned.
func (cc *CLIContext) runtime() *driveops.SessionRuntime {
	if cc == nil {
		return driveops.NewSessionRuntime(nil, graph.UserAgent(version), slog.Default())
	}

	if cc.Runtime == nil {
		cc.Runtime = cc.newSessionRuntime(nil, cc.Logger)
	}

	if cc.GraphBaseURL != "" && cc.Runtime.GraphBaseURL != cc.GraphBaseURL {
		cc.Runtime.GraphBaseURL = cc.GraphBaseURL
	}

	return cc.Runtime
}

// newSessionRuntime builds a session runtime for the command over holder
// (nil for a command without a resolved config) and keeps it, so main asks
// every runtime the command used for the sign-ins it could not save
// (tokenSaveFailure). Call it from the command goroutine. A nil cc builds a
// runtime nobody keeps.
func (cc *CLIContext) newSessionRuntime(holder *config.Holder, logger *slog.Logger) *driveops.SessionRuntime {
	runtime := driveops.NewSessionRuntime(holder, graph.UserAgent(version), logger)
	if cc == nil {
		return runtime
	}

	cc.runtimes = append(cc.runtimes, runtime)

	return runtime
}

// tokenSaveFailure is the save failures of the refreshed sign-ins every
// runtime of the command still holds, joined, or nil. A runtime a test set
// as Runtime directly is asked too.
func (cc *CLIContext) tokenSaveFailure() error {
	if cc == nil {
		return nil
	}

	runtimes := cc.runtimes
	if cc.Runtime != nil && !slices.Contains(runtimes, cc.Runtime) {
		runtimes = append(slices.Clip(runtimes), cc.Runtime)
	}

	// Two runtimes of one command can hold a source for the same file; its
	// failure is reported once.
	var errs []error
	seen := make(map[string]struct{})
	for _, runtime := range runtimes {
		for _, failure := range runtime.TokenSaveFailures() {
			if _, dup := seen[failure.Err.Error()]; dup {
				continue
			}
			seen[failure.Err.Error()] = struct{}{}
			errs = append(errs, failure.Err)
		}
	}

	return errors.Join(errs...)
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

// Session is the file-command helper for creating an interactive session from
// the resolved CLI drive at the config boundary.
// Eliminates 7 identical boilerplate blocks across file operation commands.
func (cc *CLIContext) Session(ctx context.Context) (*driveops.MountSession, error) {
	runtime := cc.runtime()

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
			// The probe flushed the token cache with the rename.
			if reloadErr := cc.reloadResolvedDriveFromFlags(); reloadErr != nil {
				return nil, reloadErr
			}
		}
	}

	mountCfg, err := interactiveMountSessionConfigFromResolvedDrive(cc.Cfg)
	if err != nil {
		return nil, err
	}

	session, err := runtime.InteractiveSession(mountCfg)
	if err != nil {
		return nil, fmt.Errorf("create drive session: %w", err)
	}

	// Successful authenticated Graph calls prove the saved login still works.
	// Attach a best-effort hook so live CLI commands clear stale catalog auth
	// requirements without treating session construction itself as proof.
	// The proof is the token owner's, the account the session signs in as.
	attachDriveAuthProof(session, newAuthProofRecorder(cc.Logger), mountCfg.TokenOwnerCanonical, "drive-session")

	return session, nil
}

func interactiveMountSessionConfigFromResolvedDrive(rd *config.ResolvedDrive) (*driveops.MountSessionConfig, error) {
	if rd == nil {
		return nil, errors.New("resolved drive is required")
	}

	tokenOwnerCanonical, err := config.TokenAccountCanonicalID(rd.CanonicalID)
	if err != nil {
		return nil, fmt.Errorf("token owner for drive %q: %w", rd.CanonicalID, err)
	}

	accountEmail := rd.CanonicalID.Email()
	if accountEmail == "" {
		accountEmail = tokenOwnerCanonical.Email()
	}

	return &driveops.MountSessionConfig{
		TokenOwnerCanonical: tokenOwnerCanonical,
		DriveID:             rd.DriveID,
		RemoteRootItemID:    rd.RemoteRootItemID,
		AccountEmail:        accountEmail,
	}, nil
}

// newGraphClientWithHTTP builds a Graph client over a token source the caller
// already holds. Only login uses it, for the source the sign-in just
// produced; every other command client comes from the session runtime
// (SessionRuntime.BootstrapClient or a drive session), which owns one token
// source per sign-in file.
func newGraphClientWithHTTP(
	baseURL string,
	httpClient *http.Client,
	ts graph.TokenSource,
	logger *slog.Logger,
) (*graph.Client, error) {
	if baseURL == "" {
		baseURL = graph.DefaultBaseURL
	}

	client, err := graph.NewClient(baseURL, httpClient, ts, logger, graph.UserAgent(version))
	if err != nil {
		return nil, fmt.Errorf("creating graph client: %w", err)
	}

	return client, nil
}

// cobraHelpCommandName and cobraCompletionCommandName are the commands cobra
// adds to the root itself; cobra exports only the names of its hidden
// completion requests.
const (
	cobraHelpCommandName       = "help"
	cobraCompletionCommandName = "completion"
)

// commandNeedsNoAccount reports the commands cobra adds itself: help, the
// shell completion scripts and the hidden completion requests a shell makes.
// They read and write nothing of onedrive-go's, so they need no CLI context,
// no config and no finished email change, and work whatever state the data
// is in. (--help and --version never reach PersistentPreRunE.)
func commandNeedsNoAccount(cmd *cobra.Command) bool {
	top := cmd
	for top.HasParent() && top.Parent().HasParent() {
		top = top.Parent()
	}
	if !top.HasParent() {
		return false
	}

	switch top.Name() {
	case cobraHelpCommandName, cobraCompletionCommandName, cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
		return true
	default:
		return false
	}
}

// initializeCLIContext is the root's PersistentPreRunE: it builds the
// CLIContext (Phase 1: flags, logger, config path, env), finishes an account
// email change an earlier command was cut short in, then resolves a shared
// target or, for a data command, the drive (Phase 2). The commands cobra adds
// itself skip all of it (commandNeedsNoAccount).
func initializeCLIContext(
	cmd *cobra.Command,
	args []string,
	bindings rootFlagBindings,
	outputWriter io.Writer,
	statusWriter io.Writer,
) error {
	if version == "dev" {
		// Keep go run . and unversioned builds away from production data.
		if err := config.CheckDevSafe(); err != nil {
			return err //nolint:wrapcheck // CheckDevSafe's text is the user-facing message.
		}
	}
	if commandNeedsNoAccount(cmd) {
		return nil
	}

	flags := bindings.cliFlags()
	// Finding the config file logs before the log settings are known, so
	// those records wait in a buffer and go through the command logger once
	// it is built: same console rules, and into the log file too.
	bootstrapLog := newBootstrapLogBuffer()
	bootstrapLogger := slog.New(bootstrapLog)
	env := config.ReadEnvOverrides(bootstrapLogger)
	cc := &CLIContext{
		OutputWriter: outputWriter,
		Flags:        flags,
		Logger:       buildLoggerWithStatusWriter(nil, flags, statusWriter),
		StatusWriter: statusWriter,
		CfgPath:      config.ResolveConfigPath(env, config.CLIOverrides{ConfigPath: flags.ConfigPath}, bootstrapLogger),
		Env:          env,
	}
	cc.isWatchCommand = isSyncWatchCommand(cmd)

	// cobra's Execute gives every command a context, so none is nil here.
	ctx := cmd.Context()

	// Every command gets the configured log file before it does any work,
	// including the commands that skip the strict config load (auth, drive,
	// status, sync, shared targets). A command that resolves a drive later
	// reuses this logger, because logging settings are global.
	if err := cc.attachConfiguredLogging(); err != nil {
		return err
	}
	bootstrapLog.replayInto(ctx, cc.Logger.Handler())
	// The command's one session runtime is built when it is first needed
	// (cc.runtime, syncSessionRuntime, initializeResolvedCLIContext), never
	// here: a data command would otherwise hold this one and the one its
	// resolved config builds, two token sources for one sign-in file.

	perfSession, perfCtx := newCommandPerfSession(ctx, cmd, cc.Logger)
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

	// An account email change an earlier command was cut short in is
	// finished here, once, before anything resolves a drive or a shared
	// target by name; a command whose change cannot be finished fails.
	if err := cc.catchUpEmailChanges(ctx); err != nil {
		return err
	}

	sharedTarget, found, err := resolveSharedTargetBootstrap(ctx, cmd, args, cc)
	if err != nil {
		return err
	}
	if found {
		cc.SharedTarget = sharedTarget
	}

	skipsConfig := cmd.Annotations[skipConfigAnnotation] == skipConfigValue
	if cc.SharedTarget == nil && !skipsConfig {
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

	if err := cc.useLoggingConfig(resolved.LoggingConfig); err != nil {
		return err
	}
	// One runtime per command: a runtime a setup step already built through
	// cc.runtime gets the resolved holder instead of a second runtime beside
	// it, which would load its own token source for the same sign-in file.
	holder := config.NewHolder(rawCfg, cc.CfgPath)
	if cc.Runtime == nil {
		cc.Runtime = cc.newSessionRuntime(holder, cc.Logger)
	} else {
		cc.Runtime.UseHolder(holder)
	}

	return nil
}

func bindRootFlags(cmd *cobra.Command, bindings *rootFlagBindings) {
	cmd.PersistentFlags().StringVar(&bindings.configPath, "config", "", "config file path")
	cmd.PersistentFlags().StringVar(&bindings.account, "account", "",
		"account email, or account ID when two accounts share it (e.g., user@example.com, business:user@example.com)")
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
		Long:          "A OneDrive CLI and sync client for Linux and macOS.",
		Version:       version,
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Cobra checks flag groups only after this hook, so a conflict
			// such as --quiet --verbose would otherwise surface as whatever
			// config or login error setup hits first.
			if err := cmd.ValidateFlagGroups(); err != nil {
				return &usageError{commandPath: cmd.CommandPath(), err: err}
			}

			return initializeCLIContext(cmd, args, bindings, outputWriter, statusWriter)
		},
	}

	bindRootFlags(cmd, &bindings)

	addRootSubcommands(cmd)
	markUsageErrors(cmd)

	return cmd
}

func addRootSubcommands(cmd *cobra.Command) {
	cmd.AddCommand(
		newLoginCmd(), newLogoutCmd(), newStatusCmd(),
		newPerfCmd(),
		newSharedCmd(),
		newDriveCmd(), newLsCmd(), newGetCmd(), newPutCmd(),
		newRmCmd(), newMkdirCmd(), newStatCmd(), newSyncCmd(),
		newPauseCmd(), newResumeCmd(),
		newMvCmd(), newCpCmd(),
		newRecycleBinCmd(),
		newTrashCmd(),
	)
}

// Main executes the CLI with the provided args and returns the desired
// process exit code. Keeping exit behavior here leaves the root package as
// a thin entrypoint while the command layer owns user-facing presentation.
func Main(args []string) int {
	return mainWithWriters(args, nil, nil)
}

// rootCLIContext is the command context PersistentPreRunE stored on the root
// command, which main reaches after the command returned; nil when it was
// never built.
func rootCLIContext(cmd *cobra.Command) *CLIContext {
	if cmd == nil {
		return nil
	}

	ctx := cmd.Root().Context()
	if ctx == nil {
		return nil
	}

	return cliContextFrom(ctx)
}

func closeRootCommandLogger(cmd *cobra.Command) error {
	return rootCLIContext(cmd).closeCommandLogger()
}

// newSignalLogger reports signal handling before the command logger exists.
// It writes to the status stream so an interrupt is visible to the user
// without polluting command output.
func newSignalLogger(statusWriter io.Writer) *slog.Logger {
	if statusWriter == nil {
		statusWriter = os.Stderr
	}

	// The stop notice is meant for whoever pressed Ctrl-C, so it renders as a
	// plain line (or a journal line) rather than a key=value log record.
	style := consoleLogHuman
	if isWriterJournal(statusWriter) {
		style = consoleLogJournal
	}

	return slog.New(newConsoleLogHandler(statusWriter, slog.LevelInfo, style))
}

// writeCommandError prints main's final error and returns the write's error.
// Under the systemd journal each line carries the err priority, so
// `journalctl -p err` finds it.
func writeCommandError(w io.Writer, text string) error {
	if isWriterJournal(w) {
		text = "<3>" + strings.ReplaceAll(text, "\n", "\n<3>")
	} else if isWriterTTY(w) {
		// Clear a progress line the failed command may have left behind.
		text = clearLine + text
	}

	return writeWarningf(w, "%s\n", text)
}

// asUsageError tags command-line mistakes so main can point at --help and
// exit 2. Cobra does not type these errors, so unknown commands and flag
// group conflicts are recognized by their fixed cobra wording.
func asUsageError(executed *cobra.Command, err error) error {
	if err == nil {
		return nil
	}

	var usage *usageError
	if errors.As(err, &usage) {
		return err
	}

	text := err.Error()
	if strings.HasPrefix(text, "unknown command ") ||
		strings.HasPrefix(text, "if any flags in the group") ||
		strings.HasPrefix(text, "required flag(s)") {
		commandPath := "onedrive-go"
		if executed != nil {
			commandPath = executed.CommandPath()
		}

		return &usageError{commandPath: commandPath, err: err}
	}

	return err
}

// markUsageErrors makes flag-parsing and argument-count failures of every
// command a usageError.
func markUsageErrors(root *cobra.Command) {
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &usageError{commandPath: cmd.CommandPath(), err: err}
	})

	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		if validate := cmd.Args; validate != nil {
			cmd.Args = func(c *cobra.Command, args []string) error {
				if err := validate(c, args); err != nil {
					return &usageError{commandPath: c.CommandPath(), err: err}
				}

				return nil
			}
		}

		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)
}

// statusStreamFailedMessage is the command logger's record of a command whose
// status stream refused a write.
const statusStreamFailedMessage = "could not write to stderr"

// mainWithWriters runs the command and returns the process exit status. The
// final error of a failed command is written while the log file is still
// open, so a stderr that refuses it leaves one warn record there
// (statusStreamFailure); the exit status stays the command's. What is
// written after the log file closed has nowhere left to be recorded and a
// refused write of it is dropped: the warning that the log file did not
// close, and the final error of a command whose only failure was that close.
// Both are written only for a command whose exit status is not 0 already.
func mainWithWriters(args []string, outputWriter, statusWriter io.Writer) (exitCode int) {
	cmd := newRootCmdWithWriters(outputWriter, statusWriter)
	cmd.SetArgs(args)

	// Every command is interruptible, not just sync. Without this a signal
	// would kill the process outright and nothing deferred would run, so an
	// interrupted transfer would leave its temporary state behind with no
	// chance to clean up.
	// The signal logger writes to the status stream because the configured
	// command logger does not exist until the command starts.
	signalLogger := newSignalLogger(statusWriter)
	ctx, cancel := shutdownContext(context.Background(), signalLogger)
	defer cancel()
	// The last line of defense: a panic on the command's own goroutine that
	// nothing below recovered ends the command with exit 1, never Go's 2
	// (R-6.3.10).
	defer func() {
		if r := recover(); r != nil {
			exitCode = reportMainPanic(cmd, statusWriter, signalLogger, r)
		}
	}()

	executed, err := cmd.ExecuteContextC(ctx)
	err = asUsageError(executed, err)
	err = withCommandPanic(cmd.Context(), err)
	err = withTokenSaveFailure(rootCLIContext(cmd).tokenSaveFailure(), err)
	completeRootCommandPerf(cmd, err)
	stopRequested := ctx.Err() != nil
	exitCode = commandExitCode(err, stopRequested)
	if writeErr := writeFinalCommandError(cmd, statusWriter, exitCode, err); writeErr != nil {
		if cc := rootCLIContext(cmd); cc != nil {
			cc.recordStatusError(fmt.Errorf("write final error: %w", writeErr))
		}
	}
	// Read while the log file is open, so the failure reaches it.
	statusErr := statusStreamFailure(cmd)
	if closeErr := closeRootCommandLogger(cmd); closeErr != nil {
		if err == nil {
			exitCode = commandExitCode(closeErr, stopRequested)
			_ = writeFinalCommandError(cmd, statusWriter, exitCode, closeErr) //nolint:errcheck // see the doc comment
		} else {
			_ = writeWarningf(statusWriter, "Warning: could not close the log file: %v\n", closeErr) //nolint:errcheck // see the doc comment
		}
	}

	return exitCodeAfterStatusStream(exitCode, statusErr)
}

// writeFinalCommandError writes main's last line for a command that ended
// with exitCode and err, and returns the write's error. A command that
// worked writes nothing, and so does one the user stopped (Ctrl-C,
// SIGTERM): the signal handler already said "Stopping...". A timeout or an
// internal cancellation is a failure and says so.
func writeFinalCommandError(cmd *cobra.Command, statusWriter io.Writer, exitCode int, err error) error {
	switch exitCode {
	case exitSuccess, exitInterrupted:
		return nil
	case exitUsage:
		return writeCommandError(statusWriter, commandErrorText(err, false))
	default:
		return writeCommandError(statusWriter, commandFailureText(err, commandWantsErrorDetails(cmd)))
	}
}

// statusStreamFailure returns the first write the command's status stream
// refused, or nil, and gives it once to the command logger at warn level:
// the log file gets it when there is one. A command whose context was never
// built (a usage error, --help) has no record.
func statusStreamFailure(cmd *cobra.Command) error {
	cc := rootCLIContext(cmd)
	if cc == nil {
		return nil
	}

	err := cc.StatusError()
	if err != nil && cc.Logger != nil {
		cc.Logger.Warn(statusStreamFailedMessage, slog.Any("error", err))
	}

	return err
}

// exitCodeAfterStatusStream is the exit status of a command whose status
// stream refused a write (streamErr): a command that worked ends with exit 1,
// as the GNU tools do when standard error fails, and a command that failed
// keeps its own status.
func exitCodeAfterStatusStream(exitCode int, streamErr error) int {
	if streamErr != nil && exitCode == exitSuccess {
		return exitFailure
	}

	return exitCode
}

// commandFailureText is what main prints for a failed command. A command that
// ended with a recovered panic also gets the panic's stack: run without a log
// file, the command has nowhere else to keep what the bug report needs.
func commandFailureText(err error, details bool) string {
	text := commandErrorText(err, details)
	if stack := panicguard.Stack(err); stack != "" {
		text += "\n\n" + strings.TrimRight(stack, "\n")
	}

	return text
}

// reportMainPanic handles a panic on the command's goroutine that nothing
// below recovered. It is logged once, with its stack, through the command
// logger, which reaches the log file when there is one (before the command
// logger exists, through fallback); the command's performance session is
// completed with the panic's error, so the summary reads "error" as for a
// helper goroutine's panic (a session already completed, by main or by the
// panic's own path through Complete, is left as it is); the log file is then
// closed so both records are on disk; and the error and the stack go to
// stderr, as for any command that ends with a recovered panic. A write of
// those last lines that fails is dropped: the exit status is 1 already and
// the log file is closed.
func reportMainPanic(cmd *cobra.Command, statusWriter io.Writer, fallback *slog.Logger, r any) int {
	logger := fallback
	if cc := rootCLIContext(cmd); cc != nil && cc.Logger != nil {
		logger = cc.Logger
	}

	err := panicguard.Recovered(logger, "in onedrive-go", r)
	completeRootCommandPerf(cmd, err)
	if closeErr := closeRootCommandLogger(cmd); closeErr != nil {
		_ = writeWarningf(statusWriter, "Warning: could not close the log file: %v\n", closeErr) //nolint:errcheck // see the doc comment
	}
	_ = writeCommandError(statusWriter, commandFailureText(err, false)) //nolint:errcheck // see the doc comment

	return exitFailure
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
	if cmd.Name() == syncCommandName {
		if watch, err := cmd.Flags().GetBool("watch"); err == nil && watch {
			interval = watchCommandPerfUpdateInterval
		}
	}

	return perf.NewSession(ctx, logger, "command", cmd.CommandPath(), interval, nil)
}

func completeRootCommandPerf(cmd *cobra.Command, err error) {
	cc := rootCLIContext(cmd)
	if cc == nil || cc.PerfSession == nil {
		return
	}

	cc.PerfSession.Complete(err)
}

// loadAndResolve resolves the effective configuration from the four-layer
// override chain. Returns the resolved drive config and the raw parsed config
// (needed by SessionRuntime for shared drive token resolution).
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
			return nil, nil, errors.New("--drive can only be specified once for this command (use 'sync' for multi-drive)")
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

// commandWantsErrorDetails reports whether --verbose or --debug asked for the
// full error chain alongside the plain message.
func commandWantsErrorDetails(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}

	cc := cliContextFrom(cmd.Context())
	if cc == nil {
		return false
	}

	return cc.Flags.Verbose || cc.Flags.Debug
}
