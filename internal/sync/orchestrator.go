package sync

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	gosync "sync"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// engineRunner is the interface the Orchestrator uses to run sync cycles.
// Implemented by *Engine; mock implementations are used in tests.
type engineRunner interface {
	RunOnce(ctx context.Context, mode SyncMode, opts RunOpts) (*SyncReport, error)
	Close() error
}

// engineFactoryFunc creates an engineRunner from an EngineConfig.
// The real implementation calls NewEngine; tests inject mocks.
type engineFactoryFunc func(cfg *EngineConfig) (engineRunner, error)

// tokenSourceFactory creates a TokenSource from a token file path.
// The real implementation calls graph.TokenSourceFromPath; tests inject stubs.
type tokenSourceFactory func(ctx context.Context, tokenPath string, logger *slog.Logger) (graph.TokenSource, error)

// driveDiscoveryFunc discovers the primary drive ID when ResolvedDrive.DriveID
// is zero. The real implementation calls client.Drives(ctx); tests inject stubs.
type driveDiscoveryFunc func(ctx context.Context, client *graph.Client) (driveid.ID, error)

// OrchestratorConfig holds the inputs for creating an Orchestrator.
// The CLI layer populates this from resolved config and HTTP clients.
type OrchestratorConfig struct {
	Config       *config.Config
	Drives       []*config.ResolvedDrive
	ConfigPath   string       // for 6.0c SIGHUP reload
	MetaHTTP     *http.Client // 30s timeout
	TransferHTTP *http.Client // no timeout
	UserAgent    string
	Logger       *slog.Logger
}

// clientPair holds metadata and transfer Graph API clients for a single
// token path. Multiple drives on the same account share one clientPair.
type clientPair struct {
	Meta     *graph.Client
	Transfer *graph.Client
}

// Orchestrator manages per-drive sync runners. It is ALWAYS used, even
// for a single drive — no separate single-drive code path (MULTIDRIVE.md §11.7).
type Orchestrator struct {
	cfg              *OrchestratorConfig
	clients          map[string]*clientPair // keyed by token file path
	engineFactory    engineFactoryFunc      // injectable for tests
	tokenSourceFn    tokenSourceFactory     // injectable for tests
	driveDiscoveryFn driveDiscoveryFunc     // injectable for tests
	logger           *slog.Logger
}

// NewOrchestrator creates an Orchestrator with real Engine and TokenSource
// factories. Tests override engineFactory, tokenSourceFn, and driveDiscoveryFn
// after construction.
func NewOrchestrator(cfg *OrchestratorConfig) *Orchestrator {
	return &Orchestrator{
		cfg:     cfg,
		clients: make(map[string]*clientPair),
		engineFactory: func(ecfg *EngineConfig) (engineRunner, error) {
			return NewEngine(ecfg)
		},
		tokenSourceFn:    graph.TokenSourceFromPath,
		driveDiscoveryFn: defaultDriveDiscovery,
		logger:           cfg.Logger,
	}
}

// defaultDriveDiscovery discovers the primary drive ID by calling /me/drives.
// Used when ResolvedDrive.DriveID is zero (config has no drive_id field).
func defaultDriveDiscovery(ctx context.Context, client *graph.Client) (driveid.ID, error) {
	drives, err := client.Drives(ctx)
	if err != nil {
		return driveid.ID{}, fmt.Errorf("discovering drive: %w", err)
	}

	if len(drives) == 0 {
		return driveid.ID{}, fmt.Errorf("no drives found for this account")
	}

	return drives[0].ID, nil
}

// getOrCreateClient returns a cached clientPair for the given token path,
// creating one if it doesn't exist. Multiple drives on the same account
// share one clientPair (keyed by token path).
func (o *Orchestrator) getOrCreateClient(ctx context.Context, tokenPath string) (*clientPair, error) {
	if pair, ok := o.clients[tokenPath]; ok {
		return pair, nil
	}

	ts, err := o.tokenSourceFn(ctx, tokenPath, o.logger)
	if err != nil {
		return nil, fmt.Errorf("loading token %s: %w", tokenPath, err)
	}

	pair := &clientPair{
		Meta:     graph.NewClient(graph.DefaultBaseURL, o.cfg.MetaHTTP, ts, o.logger, o.cfg.UserAgent),
		Transfer: graph.NewClient(graph.DefaultBaseURL, o.cfg.TransferHTTP, ts, o.logger, o.cfg.UserAgent),
	}
	o.clients[tokenPath] = pair

	return pair, nil
}

// driveWork pairs a DriveRunner with the sync function it will execute.
type driveWork struct {
	runner *DriveRunner
	fn     func(context.Context) (*SyncReport, error)
}

