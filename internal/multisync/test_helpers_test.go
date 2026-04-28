package multisync

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func testCanonicalID(t *testing.T, s string) driveid.CanonicalID {
	t.Helper()

	cid, err := driveid.NewCanonicalID(s)
	require.NoError(t, err)

	return cid
}

func testStandaloneMountIdentity(cid driveid.CanonicalID) MountIdentity {
	return MountIdentity{
		MountID:        cid.String(),
		ProjectionKind: MountProjectionStandalone,
		CanonicalID:    cid,
	}
}

func testRunnerDecisionDataDir(t *testing.T) string {
	t.Helper()

	return t.TempDir()
}

func testParentMountSpec() *mountSpec {
	return &mountSpec{
		mountID:             mountID("personal:owner@example.com"),
		projectionKind:      MountProjectionStandalone,
		canonicalID:         driveid.MustCanonicalID("personal:owner@example.com"),
		tokenOwnerCanonical: driveid.MustCanonicalID("personal:owner@example.com"),
		remoteDriveID:       driveid.New("parent-drive"),
		syncRoot:            "/tmp/parent",
	}
}

func testChildRecord(_ mountID, bindingID, relativePath string) syncengine.ShortcutChildRunner {
	return runChildPublication(bindingID, relativePath)
}

func runChildPublication(bindingID, relativePath string) syncengine.ShortcutChildRunner {
	return syncengine.ShortcutChildRunner{
		BindingItemID:     bindingID,
		RelativeLocalPath: relativePath,
		DisplayName:       "Shortcut",
		RemoteDriveID:     "remote-drive",
		RemoteItemID:      "remote-root",
		RunnerAction:      syncengine.ShortcutChildActionRun,
	}
}

func finalDrainChildRecord(bindingID, relativePath string) syncengine.ShortcutChildRunner {
	return finalDrainChildPublication(bindingID, relativePath)
}

func finalDrainChildPublication(bindingID, relativePath string) syncengine.ShortcutChildRunner {
	return syncengine.ShortcutChildRunner{
		BindingItemID:     bindingID,
		RelativeLocalPath: relativePath,
		DisplayName:       "Shortcut",
		RemoteDriveID:     "remote-drive",
		RemoteItemID:      "remote-root",
		RunnerAction:      syncengine.ShortcutChildActionFinalDrain,
	}
}

func skipChildPublication(bindingID, relativePath string) syncengine.ShortcutChildRunner {
	child := runChildPublication(bindingID, relativePath)
	child.RunnerAction = syncengine.ShortcutChildActionSkipParentBlocked
	return child
}

func runnerPublication(namespaceID string, children ...syncengine.ShortcutChildRunner) syncengine.ShortcutChildRunnerPublication {
	return runnerPublicationForRoot(namespaceID, "/tmp/parent", children...)
}

func runnerPublicationForParent(
	parent *StandaloneMountConfig,
	children ...syncengine.ShortcutChildRunner,
) syncengine.ShortcutChildRunnerPublication {
	namespaceID := ""
	parentRoot := ""
	if parent != nil {
		namespaceID = parent.CanonicalID.String()
		parentRoot = parent.SyncRoot
	}
	return runnerPublicationForRoot(namespaceID, parentRoot, children...)
}

func runnerPublicationForRoot(
	namespaceID string,
	parentRoot string,
	children ...syncengine.ShortcutChildRunner,
) syncengine.ShortcutChildRunnerPublication {
	for i := range children {
		if children[i].BindingItemID != "" && children[i].ChildMountID == "" {
			children[i].ChildMountID = config.ChildMountID(namespaceID, children[i].BindingItemID)
		}
		if children[i].RelativeLocalPath != "" && children[i].LocalRoot == "" && parentRoot != "" {
			children[i].LocalRoot = filepath.Join(parentRoot, filepath.FromSlash(children[i].RelativeLocalPath))
		}
	}
	return syncengine.ShortcutChildRunnerPublication{
		NamespaceID: namespaceID,
		RunnerWork: syncengine.ShortcutChildRunnerWork{
			Children: children,
		},
	}
}

func cleanupRequestPublication(
	namespaceID string,
	requests ...syncengine.ShortcutChildArtifactCleanupRequest,
) syncengine.ShortcutChildRunnerPublication {
	return syncengine.ShortcutChildRunnerPublication{
		NamespaceID: namespaceID,
		CleanupWork: syncengine.ShortcutChildArtifactCleanupWork{
			Requests: requests,
		},
	}
}

func seedShortcutChildRunner(
	orch *Orchestrator,
	parent *StandaloneMountConfig,
	child *syncengine.ShortcutChildRunner,
) {
	if parent == nil || child == nil || child.BindingItemID == "" {
		return
	}
	orch.receiveParentRunnerPublication(
		mountID(parent.CanonicalID.String()),
		runnerPublicationForParent(parent, *child),
	)
}
