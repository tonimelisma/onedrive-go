package sync

import (
	"context"
	"errors"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
)

// EngineMountConfig carries the non-client runtime facts needed to construct an
// engine for one mounted content root.
type EngineMountConfig struct {
	MountID string
	DBPath  string
	// TrashDir is the mount's trash directory: a configured drive's own
	// (localtrash.DirForStatePath of DBPath, the default when empty), or, for
	// a shortcut child, the child trash under its parent drive's
	// (localtrash.ChildDir), which the parent's trash commands reach.
	TrashDir                 string
	SyncRoot                 string
	DataDir                  string
	DriveID                  driveid.ID
	DriveType                string
	AccountEmail             string
	RemoteRootItemID         string
	RemoteRootDeltaCapable   bool
	ExpectedSyncRootIdentity *syncshortcut.ShortcutRootIdentity
	EnableWebsocket          bool
	ContentFilter            syncdomain.ContentFilterConfig
	LocalRules               syncdomain.LocalObservationRules
	ShortcutNamespaceID      string
	ShortcutChildWorkSink    syncshortcut.ShortcutChildWorkSink
	TransferWorkers          int
	CheckWorkers             int
	MinFreeSpace             int64
	// AccountCanonicalID is the account whose sign-in the engine syncs with,
	// the owner of its token. Its catalog record carries the sign-in
	// requirement the engine marks, checks at startup and clears.
	AccountCanonicalID driveid.CanonicalID
}

// NewMountEngine constructs an Engine directly from the authenticated session
// capabilities plus mount-owned runtime config.
func NewMountEngine(
	ctx context.Context,
	session *driveops.Session,
	mountCfg *EngineMountConfig,
	logger *slog.Logger,
	perfCollector *perf.Collector,
	verifyDrive bool,
) (*Engine, error) {
	if session == nil {
		return nil, errors.New("sync: session is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if mountCfg == nil {
		return nil, errors.New("sync: mount config is required")
	}
	if mountCfg.SyncRoot == "" {
		return nil, errors.New("sync: sync root is required")
	}
	if mountCfg.DBPath == "" {
		return nil, errors.New("sync: state DB path is required")
	}
	if mountCfg.DriveID.IsZero() {
		return nil, errors.New("sync: drive ID is required")
	}

	pathConvergence := driveops.NewMountSession(session, mountCfg.RemoteRootItemID)

	cfg := &engineInputs{
		MountID:                  mountCfg.MountID,
		DBPath:                   mountCfg.DBPath,
		TrashDir:                 mountCfg.TrashDir,
		SyncRoot:                 mountCfg.SyncRoot,
		DataDir:                  mountCfg.DataDir,
		DriveID:                  mountCfg.DriveID,
		DriveType:                mountCfg.DriveType,
		AccountEmail:             mountCfg.AccountEmail,
		AccountCanonicalID:       mountCfg.AccountCanonicalID,
		RemoteRootItemID:         mountCfg.RemoteRootItemID,
		RemoteRootDeltaCapable:   mountCfg.RemoteRootDeltaCapable,
		ExpectedSyncRootIdentity: syncshortcut.ShortcutRootIdentityToFileIdentity(mountCfg.ExpectedSyncRootIdentity),
		Fetcher:                  session.Meta,
		SocketIOFetcher:          session.Meta,
		Items:                    session.Meta,
		Downloads:                session.Transfer,
		Uploads:                  session.Transfer,
		PathConvergence:          pathConvergence,
		FolderDelta:              session.Meta,
		RecursiveLister:          session.Meta,
		PermChecker:              session.Meta,
		Logger:                   logger,
		EnableWebsocket:          mountCfg.EnableWebsocket,
		ContentFilter:            mountCfg.ContentFilter,
		LocalRules:               mountCfg.LocalRules,
		ShortcutNamespaceID:      mountCfg.ShortcutNamespaceID,
		ShortcutChildWorkSink:    mountCfg.ShortcutChildWorkSink,
		TransferWorkers:          mountCfg.TransferWorkers,
		CheckWorkers:             mountCfg.CheckWorkers,
		MinFreeSpace:             mountCfg.MinFreeSpace,
		PerfCollector:            perfCollector,
	}

	if verifyDrive {
		cfg.DriveVerifier = session.Meta
	}

	return newEngine(ctx, cfg)
}
