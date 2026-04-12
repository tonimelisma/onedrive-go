package sync

import (
	"context"
	"log/slog"
)

type IssueSummaryDescriptor struct {
	Title  string
	Reason string
	Action string
}

func DescribeIssueSummary(key SummaryKey) IssueSummaryDescriptor {
	descriptor := DescribeSummary(key)
	return IssueSummaryDescriptor{
		Title:  descriptor.Title,
		Reason: descriptor.Reason,
		Action: descriptor.Action,
	}
}

func StatusScopeKind(scopeKey ScopeKey) string {
	if scopeKey.IsZero() {
		return ""
	}

	switch scopeKey.Kind {
	case ScopeAuthAccount, ScopeThrottleAccount:
		return statusScopeAccount
	case ScopeThrottleTarget:
		if scopeKey.IsThrottleShared() {
			return statusScopeShortcut
		}
		return statusScopeDrive
	case ScopeService:
		return statusScopeService
	case ScopeQuotaOwn:
		return statusScopeDrive
	case ScopeQuotaShortcut, ScopePermRemote:
		return statusScopeShortcut
	case ScopePermDir:
		return statusScopeDirectory
	case ScopeDiskLocal:
		return statusScopeDisk
	default:
		return statusScopeFile
	}
}

func AuthAccountScopeKey() ScopeKey {
	return SKAuthAccount()
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
