package syncstore

import (
	"context"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const (
	ResolutionUnresolved = synctypes.ResolutionUnresolved
	ResolutionKeepLocal  = synctypes.ResolutionKeepLocal
	ResolutionKeepRemote = synctypes.ResolutionKeepRemote
	ResolutionKeepBoth   = synctypes.ResolutionKeepBoth

	ConflictStateUnresolved = synctypes.ConflictStateUnresolved
	ConflictStateQueued     = synctypes.ConflictStateQueued
	ConflictStateApplying   = synctypes.ConflictStateApplying
)

type IssueSummaryDescriptor struct {
	Title  string
	Reason string
	Action string
}

func DescribeIssueSummary(key synctypes.SummaryKey) IssueSummaryDescriptor {
	descriptor := synctypes.DescribeSummary(key)
	return IssueSummaryDescriptor{
		Title:  descriptor.Title,
		Reason: descriptor.Reason,
		Action: descriptor.Action,
	}
}

func StatusScopeKind(scopeKey synctypes.ScopeKey) string {
	if scopeKey.IsZero() {
		return ""
	}

	switch scopeKey.Kind {
	case synctypes.ScopeAuthAccount, synctypes.ScopeThrottleAccount:
		return statusScopeAccount
	case synctypes.ScopeThrottleTarget:
		if scopeKey.IsThrottleShared() {
			return statusScopeShortcut
		}
		return statusScopeDrive
	case synctypes.ScopeService:
		return statusScopeService
	case synctypes.ScopeQuotaOwn:
		return statusScopeDrive
	case synctypes.ScopeQuotaShortcut, synctypes.ScopePermRemote:
		return statusScopeShortcut
	case synctypes.ScopePermDir:
		return statusScopeDirectory
	case synctypes.ScopeDiskLocal:
		return statusScopeDisk
	default:
		return statusScopeFile
	}
}

func AuthAccountScopeKey() synctypes.ScopeKey {
	return synctypes.SKAuthAccount()
}

func HasAccountAuthScopeAtPath(ctx context.Context, dbPath string, logger *slog.Logger) (bool, error) {
	return HasScopeBlockAtPath(ctx, dbPath, AuthAccountScopeKey(), logger)
}

func ScopeBlocksContainAuth(blocks []*ScopeBlock) bool {
	for _, block := range blocks {
		if block != nil && block.Key == AuthAccountScopeKey() {
			return true
		}
	}

	return false
}
