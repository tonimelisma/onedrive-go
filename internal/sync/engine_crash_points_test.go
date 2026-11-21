package sync

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	stdsync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

const crashTestDBName = "test.db"

// crashCapture snapshots the running test engine's state the first time its
// point is reached.
type crashCapture struct {
	point   string
	lostWAL bool
	srcDir  string
	dstDir  string
	remote  *moveScenarioRemote

	once       stdsync.Once
	err        error
	captured   bool
	remoteCopy *moveScenarioRemote
	identities map[synctree.FileIdentity]synctree.FileIdentity
}

func (c *crashCapture) hook(point string) error {
	if point == c.point {
		c.once.Do(func() { c.err = c.capture() })
	}

	return nil
}

func (c *crashCapture) capture() error {
	c.captured = true
	if err := c.captureDatabase(); err != nil {
		return err
	}
	identities, err := copyTreePreservingLinks(c.srcDir, c.dstDir)
	if err != nil {
		return err
	}
	c.identities = identities
	c.remoteCopy = c.remote.clone()

	return nil
}

// captureDatabase keeps what was committed: VACUUM INTO reads the last
// committed state from its own connection, so the transaction open at a
// store crash point is not in it. The lost-WAL copy is the main file alone.
func (c *crashCapture) captureDatabase() error {
	src := filepath.Join(c.srcDir, crashTestDBName)
	dst := filepath.Join(c.dstDir, crashTestDBName)
	if c.lostWAL {
		return syncfixture.CopyFileContent(src, dst)
	}

	db, err := sql.Open("sqlite", syncstore.SqliteFileURI(src, url.Values{syncstore.SqlitePragmaParam: {"busy_timeout(5000)"}}))
	if err != nil {
		return fmt.Errorf("open state for snapshot: %w", err)
	}
	defer db.Close()
	// The hook has no context of its own; the snapshot is one local read.
	if _, err := db.ExecContext(context.Background(), `VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("snapshot state: %w", err)
	}

	return nil
}

// copyTreePreservingLinks copies root to dst, except the state database and
// its WAL, keeping modes, mtimes and hard links, and returns which new
// identity each old one became.
func copyTreePreservingLinks(root, dst string) (map[synctree.FileIdentity]synctree.FileIdentity, error) {
	c := &treeCopier{
		root: root, dst: dst,
		identities: map[synctree.FileIdentity]synctree.FileIdentity{},
		firstPath:  map[synctree.FileIdentity]string{},
	}
	if err := filepath.WalkDir(root, c.visit); err != nil {
		return nil, fmt.Errorf("copy %s: %w", root, err)
	}
	// Folder mtimes changed as their children were copied in.
	if err := filepath.WalkDir(root, c.restoreDirTime); err != nil {
		return nil, fmt.Errorf("restore folder times under %s: %w", dst, err)
	}

	return c.identities, nil
}

type treeCopier struct {
	root, dst  string
	identities map[synctree.FileIdentity]synctree.FileIdentity
	firstPath  map[synctree.FileIdentity]string
}

func isCrashTestStateFile(rel string) bool {
	base := filepath.Base(rel)
	return rel != "." && (base == crashTestDBName || base == crashTestDBName+"-wal" || base == crashTestDBName+"-shm")
}

func (c *treeCopier) visit(p string, d fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		return walkErr
	}
	rel, err := filepath.Rel(c.root, p)
	if err != nil {
		return fmt.Errorf("relative path of %s: %w", p, err)
	}
	if isCrashTestStateFile(rel) {
		return nil
	}
	info, err := d.Info()
	if err != nil {
		return fmt.Errorf("stat %s: %w", p, err)
	}
	target := filepath.Join(c.dst, rel)
	old, _ := synctree.IdentityFromFileInfo(info)
	if copyErr := c.copyEntry(p, target, rel, info, old); copyErr != nil {
		return copyErr
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil
	}
	copied, err := os.Lstat(target)
	if err != nil {
		return fmt.Errorf("stat copy %s: %w", target, err)
	}
	if renewed, ok := synctree.IdentityFromFileInfo(copied); ok {
		c.identities[old] = renewed
	}

	return nil
}

func (c *treeCopier) copyEntry(p, target, rel string, info fs.FileInfo, old synctree.FileIdentity) error {
	switch {
	case info.IsDir():
		if rel == "." {
			return nil
		}
		if err := os.Mkdir(target, info.Mode().Perm()); err != nil {
			return fmt.Errorf("create %s: %w", target, err)
		}
	case !info.Mode().IsRegular():
		return nil
	case c.firstPath[old] != "":
		if err := os.Link(c.firstPath[old], target); err != nil {
			return fmt.Errorf("link %s: %w", target, err)
		}
	default:
		if err := syncfixture.CopyFileContent(p, target); err != nil {
			return err
		}
		if err := os.Chtimes(target, info.ModTime(), info.ModTime()); err != nil {
			return fmt.Errorf("set times of %s: %w", target, err)
		}
		c.firstPath[old] = target
	}

	return nil
}

func (c *treeCopier) restoreDirTime(p string, d fs.DirEntry, walkErr error) error {
	if walkErr != nil || !d.IsDir() {
		return walkErr
	}
	rel, err := filepath.Rel(c.root, p)
	if err != nil {
		return fmt.Errorf("relative path of %s: %w", p, err)
	}
	info, err := d.Info()
	if err != nil {
		return fmt.Errorf("stat %s: %w", p, err)
	}
	if err := os.Chtimes(filepath.Join(c.dst, rel), info.ModTime(), info.ModTime()); err != nil {
		return fmt.Errorf("set times of %s: %w", rel, err)
	}

	return nil
}

// reopen rewrites the snapshot's recorded identities to the copy's, names
// the items a lost WAL left without their create's intent, rebinds its
// sync folder, and opens a fresh engine on it.
func (c *crashCapture) reopen(t *testing.T) *syncHarness {
	t.Helper()

	require.NoError(t, c.err, "snapshot at %s", c.point)
	require.True(t, c.captured, "crash point %s was never reached", c.point)

	dbPath := filepath.Join(c.dstDir, crashTestDBName)
	db, err := sql.Open("sqlite", syncstore.SqliteFileURI(dbPath, nil))
	require.NoError(t, err)
	for _, update := range []string{
		`UPDATE baseline SET local_device = ?, local_inode = ?
			WHERE local_has_identity = 1 AND local_device = ? AND local_inode = ?`,
		`UPDATE local_state SET local_device = ?, local_inode = ?
			WHERE local_has_identity = 1 AND local_device = ? AND local_inode = ?`,
	} {
		for old, renewed := range c.identities {
			_, err := db.ExecContext(t.Context(), update,
				int64(renewed.Device), int64(renewed.Inode), int64(old.Device), int64(old.Inode))
			require.NoError(t, err)
		}
	}
	placeholders := map[string]bool{}
	if c.lostWAL {
		placeholders = placeholdersWithoutIntentIn(t, db, c.remoteCopy)
	}
	require.NoError(t, db.Close())
	require.NoError(t, syncstore.RebindSyncRoot(t.Context(), dbPath, syncfixture.TestLogger(t)))

	eng, syncRoot := newTestEngineInDir(t, t.Context(), c.remoteCopy.client(t), c.dstDir)

	return &syncHarness{eng: eng, remote: c.remoteCopy, syncRoot: syncRoot, placeholdersWithoutIntent: placeholders}
}

// placeholdersWithoutIntentIn is every empty file of remote, the OneDrive a
// lost WAL left, that db, the state database it left, has neither a
// pending-create row nor a baseline row for, by item ID
// (syncHarness.placeholdersWithoutIntent).
func placeholdersWithoutIntentIn(t *testing.T, db *sql.DB, remote *moveScenarioRemote) map[string]bool {
	t.Helper()

	placeholders := map[string]bool{}
	for p, itemID := range remote.emptyFiles() {
		var known int
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT
			(SELECT COUNT(*) FROM pending_creates WHERE path = ? OR item_id = ?) +
			(SELECT COUNT(*) FROM baseline WHERE item_id = ?)`, p, itemID, itemID).Scan(&known))
		if known == 0 {
			placeholders[itemID] = true
		}
	}

	return placeholders
}

