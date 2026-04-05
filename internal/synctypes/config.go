package synctypes

import (
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
)

// LocalFilterConfig controls local-only observation exclusions. These filters
// affect what the scanner/watch pipeline turns into change events; they do not
// rewrite remote observation semantics.
type LocalFilterConfig struct {
	SkipDotfiles bool
	SkipSymlinks bool
	SkipDirs     []string
	SkipFiles    []string
}

// LocalObservationRules controls platform-derived local validation semantics.
// These are not user-configured exclusions; they encode rules that depend on
// the target drive type or sync surface.
type LocalObservationRules struct {
	RejectSharePointRootForms bool
}

// EngineConfig holds the options for NewEngine. Uses a struct because
// seven fields is too many for positional parameters.
type EngineConfig struct {
	DBPath             string       // path to the SQLite state database
	SyncRoot           string       // absolute path to the local sync directory
	DataDir            string       // application data directory for session files (optional)
	DriveID            driveid.ID   // normalized drive identifier
	DriveType          string       // canonical drive type ("personal", "business", "sharepoint", "shared")
	AccountEmail       string       // authenticated account email for caller-aware permission checks
	RootItemID         string       // folder-scoped virtual root; empty = drive root
	Fetcher            DeltaFetcher // satisfied by *graph.Client
	SocketIOFetcher    SocketIOEndpointFetcher
	Items              ItemClient          // satisfied by *graph.Client
	Downloads          driveops.Downloader // satisfied by *graph.Client
	Uploads            driveops.Uploader   // satisfied by *graph.Client
	DriveVerifier      DriveVerifier       // optional: verified at startup (B-074); nil skips check
	FolderDelta        FolderDeltaFetcher  // optional: folder-scoped delta for shortcut observation (6.4b)
	RecursiveLister    RecursiveLister     // optional: recursive listing for shortcut observation (6.4b)
	PermChecker        PermissionChecker   // optional: permission checking for shared folders (6.4c)
	Logger             *slog.Logger
	LocalFilter        LocalFilterConfig
	LocalRules         LocalObservationRules
	SyncScope          syncscope.Config
	EnableWebsocket    bool  // when true, full-drive watch mode enables outbound Socket.IO wakeups
	UseLocalTrash      bool  // move deleted local files to OS trash instead of permanent delete
	TransferWorkers    int   // goroutine count for the worker pool (0 → minWorkers)
	CheckWorkers       int   // goroutine limit for parallel file hashing (0 → 4)
	BigDeleteThreshold int   // max delete actions before big-delete protection triggers (0 → defaultBigDeleteThreshold)
	MinFreeSpace       int64 // minimum free disk space (bytes) before downloads; 0 disables (R-6.4.7)
}

// RunOpts holds per-pass options for RunOnce.
type RunOpts struct {
	DryRun        bool
	Force         bool
	FullReconcile bool // when true, runs a full delta enumeration + orphan detection
}

// WatchOpts holds per-watch options for RunWatch.
type WatchOpts struct {
	Force              bool
	PollInterval       time.Duration // remote delta polling interval (0 → 5m)
	Debounce           time.Duration // buffer debounce window (0 → 2s)
	SafetyScanInterval time.Duration // local safety scan interval (0 → 5m) (B-099)
	ReconcileInterval  time.Duration // periodic full reconciliation (0 → 24h, negative = disabled)
}

// DefaultBigDeleteThreshold is the default absolute delete count threshold.
// Engine uses this when EngineConfig.BigDeleteThreshold == 0.
const DefaultBigDeleteThreshold = 1000

// SafetyConfig controls big-delete protection thresholds.
// Single absolute count threshold — no percentages, no per-folder checks.
// Industry standard approach (rclone, rsync, abraunegg).
type SafetyConfig struct {
	BigDeleteThreshold int // max number of delete actions before triggering (0 = disabled)
}

// DefaultSafetyConfig returns a SafetyConfig with the default threshold.
func DefaultSafetyConfig() *SafetyConfig {
	return &SafetyConfig{
		BigDeleteThreshold: DefaultBigDeleteThreshold,
	}
}

// SyncReport summarizes the result of a single sync pass.
type SyncReport struct {
	Mode     SyncMode
	DryRun   bool
	Duration time.Duration

	// Plan counts (always populated, even for dry-run).
	FolderCreates int
	Moves         int
	Downloads     int
	Uploads       int
	LocalDeletes  int
	RemoteDeletes int
	Conflicts     int
	SyncedUpdates int
	Cleanups      int

	// Execution results (zero for dry-run).
	Succeeded int
	Failed    int
	Errors    []error
}

// DriveReport summarizes the result of a single drive's sync run.
// Err and Report are mutually exclusive: when Err is set, Report is nil.
type DriveReport struct {
	CanonicalID driveid.CanonicalID
	DisplayName string
	Report      *SyncReport // nil when Err is set
	Err         error
}
