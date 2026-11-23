package sync

import (
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
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
//     row survives at a :park: path, every baseline row agrees with
//     remote_state (same item at the same path) and with local_state, and
//     the store's baseline cache equals the baseline table, field for field;
//   - nothing left waiting: retry_work, held_deletes and pending_creates are
//     empty;
//   - no stray artifacts: no partial download or other .onedrive-go. file in
//     the tree, no upload session left behind, and no empty file on either
//     side without an allowance: scenario content is never empty, so an empty
//     file is a create's placeholder or a lost claim left behind, at a
//     conflict-copy name or any other. A scenario that syncs a zero-byte file
//     on purpose declares its path (syncHarness.emptyFilesAllowed), and the
//     item a create left on OneDrive whose intent the crash harness's lost-WAL
//     variant then took with the WAL is allowed by its ID
//     (syncHarness.placeholdersWithoutIntent).

// syncHarness is a test engine, its sync root and the fake OneDrive it
// syncs with.
type syncHarness struct {
	eng      *testEngine
	remote   *moveScenarioRemote
	syncRoot string

	// held is every file content present on either side when beginRound
	// last ran: the no-loss invariant's input.
	held []string

	// placeholdersWithoutIntent are the OneDrive items, by ID, that a
	// lost-WAL snapshot held as empty files its state database had neither
	// a pending-create row nor a baseline row for (crashCapture.reopen): the
	// item of a create cut after OneDrive made it, whose committed intent the
	// lost WAL took. Nothing then tells the item from another client's empty
	// file, so the restart keeps it beside the local file as two files created
	// at one name (R-2.3.5) and downloads it, empty. A plain crash keeps the
	// intent, so it adds nothing here.
	placeholdersWithoutIntent map[string]bool

	// emptyFilesAllowed are the paths at which the scenario syncs a
	// zero-byte file on purpose; any other empty file of a converged tree
	// fails the stray-artifact invariant.
	emptyFilesAllowed map[string]bool
}

func newSyncHarness(t *testing.T, remote *moveScenarioRemote) *syncHarness {
	t.Helper()

	return newSyncHarnessWithClient(t, remote, remote.client(t))
}

// newSyncHarnessWithClient syncs with remote through client, a wrapper of
// remote's own client (the chaos layer).
func newSyncHarnessWithClient(t *testing.T, remote *moveScenarioRemote, client *syncfixture.EngineMockClient) *syncHarness {
	t.Helper()

	eng, syncRoot := newTestEngine(t, client)

	return &syncHarness{eng: eng, remote: remote, syncRoot: syncRoot}
}

// beginRound records the content both sides hold now. Call it after a
// round's edits and before its sync passes.
func (h *syncHarness) beginRound(t *testing.T) {
	t.Helper()

	h.held = h.held[:0]
	for _, tree := range []map[string]string{harnessLocalTree(t, h.syncRoot), h.remote.tree()} {
		for _, content := range tree {
			if content != syncfixture.ModelFolderSentinel {
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

	report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
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
	db := eng.baseline.RawDBForTest()

	var integrity string
	require.NoError(t, db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity))
	ok := assert.Equal(t, "ok", integrity, "invariant: PRAGMA integrity_check")

	ok = assertForeignKeysClean(t, db) && ok
	ok = assert.NoError(t, eng.baseline.CheckCacheConsistency(ctx), "invariant: the baseline cache equals the database") && ok

	for _, query := range []struct{ what, sql string }{
		{"rows parked at :park: paths", `SELECT
			(SELECT COUNT(*) FROM remote_state WHERE substr(path, 1, 6) = ':park:') +
			(SELECT COUNT(*) FROM baseline WHERE substr(path, 1, 6) = ':park:') +
			(SELECT COUNT(*) FROM local_state WHERE substr(path, 1, 6) = ':park:')`},
		{"retry_work rows after convergence", `SELECT COUNT(*) FROM retry_work`},
		{"held deletes after convergence", `SELECT COUNT(*) FROM held_deletes`},
		{"pending creates after convergence", `SELECT COUNT(*) FROM pending_creates`},
		{"baseline rows whose item is elsewhere, of another type or absent in remote_state", `SELECT COUNT(*) FROM baseline b
			WHERE NOT EXISTS (SELECT 1 FROM remote_state r
				WHERE r.item_id = b.item_id AND r.path = b.path AND r.item_type = b.item_type)`},
		{"baseline rows absent from local_state or of another type there", `SELECT COUNT(*) FROM baseline b
			WHERE NOT EXISTS (SELECT 1 FROM local_state l WHERE l.path = b.path AND l.item_type = b.item_type)`},
	} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, query.sql).Scan(&count), query.what)
		ok = assert.Zero(t, count, "invariant: no %s", query.what) && ok
	}

	return ok
}

func assertForeignKeysClean(t *testing.T, db *sql.DB) bool {
	t.Helper()

	rows, err := db.QueryContext(t.Context(), `PRAGMA foreign_key_check`)
	require.NoError(t, err)
	defer rows.Close()
	clean := assert.False(t, rows.Next(), "invariant: PRAGMA foreign_key_check is clean")
	require.NoError(t, rows.Err())

	return clean
}

func assertNoStrayArtifacts(t *testing.T, h *syncHarness, local map[string]string) bool {
	t.Helper()

	ok := true
	for p := range local {
		ok = assert.False(t, strings.HasPrefix(filepath.Base(p), ".onedrive-go."), "invariant: no transfer artifact left at %s", p) && ok
	}
	for _, p := range strayEmptyFiles(local, h.remote.emptyFiles(), h.emptyFilesAllowed, h.placeholdersWithoutIntent) {
		ok = assert.Fail(t, "invariant: no empty file without an allowance",
			"%s is empty: scenario content is never empty, so it is a placeholder or a lost claim left behind; "+
				"a scenario that syncs a zero-byte file there declares it (syncHarness.emptyFilesAllowed)", p) && ok
	}

	sessions, err := os.ReadDir(filepath.Join(h.eng.dataDir, "upload-sessions"))
	if !os.IsNotExist(err) {
		require.NoError(t, err)
		ok = assert.Empty(t, sessions, "invariant: no upload session outlives its upload") && ok
	}

	return ok
}