// crashRecoveryScenario is a synced tree and one round of edits that passes
// every crash point: a local rename (remote move), a remote rename (local
// move), a local create (upload), a remote create (download), a remote edit
// (an observation window), a remote delete (local delete into the trash),
// a local delete (remote delete) and a remote rename onto a synced file
// OneDrive deleted (a local move that replaces its destination).
func crashRecoveryScenario(t *testing.T) (*moveScenarioRemote, *syncHarness) {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-a", "item-docs", "a.txt", "a\n")
	remote.addFile(t, "item-b", "item-docs", "b.txt", "b\n")
	remote.addFile(t, "item-e", "root", "e.txt", "e\n")
	remote.addFile(t, "item-f", "root", crashDeletedRemotelyPath, crashDeletedRemotelyContent)
	remote.addFile(t, crashDeletedLocallyItemID, "root", crashDeletedLocallyPath, "g\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	remote.addFile(t, "item-r", "item-docs", "r.txt", crashReplacingContent)
	remote.addFile(t, "item-s", "item-docs", path.Base(crashReplacedPath), crashReplacedContent)
	h := newSyncHarness(t, remote)
	h.eng.hooks.serializeWorkers = true
	runScenarioPass(t, h.eng)
	// The synced state reaches the main database file; the round's commits
	// stay in the WAL, which the lost-WAL variant then loses.
	_, err := h.eng.baseline.RawDBForTest().ExecContext(t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)

	require.NoError(t, os.Rename(filepath.Join(h.syncRoot, "docs", "a.txt"), filepath.Join(h.syncRoot, "docs", "a2.txt")))
	remote.moveAt(t, "docs/b.txt", "docs/b2.txt")
	syncfixture.WriteLocalFile(t, h.syncRoot, crashCreatedLocallyPath, crashCreatedLocallyContent)
	remote.putFileAt(t, "d.txt", "d remote\n")
	remote.putFileAt(t, "e.txt", "e edited\n")
	remote.deleteRemotely("item-f")
	require.NoError(t, os.Remove(filepath.Join(h.syncRoot, crashDeletedLocallyPath)))
	remote.deleteAt(t, crashReplacedPath)
	remote.moveAt(t, "docs/r.txt", crashReplacedPath)

	return remote, h
}

// The create, delete and replace edits of crashRecoveryScenario: c.txt is
// created locally and so uploaded as a new file, the one create of the round;
// f.txt is deleted on OneDrive and so leaves the local tree for the trash;
// g.txt is deleted locally and so is deleted on OneDrive; docs/s.txt is
// deleted on OneDrive and docs/r.txt renamed onto its name there, so the local
// move takes the synced docs/s.txt into the trash before it renames. That
// move is dispatched last, after the plain local move of docs/b2.txt, so a
// crash at exec.local_move_done cuts the plain one.
const (
	crashCreatedLocallyPath     = "c.txt"
	crashCreatedLocallyContent  = "c local\n"
	crashDeletedRemotelyPath    = "f.txt"
	crashDeletedRemotelyContent = "f\n"
	crashDeletedLocallyPath     = "g.txt"
	crashDeletedLocallyItemID   = "item-g"
	crashReplacedPath           = "docs/s.txt"
	crashReplacedContent        = "s\n"
	crashReplacingContent       = "r\n"
)

// crashAndRecover runs the scenario round with a capture armed at point and
// returns the engine that recovered from it, converged.
func crashAndRecover(t *testing.T, point string, lostWAL bool) (*crashCapture, *syncHarness) {
	t.Helper()

	remote, h := crashRecoveryScenario(t)
	capture := &crashCapture{
		point: point, lostWAL: lostWAL,
		srcDir: filepath.Dir(h.syncRoot), dstDir: t.TempDir(), remote: remote,
	}
	h.eng.armCrash(capture.hook)
	h.beginRound(t)
	_, err := h.eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	recovered := capture.reopen(t)
	recovered.held = h.held
	syncHarnessUntilQuiet(t, recovered)

	return capture, recovered
}

// syncHarnessUntilQuiet runs passes until one changes nothing.
func syncHarnessUntilQuiet(t *testing.T, h *syncHarness) {
	t.Helper()

	for range modelMaxSyncPasses {
		report, err := h.eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
		require.NoError(t, err)
		if report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes+report.FolderCreates+report.Moves+
			report.ConflictCopies+report.BaselineUpdates+report.Failed+report.HeldLocalDeletes+report.HeldRemoteDeletes == 0 {
			return
		}
	}
	require.Fail(t, "recovery did not converge", "state:%s", syncStateDump(t, h.eng))
}

// assertTrashHoldsReplacedOnce asserts that the trash holds one entry of the
// file replaced at p, holding content.
func assertTrashHoldsReplacedOnce(t *testing.T, trash *localtrash.Trash, p, content string) {
	t.Helper()

	entries, err := trash.List()
	require.NoError(t, err)
	var replaced []localtrash.Entry
	for i := range entries {
		if entries[i].Reason == localtrash.ReasonReplaced && entries[i].Path == p {
			replaced = append(replaced, entries[i])
		}
	}
	require.Len(t, replaced, 1, "the file replaced at %s is in the trash exactly once: %+v", p, entries)
	data, err := localpath.ReadFile(replaced[0].ItemPath)
	require.NoError(t, err)
	assert.Equal(t, content, string(data))
}

func crashCaseName(point string, lostWAL bool) string {
	if lostWAL {
		return point + "/lost-wal"
	}

	return point
}

// Validates: R-6.5.1, R-2.17.1, R-6.5.2, R-5.6.15, R-2.17.9
func TestCrashRecovery_EveryCrashPointConverges(t *testing.T) {
	t.Parallel()

	for _, point := range syncfixture.AllCrashPoints() {
		for _, lostWAL := range []bool{false, true} {
			t.Run(crashCaseName(point, lostWAL), func(t *testing.T) {
				t.Parallel()

				_, recovered := crashAndRecover(t, point, lostWAL)
				assertSyncInvariants(t, recovered)
				if point == syncdomain.CrashPointUploadCreatedUnrecorded && !lostWAL {
					// The kept intent claims the cut create's empty item, so the
					// upload finishes into it instead of meeting a create conflict.
					assert.Equal(t, crashCreatedLocallyContent,
						harnessLocalTree(t, recovered.syncRoot)[crashCreatedLocallyPath],
						"the cut create's file holds its content at its own name")
				}
				// Whether the crash came before the replacing move or between
				// its two steps, the file it displaced leaves for the trash once.
				assertTrashHoldsReplacedOnce(t, recovered.eng.trash, crashReplacedPath, crashReplacedContent)
			})
		}
	}
}

// A delete is the one action that destroys content, so a crash at either of
// its boundaries is checked beyond the shared invariants: the file deleted
// on OneDrive is in the local trash exactly once and restores from there;
// the file deleted locally is deleted on OneDrive exactly once and the
// recovered engine does not delete it again, re-upload it from the trash
// or bring it back down; and both baseline rows are gone.
//
// It runs on the production store setting, synchronous=FULL, where every
// other engine test skips the waits for the disk.
//
// Validates: R-6.5.2, R-6.4.9
func TestCrashRecovery_DeleteBoundariesDeleteOnceAndKeepTheTrashEntry(t *testing.T) {
	keepRealFsync(t)
	t.Parallel()

	for _, point := range []string{syncdomain.CrashPointLocalDeleteTrashed, syncdomain.CrashPointRemoteDeleteDone} {
		for _, lostWAL := range []bool{false, true} {
			t.Run(crashCaseName(point, lostWAL), func(t *testing.T) {
				t.Parallel()

				capture, recovered := crashAndRecover(t, point, lostWAL)
				require.Equal(t, sqliteSynchronousFull, storeSynchronous(t, recovered.eng.Engine), "the recovered store waits for the disk")
				require.True(t, assertSyncInvariants(t, recovered), "state:%s", syncStateDump(t, recovered.eng))

				local := harnessLocalTree(t, recovered.syncRoot)
				assert.NotContains(t, local, crashDeletedRemotelyPath, "the file deleted on OneDrive is not resurrected locally")
				assert.NotContains(t, local, crashDeletedLocallyPath, "the file deleted locally is not downloaded again")
				assert.NotContains(t, recovered.remote.tree(), crashDeletedRemotelyPath)
				assert.NotContains(t, recovered.remote.tree(), crashDeletedLocallyPath, "the file deleted locally is not re-uploaded")
				// The crashed engine finished its pass after the snapshot, so it
				// deleted g.txt once; the recovered engine deletes it only when
				// the snapshot still held it, never after the DELETE succeeded.
				assert.Equal(t, 1, syncfixture.CountOf(capture.remote.deletes, crashDeletedLocallyItemID),
					"the crashed engine deleted the locally deleted file on OneDrive exactly once")
				recoveredDeletes := syncfixture.CountOf(recovered.remote.deletes, crashDeletedLocallyItemID)
				assert.LessOrEqual(t, recoveredDeletes, 1, "the recovered engine deletes it at most once")
				if point == syncdomain.CrashPointRemoteDeleteDone {
					assert.Zero(t, recoveredDeletes, "a delete that reached OneDrive is not sent again")
				}
				bl, err := recovered.eng.baseline.Load(t.Context())
				require.NoError(t, err)
				for _, p := range []string{crashDeletedRemotelyPath, crashDeletedLocallyPath} {
					_, found := bl.GetByPath(p)
					assert.False(t, found, "baseline row for %s is gone", p)
				}

				entries, err := recovered.eng.trash.List()
				require.NoError(t, err)
				var deleted []localtrash.Entry
				for _, entry := range entries {
					if entry.Reason == localtrash.ReasonDeleted {
						deleted = append(deleted, entry)
					}
				}
				require.Len(t, deleted, 1, "the file deleted on OneDrive is in the trash exactly once: %+v", entries)
				entry := deleted[0]
				assert.Equal(t, crashDeletedRemotelyPath, entry.Path)
				assert.Contains(t, []localtrash.State{localtrash.StatePending, localtrash.StateCommitted}, entry.State)
				// The entry names the sync folder it came from; the snapshot is a
				// copy of that tree, so the restore goes back to the original.
				restoredTo, err := recovered.eng.trash.Restore(entry.ID)
				require.NoError(t, err, "a trash entry left by a crash restores")
				assert.Equal(t, filepath.Join(entry.SyncRoot, crashDeletedRemotelyPath), restoredTo)
				data, err := os.ReadFile(restoredTo) //nolint:gosec // test reads its own temp tree
				require.NoError(t, err)
				assert.Equal(t, crashDeletedRemotelyContent, string(data))
			})
		}
	}
}
