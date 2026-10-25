package cli

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
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

// Validates: R-6.6.31
func TestFormatWatchStartupError_ExitStatusFollowsSharedCause(t *testing.T) {
	t.Parallel()

	signIn := fmt.Errorf("session: %w", graph.ErrNotLoggedIn)
	badConfig := fmt.Errorf("validate sync config: %w", config.ErrInvalidConfig)
	transient := fmt.Errorf("list: %w", graph.ErrServerError)
	startupErr := func(causes ...error) error {
		results := make([]multisync.MountStartupResult, 0, len(causes))
		for i, cause := range causes {
			results = append(results, multisync.MountStartupResult{
				SelectionIndex: i,
				DisplayName:    fmt.Sprintf("Drive %d", i),
				Status:         multisync.MountStartupFatal,
				Err:            cause,
			})
		}
		return &multisync.WatchStartupError{Summary: multisync.StartupSelectionSummary{Results: results}}
	}

	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "one drive needs sign-in", err: startupErr(signIn), want: exitSignInRequired},
		{name: "every drive needs sign-in", err: startupErr(signIn, signIn), want: exitSignInRequired},
		{name: "one drive has bad config", err: startupErr(badConfig), want: exitConfigInvalid},
		{name: "every drive has bad config", err: startupErr(badConfig, badConfig), want: exitConfigInvalid},
		{name: "sign-in plus transient may recover", err: startupErr(signIn, transient), want: exitFailure},
		{name: "sign-in plus config is mixed", err: startupErr(signIn, badConfig), want: exitFailure},
		{name: "transient", err: startupErr(transient), want: exitFailure},
	}

	for _, tt := range tests {
		formatted := formatWatchStartupError(tt.err)
		assert.Equal(t, tt.want, commandExitCode(formatted, false), tt.name)
		assert.NotContains(t, formatted.Error(), "%!", tt.name)
	}

	assert.Equal(t, "Drive 0 could not start syncing: session: graph: not logged in",
		startupErr(signIn).Error(), "the multisync summary text is unchanged")
	assert.Equal(t, "session: graph: not logged in", formatWatchStartupError(startupErr(signIn)).Error())
}
