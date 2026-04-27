package multisync

import (
	"context"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// shortcutChildAckHandle is the only parent-ack capability multisync keeps.
// Production values come from a live parent engine through engineRunnerAdapter;
// multisync tests may inject fakes without constructing sync's concrete handle.
type shortcutChildAckHandle interface {
	IsZero() bool
	AcknowledgeChildFinalDrain(context.Context, syncengine.ShortcutChildDrainAck) (syncengine.ShortcutChildRunnerPublication, error)
	AcknowledgeChildArtifactsPurged(
		context.Context,
		syncengine.ShortcutChildArtifactCleanupAck,
	) (syncengine.ShortcutChildRunnerPublication, error)
}

func shortcutChildAckHandleIsZero(handle shortcutChildAckHandle) bool {
	return handle == nil || handle.IsZero()
}

func cloneParentAckHandles(
	ackersByParent map[mountID]shortcutChildAckHandle,
) map[mountID]shortcutChildAckHandle {
	ackers := make(map[mountID]shortcutChildAckHandle, len(ackersByParent))
	for id, acker := range ackersByParent {
		ackers[id] = acker
	}
	return ackers
}