// RunOnce executes a single sync cycle for all configured drives. Each drive
// runs in its own goroutine via a DriveRunner with panic recovery. RunOnce
// itself never returns an error — individual drive errors are captured in
// the per-drive DriveReport. The caller inspects each report to determine
// success or failure.
func (o *Orchestrator) RunOnce(ctx context.Context, mode SyncMode, opts RunOpts) ([]*DriveReport, error) {
	drives := o.cfg.Drives
	if len(drives) == 0 {
		return nil, nil
	}

	o.logger.Info("orchestrator starting RunOnce",
		slog.Int("drives", len(drives)),
		slog.String("mode", mode.String()),
	)

	work := o.prepareDriveWork(ctx, mode, opts)

	// Run all drives concurrently.
	reports := make([]*DriveReport, len(work))
	var wg gosync.WaitGroup

	for i, w := range work {
		wg.Add(1)

		go func(idx int, dw driveWork) {
			defer wg.Done()
			reports[idx] = dw.runner.run(ctx, dw.fn)
		}(i, w)
	}

	wg.Wait()

	o.logger.Info("orchestrator RunOnce complete", slog.Int("reports", len(reports)))

	return reports, nil
}

// prepareDriveWork resolves tokens, creates clients, and builds engines for
// each configured drive. Errors are captured as closures that return the error
// when the DriveRunner executes — no early abort for individual drive failures.
func (o *Orchestrator) prepareDriveWork(ctx context.Context, mode SyncMode, opts RunOpts) []driveWork {
	drives := o.cfg.Drives
	work := make([]driveWork, 0, len(drives))

	for _, rd := range drives {
		tokenPath := config.DriveTokenPath(rd.CanonicalID, o.cfg.Config)

		if tokenPath == "" {
			capturedCID := rd.CanonicalID
			work = append(work, driveWork{
				runner: &DriveRunner{canonID: rd.CanonicalID, displayName: rd.DisplayName},
				fn: func(_ context.Context) (*SyncReport, error) {
					return nil, fmt.Errorf("cannot determine token path for drive %s", capturedCID)
				},
			})

			continue
		}

		clients, err := o.getOrCreateClient(ctx, tokenPath)
		if err != nil {
			capturedErr := err
			capturedCID := rd.CanonicalID

			work = append(work, driveWork{
				runner: &DriveRunner{canonID: rd.CanonicalID, displayName: rd.DisplayName},
				fn: func(_ context.Context) (*SyncReport, error) {
					return nil, fmt.Errorf("token error for drive %s: %w", capturedCID, capturedErr)
				},
			})

			continue
		}

		// Discover drive ID if not configured (e.g., zero-config mode).
		driveID := rd.DriveID
		if driveID.IsZero() {
			discovered, discoverErr := o.driveDiscoveryFn(ctx, clients.Meta)
			if discoverErr != nil {
				capturedErr := discoverErr
				capturedCID := rd.CanonicalID

				work = append(work, driveWork{
					runner: &DriveRunner{canonID: rd.CanonicalID, displayName: rd.DisplayName},
					fn: func(_ context.Context) (*SyncReport, error) {
						return nil, fmt.Errorf("discovering drive for %s: %w", capturedCID, capturedErr)
					},
				})

				continue
			}

			driveID = discovered
			o.logger.Debug("discovered primary drive",
				slog.String("drive_id", driveID.String()),
				slog.String("canonical_id", rd.CanonicalID.String()),
			)
		}

		w := o.buildEngineWork(rd, driveID, clients, mode, opts)
		work = append(work, w)
	}

	return work
}

// buildEngineWork creates a driveWork item for a successfully-resolved drive.
// If engine creation fails, the error is captured and reported at run time.
// The driveID parameter is the resolved drive ID (either from config or discovered).
func (o *Orchestrator) buildEngineWork(
	rd *config.ResolvedDrive, driveID driveid.ID, clients *clientPair, mode SyncMode, opts RunOpts,
) driveWork {
	engine, engineErr := o.engineFactory(&EngineConfig{
		DBPath:        rd.StatePath(),
		SyncRoot:      rd.SyncDir,
		DataDir:       config.DefaultDataDir(),
		DriveID:       driveID,
		Fetcher:       clients.Meta,
		Items:         clients.Meta,
		Downloads:     clients.Transfer,
		Uploads:       clients.Transfer,
		DriveVerifier: clients.Meta,
		Logger:        o.logger,
		UseLocalTrash: rd.UseLocalTrash,
	})
	if engineErr != nil {
		capturedErr := engineErr

		return driveWork{
			runner: &DriveRunner{canonID: rd.CanonicalID, displayName: rd.DisplayName},
			fn: func(_ context.Context) (*SyncReport, error) {
				return nil, capturedErr
			},
		}
	}

	return driveWork{
		runner: &DriveRunner{canonID: rd.CanonicalID, displayName: rd.DisplayName},
		fn: func(c context.Context) (*SyncReport, error) {
			defer engine.Close()
			return engine.RunOnce(c, mode, opts)
		},
	}
}
