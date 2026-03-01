package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// version is set at build time via ldflags.
var version = "dev"

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
	Drive      string
	JSON       bool
	Verbose    bool
	Debug      bool
	Quiet      bool
}

// CLIContext bundles resolved config, flags, and logger. Created in
// PersistentPreRunE with two-phase initialization:
//   - Phase 1 (always): Flags + Logger populated for every command.
//   - Phase 2 (data commands): Cfg + RawConfig populated after config resolution.
//
// Auth commands get CLIContext with Flags + Logger but nil Cfg/RawConfig.
type CLIContext struct {
	Flags     CLIFlags
	Logger    *slog.Logger
	Cfg       *config.ResolvedDrive // nil for auth/account commands
	RawConfig *config.Config        // nil for auth/account commands
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

// httpClientTimeout is the default timeout for HTTP requests.
// Prevents hung connections from blocking CLI commands indefinitely.
const httpClientTimeout = 30 * time.Second

// defaultHTTPClient returns an HTTP client with a sensible timeout.
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: httpClientTimeout}
}

// transferHTTPClient returns an HTTP client with no timeout for
// upload/download operations. Large file transfers on slow connections
// can exceed the 30-second default (e.g., 10MB chunks at 100KB/s = 100s).
// Transfers are bounded by context cancellation instead.
func transferHTTPClient() *http.Client {
	return &http.Client{Timeout: 0}
}

// newGraphClient creates a graph.Client with the standard HTTP client,
// user-agent, and base URL. Eliminates boilerplate repeated across commands.
func newGraphClient(ts graph.TokenSource, logger *slog.Logger) *graph.Client {
	return graph.NewClient(graph.DefaultBaseURL, defaultHTTPClient(), ts, logger, "onedrive-go/"+version)
}

// newTransferGraphClient creates a graph.Client without a timeout for
// upload/download operations. Metadata operations (ls, rm, mkdir, stat,
// Drives(), Me()) should use newGraphClient with the 30-second timeout.
func newTransferGraphClient(ts graph.TokenSource, logger *slog.Logger) *graph.Client {
	return graph.NewClient(graph.DefaultBaseURL, transferHTTPClient(), ts, logger, "onedrive-go/"+version)
}

// newRootCmd builds and returns the fully-assembled root command with all
// subcommands registered. Called once from main().
func newRootCmd() *cobra.Command {
	// Local flag variables — NOT globals. Cobra binds to these; Phase 1 of
	// PersistentPreRunE copies them into CLIFlags on CLIContext.
	var (
		flagConfigPath string
		flagAccount    string
		flagDrive      string
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
		//   Phase 1 (always): read Cobra flags → build CLIFlags → build bootstrap logger
		//   Phase 2 (data commands only): load config → resolve drive → build final logger
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			// Phase 1: always populate flags + bootstrap logger.
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
			cc := &CLIContext{Flags: flags, Logger: logger}

			// Phase 2: load config for data commands.
			if cmd.Annotations[skipConfigAnnotation] != "true" {
				resolved, rawCfg, err := loadAndResolve(cmd, flags, logger)
				if err != nil {
					return err
				}

				cc.Cfg = resolved
				cc.RawConfig = rawCfg
				cc.Logger = buildLogger(resolved, flags)
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
	}

	cmd.PersistentFlags().StringVar(&flagConfigPath, "config", "", "config file path")
	cmd.PersistentFlags().StringVar(&flagAccount, "account", "", "account for auth commands (e.g., user@example.com)")
	cmd.PersistentFlags().StringVar(&flagDrive, "drive", "", "drive selector (canonical ID, display name, or partial match)")
	cmd.PersistentFlags().BoolVar(&flagJSON, "json", false, "output in JSON format")
	cmd.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "show detailed output")
	cmd.PersistentFlags().BoolVar(&flagDebug, "debug", false, "enable debug logging (HTTP requests, config resolution)")
	cmd.PersistentFlags().BoolVarP(&flagQuiet, "quiet", "q", false, "suppress informational output")

	cmd.MarkFlagsMutuallyExclusive("verbose", "debug", "quiet")

	// Register subcommands.
	cmd.AddCommand(newLoginCmd())
	cmd.AddCommand(newLogoutCmd())
	cmd.AddCommand(newWhoamiCmd())
	cmd.AddCommand(newStatusCmd())
	cmd.AddCommand(newDriveCmd())
	cmd.AddCommand(newLsCmd())
	cmd.AddCommand(newGetCmd())
	cmd.AddCommand(newPutCmd())
	cmd.AddCommand(newRmCmd())
	cmd.AddCommand(newMkdirCmd())
	cmd.AddCommand(newStatCmd())
	cmd.AddCommand(newSyncCmd())
	cmd.AddCommand(newPauseCmd())
	cmd.AddCommand(newResumeCmd())
	cmd.AddCommand(newConflictsCmd())
	cmd.AddCommand(newVerifyCmd())
	cmd.AddCommand(newResolveCmd())

	return cmd
}

// loadAndResolve resolves the effective configuration from the four-layer
// override chain. Returns the resolved drive config and the raw parsed config
// (needed by DriveSession for shared drive token resolution).
func loadAndResolve(cmd *cobra.Command, flags CLIFlags, logger *slog.Logger) (*config.ResolvedDrive, *config.Config, error) {
	cli := config.CLIOverrides{
		ConfigPath: flags.ConfigPath,
	}

	// Only pass --drive to the resolver if the user explicitly set it.
	if cmd.Flags().Changed("drive") {
		cli.Drive = flags.Drive
	}

	env := config.ReadEnvOverrides(logger)

	logger.Debug("resolving config",
		slog.String("config_path", cli.ConfigPath),
		slog.String("cli_drive", cli.Drive),
		slog.String("env_config", env.ConfigPath),
		slog.String("env_drive", env.Drive),
	)

	resolved, err := config.ResolveDrive(env, cli, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}

	logger.Debug("config resolved",
		slog.String("canonical_id", resolved.CanonicalID.String()),
		slog.String("sync_dir", resolved.SyncDir),
		slog.String("drive_id", resolved.DriveID.String()),
	)

	// Load the raw config for DriveSession token resolution. Use the same
	// config path resolution as ResolveDrive (CLI > env > default) to ensure
	// we read the same file.
	cfgPath := config.DefaultConfigPath()
	if env.ConfigPath != "" {
		cfgPath = env.ConfigPath
	}

	if cli.ConfigPath != "" {
		cfgPath = cli.ConfigPath
	}

	rawCfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		// Non-fatal: raw config is optional (only needed for shared drives).
		logger.Debug("could not load raw config for token resolution", "error", err)

		rawCfg = config.DefaultConfig()
	}

	return resolved, rawCfg, nil
}

// buildLogger creates an slog.Logger configured by the resolved config and
// CLI flags. Pass nil for pre-config bootstrap (no config-file log level).
// Config-file log level provides the baseline; --verbose, --debug, and --quiet
// override it because CLI flags always win. The flags are mutually exclusive
// (enforced by Cobra).
func buildLogger(cfg *config.ResolvedDrive, flags CLIFlags) *slog.Logger {
	level := slog.LevelWarn

	// Config-based log level (lower priority than CLI flags).
	if cfg != nil {
		switch cfg.LogLevel {
		case "debug":
			level = slog.LevelDebug
		case "info":
			level = slog.LevelInfo
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		default:
			if cfg.LogLevel != "" {
				fmt.Fprintf(os.Stderr, "warning: unknown log level %q, using warn\n", cfg.LogLevel)
			}
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

	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// exitOnError prints a user-friendly error message to stderr and exits.
func exitOnError(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}
