package sync

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Invariant library (D-095). One assertion of everything a converged sync
// must hold, shared by the model test and the scenario tests, so each new
// harness checks the same properties instead of the few its author thought
// of:
//
//   - convergence: the local tree equals the OneDrive tree;
//   - no loss: every file content either side held when the round began is
//     still somewhere -- on either side, in OneDrive's recycle bin or version
//     history, or in the local trash;
//   - idempotence: one more pass changes nothing (it runs first, so the
//     checks below also see the observation that follows the last
//     mutation: local_state is observed truth, refreshed by a scan);
//   - DB integrity: PRAGMA integrity_check and foreign_key_check pass, no
//     row survives at a :park: path, and every baseline row agrees with
//     remote_state (same item at the same path) and with local_state;
//   - nothing left waiting: retry_work, held_deletes and pending_creates are
//     empty;
//   - no stray artifacts: no partial download or other .onedrive-go. file in
//     the tree, no upload session left behind, no empty conflict-copy
//     reservation.

// syncHarness is a test engine, its sync root and the fake OneDrive it
// syncs with.
type syncHarness struct {
	eng      *testEngine
	remote   *moveScenarioRemote
	syncRoot string

	// held is every file content present on either side when beginRound
	// last ran: the no-loss invariant's input.
	held []string
}

func newSyncHarness(t *testing.T, remote *moveScenarioRemote) *syncHarness {
	t.Helper()

	eng, syncRoot := newTestEngine(t, remote.client(t))

	return &syncHarness{eng: eng, remote: remote, syncRoot: syncRoot}
}

// beginRound records the content both sides hold now. Call it after a
// round's edits and before its sync passes.
func (h *syncHarness) beginRound(t *testing.T) {
	t.Helper()

	h.held = h.held[:0]
	for _, tree := range []map[string]string{harnessLocalTree(t, h.syncRoot), h.remote.tree()} {
		for _, content := range tree {
			if content != modelFolderSentinel {
				h.held = append(h.held, content)
			}
		}
	}
}

// assertSyncInvariants checks every invariant above, reports each violation
// separately, and returns whether all held.
func assertSyncInvariants(t *testing.T, h *syncHarness) bool {
	t.Helper()

	ok := assertQuietNextPass(t, h.eng)
	local := harnessLocalTree(t, h.syncRoot)
	ok = assert.Equal(t, h.remote.tree(), local, "invariant: local and OneDrive trees converge") && ok
	ok = assertNothingLost(t, h, local) && ok
	ok = assertStoreIntegrity(t, h.eng) && ok
	ok = assertNoStrayArtifacts(t, h, local) && ok

	return ok
}

func assertQuietNextPass(t *testing.T, eng *testEngine) bool {
	t.Helper()

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	return assert.Zero(t, report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes+report.FolderCreates+
		report.Moves+report.ConflictCopies+report.HeldLocalDeletes+report.HeldRemoteDeletes+report.Failed,
		"invariant: a pass over converged truth changes nothing: %+v", report)
}

func assertNothingLost(t *testing.T, h *syncHarness, local map[string]string) bool {
	t.Helper()

	kept := map[string]bool{}
	for _, tree := range []map[string]string{local, h.remote.tree()} {
		for _, content := range tree {
			kept[content] = true
		}
	}
	for _, content := range h.remote.retainedContents() {
		kept[content] = true
	}
	for _, content := range harnessTrashContents(t, h.eng) {
		kept[content] = true
	}
	ok := true
	for _, content := range h.held {
		ok = assert.True(t, kept[content], "invariant: no loss; %q is on neither side, in no recycle bin, version or trash", content) && ok
	}

	return ok
}

func assertStoreIntegrity(t *testing.T, eng *testEngine) bool {
	t.Helper()

	ctx := t.Context()
	db := eng.baseline.rawDB()

	var integrity string
	require.NoError(t, db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity))
	ok := assert.Equal(t, "ok", integrity, "invariant: PRAGMA integrity_check")

	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	require.NoError(t, err)
	ok = assert.False(t, rows.Next(), "invariant: PRAGMA foreign_key_check is clean") && ok
	require.NoError(t, rows.Close())
	require.NoError(t, rows.Err())

	for _, query := range []struct{ what, sql string }{
		{"rows parked at :park: paths", `SELECT
			(SELECT COUNT(*) FROM remote_state WHERE substr(path, 1, 6) = ':park:') +
			(SELECT COUNT(*) FROM baseline WHERE substr(path, 1, 6) = ':park:') +
			(SELECT COUNT(*) FROM local_state WHERE substr(path, 1, 6) = ':park:')`},
		{"retry_work rows after convergence", `SELECT COUNT(*) FROM retry_work`},
		{"held deletes after convergence", `SELECT COUNT(*) FROM held_deletes`},
		{"pending creates after convergence", `SELECT COUNT(*) FROM pending_creates`},
		{"baseline rows whose item is elsewhere or absent in remote_state", `SELECT COUNT(*) FROM baseline b
			WHERE NOT EXISTS (SELECT 1 FROM remote_state r WHERE r.item_id = b.item_id AND r.path = b.path)`},
		{"baseline rows absent from local_state", `SELECT COUNT(*) FROM baseline b
			WHERE NOT EXISTS (SELECT 1 FROM local_state l WHERE l.path = b.path)`},
	} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, query.sql).Scan(&count), query.what)
		ok = assert.Zero(t, count, "invariant: no %s", query.what) && ok
	}

	return ok
}

func assertNoStrayArtifacts(t *testing.T, h *syncHarness, local map[string]string) bool {
	t.Helper()

	ok := true
	for p, content := range local {
		name := filepath.Base(p)
		ok = assert.False(t, strings.HasPrefix(name, ".onedrive-go."), "invariant: no transfer artifact left at %s", p) && ok
		ok = assert.False(t, strings.Contains(name, modelConflictMarker) && content == "",
			"invariant: no empty conflict-copy reservation left at %s (scenario content is never empty)", p) && ok
	}

	sessions, err := os.ReadDir(filepath.Join(h.eng.dataDir, "upload-sessions"))
	if !os.IsNotExist(err) {
		require.NoError(t, err)
		ok = assert.Empty(t, sessions, "invariant: no upload session outlives its upload") && ok
	}

	return ok
}

// harnessLocalTree is the sync root as path -> content, folders as "/".
func harnessLocalTree(t *testing.T, syncRoot string) map[string]string {
	t.Helper()

	return readTreeContents(t, syncRoot, true)
}

// harnessTrashContents is every file content in the engine's local trash.
func harnessTrashContents(t *testing.T, eng *testEngine) []string {
	t.Helper()

	if eng.trash == nil {
		return nil
	}
	var contents []string
	for _, content := range readTreeContents(t, eng.trash.Dir(), false) {
		contents = append(contents, content)
	}

	return contents
}

func readTreeContents(t *testing.T, root string, withFolders bool) map[string]string {
	t.Helper()

	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil || rel == "." {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if withFolders {
				out[rel] = modelFolderSentinel
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, readErr := os.ReadFile(p) //nolint:gosec // test reads its own temp tree
		if readErr != nil {
			return readErr
		}
		out[rel] = string(data)
		return nil
	})
	if os.IsNotExist(err) {
		return out
	}
	require.NoError(t, err)

	return out
}
