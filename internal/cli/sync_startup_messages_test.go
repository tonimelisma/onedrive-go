package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func TestShellQuoteArg_EscapesSingleQuotes(t *testing.T) {
	t.Parallel()

	assert.Equal(t, `'owner'"'"'s drive'`, shellQuoteArg("owner's drive"))
}

func TestSyncStateResetCommand_QuotesCanonicalID(t *testing.T) {
	t.Parallel()

	cid := driveid.MustCanonicalID("personal:user@example.com")
	assert.Equal(t,
		"onedrive-go drive reset-sync-state --drive 'personal:user@example.com'",
		syncStateResetCommand(cid),
	)
}

func TestFormatStartupResultMessage_StandaloneIncompatibleStoreUsesDriveCommands(t *testing.T) {
	t.Parallel()

	cid := driveid.MustCanonicalID("personal:user@example.com")
	message := formatStartupResultMessage(&multisync.MountStartupResult{
		Identity: testStandaloneMountIdentity(cid),
		Status:   multisync.MountStartupIncompatibleStore,
		Err: &syncengine.StateStoreIncompatibleError{
			Reason: syncengine.StateStoreIncompatibleReasonIncompatibleSchema,
		},
	})

	assert.Contains(t, message, "onedrive-go pause --drive 'personal:user@example.com'")
	assert.Contains(t, message, "onedrive-go drive reset-sync-state --drive 'personal:user@example.com'")
}

func TestFormatStartupResultMessage_ChildIncompatibleStoreUsesMountStatePath(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	message := formatStartupResultMessage(&multisync.MountStartupResult{
		Identity: multisync.MountIdentity{
			MountID:        "child-docs",
			ParentMountID:  "personal:owner@example.com",
			ProjectionKind: multisync.MountProjectionChild,
		},
		Status: multisync.MountStartupIncompatibleStore,
		Err: &syncengine.StateStoreIncompatibleError{
			Reason: syncengine.StateStoreIncompatibleReasonIncompatibleSchema,
		},
	})

	assert.Contains(t, message, "child-docs")
	assert.Contains(t, message, config.MountStatePath("child-docs"))
	assert.NotContains(t, message, "--drive")
	assert.NotContains(t, message, "drive reset-sync-state")
}