// strayEmptyFiles lists, sorted, every path at which either side of a
// converged tree holds an empty file without an allowance: local is the sync
// root as path -> content, remoteEmpty OneDrive's empty files as path ->
// item ID, allowedPaths the paths a scenario declares, and placeholders the
// item IDs the lost-WAL crash variant allows.
func strayEmptyFiles(local, remoteEmpty map[string]string, allowedPaths, placeholders map[string]bool) []string {
	empty := map[string]bool{}
	for p, content := range local {
		if content == "" {
			empty[p] = true
		}
	}
	for p := range remoteEmpty {
		empty[p] = true
	}

	var stray []string
	for p := range empty {
		if allowedPaths[p] || placeholders[remoteEmpty[p]] {
			continue
		}
		stray = append(stray, p)
	}
	slices.Sort(stray)

	return stray
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
		if relErr != nil {
			return fmt.Errorf("relative path of %s: %w", p, relErr)
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if withFolders {
				out[rel] = syncfixture.ModelFolderSentinel
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, readErr := os.ReadFile(p) //nolint:gosec // test reads its own temp tree
		if readErr != nil {
			return fmt.Errorf("read %s: %w", p, readErr)
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

func syncStateRows(t *testing.T, eng *testEngine, query string) string {
	t.Helper()

	rows, err := eng.baseline.RawDBForTest().QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer rows.Close()
	var out strings.Builder
	for rows.Next() {
		var p, id, kind, hash string
		var inode int64
		require.NoError(t, rows.Scan(&p, &id, &kind, &hash, &inode))
		fmt.Fprintf(&out, "\n    %s %s %s %s ino=%d", p, id, kind, hash, inode)
	}
	require.NoError(t, rows.Err())

	return out.String()
}

// syncStateDump renders the baseline, remote_state and local_state rows for a
// failure message: a non-converging seed is diagnosed from these three views.
func syncStateDump(t *testing.T, eng *testEngine) string {
	t.Helper()

	var out strings.Builder
	for _, q := range []struct{ name, sql string }{
		{"baseline", `SELECT path, item_id, item_type, COALESCE(local_hash, '') || ' / ' || COALESCE(remote_hash, ''), local_inode FROM baseline ORDER BY path`},
		{"remote_state", `SELECT path, item_id, item_type, COALESCE(hash, ''), 0 FROM remote_state ORDER BY path`},
		{"local_state", `SELECT path, '', item_type, COALESCE(hash, ''), local_inode FROM local_state ORDER BY path`},
		{"pending_creates", `SELECT path, item_id, created_etag, local_hash, local_size FROM pending_creates ORDER BY path`},
	} {
		out.WriteString("\n  " + q.name + ":")
		out.WriteString(syncStateRows(t, eng, q.sql))
	}

	return out.String()
}

// Validates: R-6.5.2
//
// The stray-artifact invariant reads every empty file of a converged tree,
// on either side, against an explicit allowance: a planted recovery that
// left a lost claim's empty file at an ordinary name beside the user's
// conflict copy is caught, and so is one at a conflict-copy name and one
// only OneDrive holds; a scenario's zero-byte file passes with its declared
// path and is caught without it; the lost-WAL placeholder stays allowed by
// its item.
func TestStrayEmptyFiles_EveryEmptyFileNeedsAnAllowance(t *testing.T) {
	t.Parallel()

	const conflictCopy = "doc.conflict-20260101-000000.txt"
	plantedRecovery := map[string]string{"doc.txt": "", conflictCopy: "my words\n", "keep.txt": "keep\n"}
	assert.Equal(t, []string{"doc.txt"},
		strayEmptyFiles(plantedRecovery, map[string]string{"doc.txt": "item-lost"}, nil, nil),
		"a lost claim's empty file at an ordinary name")
	assert.Equal(t, []string{conflictCopy},
		strayEmptyFiles(map[string]string{conflictCopy: ""}, map[string]string{conflictCopy: "item-copy"}, nil, nil),
		"an empty file at a conflict-copy name")
	assert.Equal(t, []string{"remote-only.txt"},
		strayEmptyFiles(map[string]string{}, map[string]string{"remote-only.txt": "item-remote"}, nil, nil),
		"an empty file only OneDrive holds")

	zeroByte := map[string]string{"empty.txt": "", "keep.txt": "keep\n"}
	remoteZeroByte := map[string]string{"empty.txt": "item-empty"}
	assert.Empty(t, strayEmptyFiles(zeroByte, remoteZeroByte, map[string]bool{"empty.txt": true}, nil),
		"a zero-byte file the scenario declares")
	assert.Equal(t, []string{"empty.txt"}, strayEmptyFiles(zeroByte, remoteZeroByte, nil, nil),
		"the same zero-byte file without the declaration")

	assert.Empty(t, strayEmptyFiles(map[string]string{"doc.txt": ""}, map[string]string{"doc.txt": "orphan"},
		nil, map[string]bool{"orphan": true}), "the lost-WAL placeholder, allowed by its item")
	assert.Empty(t, strayEmptyFiles(map[string]string{"docs": syncfixture.ModelFolderSentinel, "a.txt": "a\n"},
		map[string]string{}, nil, nil), "folders and files with content")
}
