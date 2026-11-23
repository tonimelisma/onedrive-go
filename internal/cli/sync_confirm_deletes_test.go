package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

// confirmTestHeld is the held set the confirmation tests record.
func confirmTestHeld() []syncdomain.HeldDelete {
	return []syncdomain.HeldDelete{
		{Path: "Docs/a.txt", Side: syncdomain.DeleteSafetySideRemote, Reason: syncdomain.DeleteSafetyReasonTooMany, ItemType: syncdomain.ItemTypeFile, ItemID: "item-a"},
		{Path: "Docs/b.txt", Side: syncdomain.DeleteSafetySideRemote, Reason: syncdomain.DeleteSafetyReasonTooMany, ItemType: syncdomain.ItemTypeFile, ItemID: "item-b"},
	}
}

// seedHeldDeletesForDrive records held as the drive's held set in its state
// store and returns the drive.
func seedHeldDeletesForDrive(t *testing.T, cid driveid.CanonicalID, held []syncdomain.HeldDelete) *config.ResolvedDrive {
	t.Helper()

	statePath := config.DriveStatePath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
	store, err := syncstore.NewSyncStore(t.Context(), statePath, testDriveLogger(t))
	require.NoError(t, err)
	require.NoError(t, store.ReplaceHeldDeletes(t.Context(), held))
	require.NoError(t, store.Close(t.Context()))

	return &config.ResolvedDrive{CanonicalID: cid}
}

// confirmRecorder is a watch owner that answers confirmations as answer
// says and keeps the requests it was sent.
type confirmRecorder struct {
	mu       sync.Mutex
	requests []synccontrol.ConfirmDeletesRequest
}

func (c *confirmRecorder) handler(t *testing.T, status int, answer synccontrol.MutationResponse) controlTestHandler {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		if !assert.Equal(t, synccontrol.PathConfirmDeletes, r.URL.Path) {
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
		}
		var request synccontrol.ConfirmDeletesRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		c.mu.Lock()
		c.requests = append(c.requests, request)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		assert.NoError(t, json.NewEncoder(w).Encode(answer))
	}
}

func (c *confirmRecorder) sent() []synccontrol.ConfirmDeletesRequest {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]synccontrol.ConfirmDeletesRequest(nil), c.requests...)
}

// probeTestWatchOwner is the client of the test socket's watch owner.
func probeTestWatchOwner(t *testing.T) *controlSocketClient {
	t.Helper()

	probe, err := probeControlOwner(t.Context())
	require.NoError(t, err)
	require.Equal(t, controlOwnerStateWatchOwner, probe.state)

	return probe.client
}

// Without a held set named, `sync --confirm-deletes` against a running watch
// shows each drive's held set and the command that confirms exactly it, and
// confirms nothing.
//
// Validates: R-6.2.17
func TestConfirmDeletesToRunningWatch_ShowsTheHeldSetAndConfirmsNothing(t *testing.T) {
	setTestDriveHome(t)
	startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch}, refuseControlMutation(t))
	cid := driveid.MustCanonicalID("personal:confirm@example.com")
	drive := seedHeldDeletesForDrive(t, cid, confirmTestHeld())
	set := syncdomain.ConfirmationOf(confirmTestHeld())

	var out bytes.Buffer
	cc := &CLIContext{Logger: testDriveLogger(t), OutputWriter: &out, StatusWriter: &out}
	err := confirmDeletesToRunningWatch(t.Context(), cc, probeTestWatchOwner(t), []*config.ResolvedDrive{drive}, "")

	require.ErrorIs(t, err, errHeldDeletesNotConfirmed)
	assert.Contains(t, out.String(), cid.String()+" holds 2 deletes for confirmation:\n")
	assert.Contains(t, out.String(), "  Docs/a.txt (delete on OneDrive)\n")
	assert.Contains(t, out.String(), "  onedrive-go sync --confirm-deletes --held-set "+set.String()+"\n")
}

