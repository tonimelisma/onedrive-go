package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// These pin the shape of the --json documents the mutating commands print.
// TestCommandTree_EveryLeafHonorsJSON proves each one is valid JSON; these
// prove it says the right thing.

func newJSONCommandContext(cfgPath string) (*CLIContext, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	cc := newCommandContext(&stdout, cfgPath)
	cc.StatusWriter = &stderr
	cc.Flags.JSON = true

	return cc, &stdout, &stderr
}

// Validates: R-2.6, R-6.6.31
func TestPauseCommand_JSONReportsDriveAndEndTime(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:pause@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))

	cc, stdout, stderr := newJSONCommandContext(cfgPath)
	now := time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)
	require.NoError(t, runPauseCommand(t.Context(), cc, func() time.Time { return now }, []string{"2h"}))

	var got pauseJSONOutput
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, pauseJSONOutput{Drive: cid.String(), Paused: true, PausedUntil: "2026-04-02T14:00:00Z"}, got)
	assert.NotContains(t, stderr.String(), "Paused ", "the JSON document replaces the prose confirmation")

	// Without a duration there is no end time.
	stdout.Reset()
	require.NoError(t, runPauseCommand(t.Context(), cc, time.Now, nil))
	assert.NotContains(t, stdout.String(), "paused_until")
}

// Validates: R-2.6, R-6.6.31
func TestResumeCommand_JSONReportsEveryDriveOutcome(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	paused := driveid.MustCanonicalID("personal:a@example.com")
	expired := driveid.MustCanonicalID("personal:b@example.com")
	running := driveid.MustCanonicalID("personal:c@example.com")
	for _, cid := range []driveid.CanonicalID{paused, expired, running} {
		require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/"+cid.Email()))
	}
	require.NoError(t, config.SetDriveKey(cfgPath, paused, "paused", "true"))
	require.NoError(t, config.SetDriveKey(cfgPath, expired, "paused", "true"))
	require.NoError(t, config.SetDriveKey(cfgPath, expired, "paused_until", "2000-01-01T00:00:00Z"))

	cc, stdout, stderr := newJSONCommandContext(cfgPath)
	require.NoError(t, runResumeCommand(t.Context(), cc, time.Now))

	var got resumeJSONOutput
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, resumeJSONOutput{Drives: []resumeJSONDrive{
		{Drive: paused.String(), Result: resumeOutcomeResumed},
		{Drive: expired.String(), Result: resumeOutcomeExpiredPause},
		{Drive: running.String(), Result: resumeOutcomeNotPaused},
	}}, got)
	assert.NotContains(t, stderr.String(), "resumed")
}

// Validates: R-3.3, R-6.6.31
func TestDriveRemove_JSONReportsWhatWasRemoved(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:remove@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))

	cc, stdout, _ := newJSONCommandContext(cfgPath)
	cc.Flags.Drive = []string{cid.String()}
	require.NoError(t, runDriveRemoveWithContext(t.Context(), cc, false))

	var got driveRemoveJSONOutput
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, driveRemoveJSONOutput{Drive: cid.String(), RemovedFromConfig: true}, got)

	// A second remove with --purge only cleans up leftover state.
	stdout.Reset()
	require.NoError(t, runDriveRemoveWithContext(t.Context(), cc, true))
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, driveRemoveJSONOutput{Drive: cid.String(), Purged: true}, got)
}

// Validates: R-3.3, R-6.6.31
func TestDriveAdd_JSONReportsAddedThenAlreadyConfigured(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("business:add@example.com")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_business_add@example.com.json")
	cc, stdout, _ := newJSONCommandContext(filepath.Join(t.TempDir(), "config.toml"))

	require.NoError(t, runDriveAddWithContext(t.Context(), cc, []string{cid.String()}))
	var got driveAddResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, cid.String(), got.Drive)
	assert.True(t, got.Added)
	assert.NotEmpty(t, got.SyncDir)

	stdout.Reset()
	require.NoError(t, runDriveAddWithContext(t.Context(), cc, []string{cid.String()}))
	var again driveAddResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &again))
	assert.False(t, again.Added)
	assert.Equal(t, got.SyncDir, again.SyncDir)
}

// Validates: R-3.1.3, R-6.6.31
func TestLogoutCommand_JSONNamesAccountAndDrives(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:logout@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_logout@example.com.json")
	seedCatalogDrive(t, cid, func(drive *config.CatalogDrive) { drive.RemoteDriveID = "drive-1" })

	cc, stdout, _ := newJSONCommandContext(cfgPath)
	require.NoError(t, runLogoutWithContext(t.Context(), cc, false))

	var got logoutJSONOutput
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, logoutJSONOutput{
		Account: "logout@example.com", TokenRemoved: true, Drives: []string{cid.String()},
	}, got)
}

// Validates: R-1.9, R-6.6.31
func TestRecycleBinEmpty_JSONListsDeletedAndFailedAndStillFails(t *testing.T) {
	setTestDriveHome(t)

	session := &partialRecycleBinSession{mockRecycleBinSession: mockRecycleBinSession{
		items: []graph.Item{{ID: "ok-1", Name: "old.txt"}, {ID: "bad-1", Name: "locked.txt"}},
	}, failID: "bad-1"}
	cc, stdout, stderr := newJSONCommandContext(filepath.Join(t.TempDir(), "config.toml"))

	err := runRecycleBinEmptyWithFactory(t.Context(), cc, true, func(context.Context) (recycleBinSession, error) {
		return session, nil
	})
	require.Error(t, err, "a partial failure still fails the command")

	var got recycleBinEmptyJSONOutput
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, []recycleBinEmptyJSONItem{{ID: "ok-1", Name: "old.txt"}}, got.Deleted)
	require.Len(t, got.Failed, 1)
	assert.Equal(t, "bad-1", got.Failed[0].ID)
	assert.NotEmpty(t, got.Failed[0].Error)
	assert.Contains(t, stderr.String(), "Failed to delete", "failures are still warned about on stderr")
	assert.NotContains(t, stderr.String(), "Deleted \"old.txt\"")
}

type partialRecycleBinSession struct {
	mockRecycleBinSession
	failID string
}

func (s *partialRecycleBinSession) PermanentDeleteItem(ctx context.Context, itemID string) error {
	if itemID == s.failID {
		return errors.New("item is locked")
	}

	return s.mockRecycleBinSession.PermanentDeleteItem(ctx, itemID)
}
