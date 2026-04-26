package multisync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/config"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type releasedShortcutChild struct {
	mountID       string
	namespaceID   string
	bindingItemID string
	reason        syncengine.ShortcutChildReleaseReason
}

func releasedChildren(
	topologies map[mountID]syncengine.ShortcutChildTopologyPublication,
) []releasedShortcutChild {
	if len(topologies) == 0 {
		return nil
	}

	releases := make([]releasedShortcutChild, 0)
	for parentID, publication := range topologies {
		namespaceID := parentID.String()
		if publication.NamespaceID != "" {
			namespaceID = publication.NamespaceID
		}
		for i := range publication.Released {
			release := publication.Released[i]
			if release.BindingItemID == "" {
				continue
			}
			reason := release.Reason
			if reason == "" {
				reason = syncengine.ShortcutChildReleaseParentRemoved
			}
			releases = append(releases, releasedShortcutChild{
				mountID:       config.ChildMountID(namespaceID, release.BindingItemID),
				namespaceID:   namespaceID,
				bindingItemID: release.BindingItemID,
				reason:        reason,
			})
		}
	}
	sort.Slice(releases, func(i, j int) bool {
		if releases[i].mountID == releases[j].mountID {
			return releases[i].bindingItemID < releases[j].bindingItemID
		}
		return releases[i].mountID < releases[j].mountID
	})
	return releases
}

func (o *Orchestrator) purgeReleasedShortcutChildArtifactsForCompiled(
	ctx context.Context,
	compiled *compiledMountSet,
) error {
	if compiled == nil || len(compiled.ReleasedChildren) == 0 {
		return nil
	}
	var errs []error
	purged := make([]releasedShortcutChild, 0, len(compiled.ReleasedChildren))
	for _, release := range compiled.ReleasedChildren {
		if err := purgeShortcutChildArtifacts(ctx, release.mountID, o.logger); err != nil {
			errs = append(errs, fmt.Errorf("purging released child mount %s: %w", release.mountID, err))
			continue
		}
		purged = append(purged, release)
	}
	if len(purged) > 0 {
		o.forgetReleasedShortcutChildren(purged)
	}
	return errors.Join(errs...)
}

func purgeShortcutChildArtifacts(ctx context.Context, childMountID string, logger *slog.Logger) error {
	if strings.TrimSpace(childMountID) == "" {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return fmt.Errorf("purging shortcut child artifacts: %w", ctx.Err())
	}

	if err := config.PurgeManagedChildMountArtifacts(childMountID); err != nil {
		return fmt.Errorf("purging managed child mount artifacts: %w", err)
	}

	if logger != nil {
		logger.Info("purged shortcut child state artifacts",
			slog.String("mount_id", childMountID),
		)
	}
	return nil
}