// The named set reaches the running watch with the drive it belongs to; the
// watch's answer says the next pass applies it.
//
// Validates: R-6.2.17, R-2.9.2
func TestConfirmDeletesToRunningWatch_SendsTheNamedSetToTheWatch(t *testing.T) {
	setTestDriveHome(t)
	recorder := &confirmRecorder{}
	startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch},
		recorder.handler(t, http.StatusOK, synccontrol.MutationResponse{Status: synccontrol.StatusDeletesConfirmed}))
	cid := driveid.MustCanonicalID("personal:confirm@example.com")
	drive := seedHeldDeletesForDrive(t, cid, confirmTestHeld())
	set := syncdomain.ConfirmationOf(confirmTestHeld())

	var out bytes.Buffer
	cc := &CLIContext{Logger: testDriveLogger(t), OutputWriter: &out, StatusWriter: &out}
	require.NoError(t, confirmDeletesToRunningWatch(t.Context(), cc, probeTestWatchOwner(t),
		[]*config.ResolvedDrive{drive}, set.String()))

	assert.Equal(t, []synccontrol.ConfirmDeletesRequest{{Drive: cid.String(), Count: 2, Token: set.Token}}, recorder.sent())
	assert.Equal(t, "The running 'sync --watch' applies the 2 confirmed deletes of "+cid.String()+" in its next pass.\n",
		out.String())
}

// A named set that is not the one held now is refused before it is sent;
// one the watch refuses because its set changed in between is refused with
// the watch's reason. Neither confirms anything.
//
// Validates: R-6.2.17
func TestConfirmDeletesToRunningWatch_RefusesASetThatChanged(t *testing.T) {
	t.Run("changed before it was sent", func(t *testing.T) {
		setTestDriveHome(t)
		startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch}, refuseControlMutation(t))
		cid := driveid.MustCanonicalID("personal:confirm@example.com")
		drive := seedHeldDeletesForDrive(t, cid, confirmTestHeld())
		shown := syncdomain.ConfirmationOf(confirmTestHeld()[:1])

		var out bytes.Buffer
		cc := &CLIContext{Logger: testDriveLogger(t), OutputWriter: &out, StatusWriter: &out}
		err := confirmDeletesToRunningWatch(t.Context(), cc, probeTestWatchOwner(t), []*config.ResolvedDrive{drive}, shown.String())

		require.Error(t, err)
		assert.Contains(t, err.Error(), "the held deletes changed since they were shown; nothing was confirmed")
		assert.Contains(t, out.String(), "--held-set "+syncdomain.ConfirmationOf(confirmTestHeld()).String(),
			"the set held now is shown with its own command")
	})

	t.Run("changed in the watch before it took it", func(t *testing.T) {
		setTestDriveHome(t)
		recorder := &confirmRecorder{}
		startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch},
			recorder.handler(t, http.StatusConflict, synccontrol.MutationResponse{
				Status:  synccontrol.StatusError,
				Code:    synccontrol.ErrorHeldDeletesChanged,
				Message: "the held deletes changed since they were shown: 2 were shown, 3 are held now",
			}))
		cid := driveid.MustCanonicalID("personal:confirm@example.com")
		drive := seedHeldDeletesForDrive(t, cid, confirmTestHeld())
		set := syncdomain.ConfirmationOf(confirmTestHeld())

		var out bytes.Buffer
		cc := &CLIContext{Logger: testDriveLogger(t), OutputWriter: &out, StatusWriter: &out}
		err := confirmDeletesToRunningWatch(t.Context(), cc, probeTestWatchOwner(t), []*config.ResolvedDrive{drive}, set.String())

		require.Error(t, err)
		assert.Contains(t, err.Error(), "2 were shown, 3 are held now; nothing was confirmed")
		assert.Len(t, recorder.sent(), 1)
		assert.Empty(t, out.String(), "nothing reads as confirmed")
	})
}

// --held-set names a set for a running watch; it needs --confirm-deletes and
// cannot go with --watch.
//
// Validates: R-6.2.17
func TestNewSyncCmd_HeldSetIsNotForAWatchSession(t *testing.T) {
	t.Parallel()

	cmd := newSyncCmd()
	require.NotNil(t, cmd.Flags().Lookup(heldSetFlag))
	require.NoError(t, cmd.Flags().Set(heldSetFlag, "2-0123456789abcdef"))
	require.NoError(t, cmd.Flags().Set("watch", "true"))

	err := cmd.ValidateFlagGroups()
	require.Error(t, err)
	assert.Contains(t, err.Error(), heldSetFlag)
}
