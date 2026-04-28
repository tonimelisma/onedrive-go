package multisync

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// Validates: R-2.4.8
func TestClassifyShortcutChildDrainResultsOnlyCleanIsAckable(t *testing.T) {
	t.Parallel()

	mounts := []*mountSpec{
		{mountID: "clean", child: &childMountRuntime{
			mode:   syncengine.ShortcutChildRunModeFinalDrain,
			ackRef: syncengine.NewShortcutChildAckRef("binding-clean"),
		}},
		{mountID: "failed", child: &childMountRuntime{
			mode:   syncengine.ShortcutChildRunModeFinalDrain,
			ackRef: syncengine.NewShortcutChildAckRef("binding-failed"),
		}},
		{mountID: "missing", child: &childMountRuntime{
			mode:   syncengine.ShortcutChildRunModeFinalDrain,
			ackRef: syncengine.NewShortcutChildAckRef("binding-missing"),
		}},
		{mountID: "root-missing", child: &childMountRuntime{
			mode:   syncengine.ShortcutChildRunModeFinalDrain,
			ackRef: syncengine.NewShortcutChildAckRef("binding-root"),
		}},
	}
	reports := []*MountReport{
		{
			Identity: MountIdentity{MountID: "clean"},
			Report:   &syncengine.Report{},
		},
		{
			Identity: MountIdentity{MountID: "failed"},
			Report: &syncengine.Report{
				Failed: 1,
				Errors: []error{
					fmt.Errorf("upload blocked"),
				},
			},
		},
		{
			Identity: MountIdentity{MountID: "root-missing"},
			Err:      fmt.Errorf("opening child root: %w", syncengine.ErrMountRootUnavailable),
		},
	}

	results := classifyShortcutChildDrainResults(
		mounts,
		reports,
	)

	require.Len(t, results, 4)
	byMount := make(map[string]shortcutChildDrainResult, len(results))
	for _, result := range results {
		byMount[result.MountID] = result
	}
	assert.Equal(t, shortcutChildDrainClean, byMount["clean"].Status)
	assert.Equal(t, shortcutChildDrainBlocked, byMount["failed"].Status)
	assert.Equal(t, shortcutChildDrainFailed, byMount["missing"].Status)
	assert.Equal(t, shortcutChildDrainRootUnavailable, byMount["root-missing"].Status)

	clean := cleanShortcutChildDrainResults(results)
	require.Len(t, clean, 1)
	assert.Equal(t, "clean", clean[0].MountID)
	assert.False(t, clean[0].AckRef.IsZero())
}

// Validates: R-2.4.8
func TestAcknowledgeSuccessfulFinalDrainsRequiresLiveParentAck(t *testing.T) {
	t.Parallel()

	_, err := acknowledgeSuccessfulFinalDrains(
		t.Context(),
		[]shortcutChildDrainResult{{
			MountID: "child",
			AckRef:  syncengine.NewShortcutChildAckRef("binding-clean"),
			Status:  shortcutChildDrainClean,
		}},
		[]*mountSpec{{
			mountID: "child",
			child: &childMountRuntime{
				parentMountID: "parent",
				mode:          syncengine.ShortcutChildRunModeFinalDrain,
				ackRef:        syncengine.NewShortcutChildAckRef("binding-clean"),
			},
		}},
		nil,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parent mount parent is unavailable for final-drain acknowledgement")
}
