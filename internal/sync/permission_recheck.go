package sync

import (
	"context"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// recheckPermissions rechecks persisted remote write-denial scopes at the
// start of each sync pass. Read-denial scopes are observation-owned and clear
// only through observation reconciliation.
func (ph *PermissionHandler) recheckPermissions(
	ctx context.Context,
	bl *Baseline,
) []PermissionRecheckDecision {
	return ph.recheckPermissionsForScopeKeys(ctx, bl, nil)
}

func (ph *PermissionHandler) recheckPermissionsForScopeKeys(
	ctx context.Context,
	bl *Baseline,
	scopeFilter map[ScopeKey]bool,
) []PermissionRecheckDecision {
	if ph.permChecker == nil {
		return nil
	}

	blocks, err := ph.store.ListBlockScopes(ctx)
	if err != nil || len(blocks) == 0 {
		return nil
	}

	var decisions []PermissionRecheckDecision
	seen := make(map[ScopeKey]bool, len(blocks))

	for i := range blocks {
		block := blocks[i]
		if block == nil || !block.Key.IsPermRemoteWrite() {
			continue
		}
		if seen[block.Key] {
			continue
		}
		if len(scopeFilter) > 0 && !scopeFilter[block.Key] {
			continue
		}
		seen[block.Key] = true
		decisions = append(decisions, ph.recheckRemotePermissionBlock(ctx, bl, block))
	}

	return decisions
}

func (ph *PermissionHandler) recheckRemotePermissionBlock(
	ctx context.Context,
	bl *Baseline,
	block *BlockScope,
) PermissionRecheckDecision {
	boundaryPath := block.Key.RemotePath()

	root := ph.permissionRoot()
	if root == nil {
		return PermissionRecheckDecision{
			Kind:     permissionRecheckKeepScope,
			Path:     boundaryPath,
			ScopeKey: block.Key,
			Reason:   "configured remote root unavailable; keeping remote permission scope until probe succeeds",
		}
	}

	remoteDriveID := driveid.New(root.remoteDrive)
	remoteItemID := resolveBoundaryRemoteItemID(bl, boundaryPath, remoteDriveID, root)
	if remoteItemID == "" {
		return PermissionRecheckDecision{
			Kind:     permissionRecheckKeepScope,
			Path:     boundaryPath,
			ScopeKey: block.Key,
			Reason:   "remote permission boundary not resolvable yet; keeping scope",
		}
	}

	perms, permErr := ph.permChecker.ListItemPermissions(ctx, remoteDriveID, remoteItemID)
	if permErr != nil {
		return PermissionRecheckDecision{
			Kind:     permissionRecheckKeepScope,
			Path:     boundaryPath,
			ScopeKey: block.Key,
			Reason:   "permission recheck failed; keeping remote permission scope",
		}
	}

	switch graph.EvaluateWriteAccess(perms, ph.accountEmail) {
	case graph.PermissionWriteAccessWritable:
		return PermissionRecheckDecision{
			Kind:     permissionRecheckReleaseScope,
			Path:     boundaryPath,
			ScopeKey: block.Key,
			Reason:   "remote write permission granted; releasing remote permission scope",
		}
	case graph.PermissionWriteAccessInconclusive:
		return PermissionRecheckDecision{
			Kind:     permissionRecheckKeepScope,
			Path:     boundaryPath,
			ScopeKey: block.Key,
			Reason:   "permission recheck inconclusive; keeping remote permission scope",
		}
	case graph.PermissionWriteAccessReadOnly:
		return PermissionRecheckDecision{
			Kind:     permissionRecheckKeepScope,
			Path:     boundaryPath,
			ScopeKey: block.Key,
			Reason:   "remote permission boundary still denied",
		}
	}

	return PermissionRecheckDecision{
		Kind:     permissionRecheckKeepScope,
		Path:     boundaryPath,
		ScopeKey: block.Key,
		Reason:   "permission recheck inconclusive; keeping remote permission scope",
	}
}

// recheckLocalPermissions revalidates persisted local permission scopes. Local
// write scopes clear on affirmative write-probe success. Local read scopes may
// also clear on direct accessibility revalidation because that check is part of
// observation.
func (ph *PermissionHandler) recheckLocalPermissions(ctx context.Context) []PermissionRecheckDecision {
	blocks, err := ph.store.ListBlockScopes(ctx)
	if err != nil || len(blocks) == 0 {
		return nil
	}

	var decisions []PermissionRecheckDecision

	for i := range blocks {
		block := blocks[i]
		if block == nil || !block.Key.IsPermDir() {
			continue
		}

		dirPath := block.Key.DirPath()
		clearable := false
		switch {
		case block.Key.IsPermLocalRead():
			clearable = isDirAccessible(ph.syncTree, dirPath)
		case block.Key.IsPermLocalWrite():
			clearable = isDirWritable(ph.syncTree, dirPath)
		}
		if !clearable {
			decisions = append(decisions, PermissionRecheckDecision{
				Kind:     permissionRecheckKeepScope,
				Path:     dirPath,
				ScopeKey: block.Key,
				Reason:   "local permission denial still active",
			})
			continue
		}

		decisions = append(decisions, PermissionRecheckDecision{
			Kind:     permissionRecheckReleaseScope,
			Path:     dirPath,
			ScopeKey: block.Key,
			Reason:   "local permission restored, clearing denial",
		})
	}

	return decisions
}
