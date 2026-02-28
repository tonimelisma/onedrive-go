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

// Global persistent flags, bound in setupRootCmd().
var (
	flagConfigPath string
	flagAccount    string
	flagDrive      string
	flagJSON       bool
	flagVerbose    bool
	flagDebug      bool
	flagQuiet      bool
)

// skipConfigAnnotation marks commands that handle config loading themselves.
// Commands annotated with this key skip the automatic four-layer config
// resolution in PersistentPreRunE. This replaces the fragile string map
// (skipConfigCommands) which required manual maintenance when adding commands.
const skipConfigAnnotation = "skipConfig"

// CLIContext bundles resolved config and logger. Created once in
// PersistentPreRunE; eliminates redundant buildLogger calls in RunE handlers.
type CLIContext struct {
	Cfg    *config.ResolvedDrive
	Logger *slog.Logger
}

// cliContextKey is the context key for CLIContext.
type cliContextKey struct{}

// cliContextFrom extracts the CLIContext from the command's context.
// Returns nil if no config was loaded (e.g., auth commands that skip config).
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
	cmd := &cobra.Command{
		Use:     "onedrive-go",
		Short:   "OneDrive CLI client",
		Long:    "A fast, safe OneDrive CLI and sync client for Linux and macOS.",
		Version: version,
		// Silence Cobra's default error/usage printing — we handle it ourselves.
		SilenceErrors: true,
		SilenceUsage:  true,
		// PersistentPreRunE loads configuration before every command. Commands
		// annotated with skipConfigAnnotation handle config access themselves.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Annotations[skipConfigAnnotation] == "true" {
				return nil
			}

			return loadConfig(cmd)
		},
	}

	cmd.PersistentFlags().StringVar(&flagConfigPath, "config", "", "config file path")
	cmd.PersistentFlags().StringVar(&flagAccount, "account", "", "account for auth commands (e.g., user@example.com)")
	cmd.PersistentFlags().StringVar(&flagDrive, "drive", "", "drive selector (canonical ID, alias, or partial match)")
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
	cmd.AddCommand(newConflictsCmd())
	cmd.AddCommand(newVerifyCmd())
	cmd.AddCommand(newResolveCmd())

	return cmd
}

// loadConfig resolves the effective configuration from the four-layer override
// chain and stores the result in the command's context for use by subcommands.
func loadConfig(cmd *cobra.Command) error {
	// Bootstrap logger derived from CLI flags only (config doesn't exist yet).
	logger := buildLogger(nil)

	cli := config.CLIOverrides{
		ConfigPath: flagConfigPath,
	}

	// Only pass --drive to the resolver if the user explicitly set it.
	if cmd.Flags().Changed("drive") {
		cli.Drive = flagDrive
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
		return fmt.Errorf("loading config: %w", err)
	}

	logger.Debug("config resolved",
		slog.String("canonical_id", resolved.CanonicalID.String()),
		slog.String("sync_dir", resolved.SyncDir),
		slog.String("drive_id", resolved.DriveID.String()),
	)

	// Build the final logger incorporating config-file log level.
	finalLogger := buildLogger(resolved)
	cc := &CLIContext{Cfg: resolved, Logger: finalLogger}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	cmd.SetContext(context.WithValue(ctx, cliContextKey{}, cc))

	config.WarnUnimplemented(resolved, finalLogger)

	return nil
}

// buildLogger creates an slog.Logger configured by the resolved config and
// CLI flags. Pass nil for pre-config bootstrap (no config-file log level).
// Config-file log level provides the baseline; --verbose, --debug, and --quiet
// override it because CLI flags always win. The flags are mutually exclusive
// (enforced by Cobra).
func buildLogger(cfg *config.ResolvedDrive) *slog.Logger {
	level := slog.LevelWarn

	// Config-based log level (lower priority than CLI flags).
	if cfg != nil {
		switch cfg.LogLevel {
		case "debug":
			level = slog.LevelDebug
		case "info":
			level = slog.LevelInfo
		case "error":
			level = slog.LevelError
		}
	}

	// CLI flags override config (highest priority).
	if flagVerbose {
		level = slog.LevelInfo
	}

	if flagDebug {
		level = slog.LevelDebug
	}

	if flagQuiet {
		level = slog.LevelError
	}

	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// exitOnError prints a user-friendly error message to stderr and exits.
func exitOnError(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}
