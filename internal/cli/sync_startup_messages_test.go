package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
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
		Err: &syncstore.StateStoreIncompatibleError{
			Reason: syncstore.StateStoreIncompatibleReasonIncompatibleSchema,
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
		Err: &syncstore.StateStoreIncompatibleError{
			Reason: syncstore.StateStoreIncompatibleReasonIncompatibleSchema,
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

// A panic the error carries survives the startup error's presentation: a
// watch whose every drive needs sign-in while its keepalive panicked still
// answers to panicguard.ErrPanic with its stack, says which drive could not
// start, and exits 1, not 77.
//
// Validates: R-6.3.10, R-6.6.31
func TestFormatWatchStartupError_KeepsAPanicTheErrorCarries(t *testing.T) {
	t.Parallel()

	signIn := fmt.Errorf("session: %w", graph.ErrNotLoggedIn)
	startupErr := &multisync.WatchStartupError{Summary: multisync.StartupSelectionSummary{
		Results: []multisync.MountStartupResult{{
			SelectionIndex: 0,
			DisplayName:    "Drive 0",
			Status:         multisync.MountStartupFatal,
			Err:            signIn,
		}},
	}}
	recovered := testPanicError(t)
	require.Equal(t, exitSignInRequired, commandExitCode(formatWatchStartupError(startupErr), false),
		"without the panic, the drive's sign-in cause sets the status")

	formatted := formatWatchStartupError(errors.Join(recovered, startupErr))

	require.ErrorIs(t, formatted, panicguard.ErrPanic)
	assert.Equal(t, panicguard.Stack(recovered), panicguard.Stack(formatted), "the panic's stack is kept")
	assert.NotEmpty(t, panicguard.Stack(formatted))
	assert.Contains(t, formatted.Error(), "boom", "the panic's message is kept")
	assert.Contains(t, formatted.Error(), "not logged in", "the presented startup message stays")
	assert.Equal(t, exitFailure, commandExitCode(formatted, false))
}
