package sync

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	stdsync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// Crash-point harness (D-091). A hook at one commit boundary (crash_points.go)
// takes a snapshot of everything a process killed at that instant leaves
// behind: the committed state database, the local tree and data directory,
// and OneDrive as it is. A fresh engine then opens the snapshot and must
// converge with every invariant holding (assertSyncInvariants), no content
// lost.
//
// The power-loss variant keeps only the main database file: whatever was
// committed to the write-ahead log since the last checkpoint is gone, as
// after a disk that never honored the fsyncs, or a restore from a copy taken
// without the WAL. The state is then older than both trees.
//
// The snapshot is a copy, so every entry has a new inode. The copy records
// which new identity stands for which old one, and the copied database is
// rewritten to match (and its sync folder rebound, as the rebind-root command
// does), so recovery sees the identities a restart on the same disk would.

const crashTestDBName = "test.db"

// crashCapture snapshots the running test engine's state the first time its
// point is reached.
type crashCapture struct {
	point     string
	powerLoss bool
	srcDir    string
	dstDir    string
	remote    *moveScenarioRemote

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
// store crash point is not in it. The power-loss copy is the main file alone.
func (c *crashCapture) captureDatabase() error {
	src := filepath.Join(c.srcDir, crashTestDBName)
	dst := filepath.Join(c.dstDir, crashTestDBName)
	if c.powerLoss {
		return copyFileContent(src, dst)
	}

	db, err := sql.Open("sqlite", "file:"+src+"?_pragma=busy_timeout(5000)")
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
		if err := copyFileContent(p, target); err != nil {
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

func copyFileContent(src, dst string) (err error) {
	in, err := os.Open(src) //nolint:gosec // test copies its own temp tree
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm()) //nolint:gosec // test copies its own temp tree
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer func() {
		if closeErr := out.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", dst, closeErr)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}

	return nil
}

// reopen rewrites the snapshot's recorded identities to the copy's, rebinds
// its sync folder, and opens a fresh engine on it.
func (c *crashCapture) reopen(t *testing.T) *syncHarness {
	t.Helper()

	require.NoError(t, c.err, "snapshot at %s", c.point)
	require.True(t, c.captured, "crash point %s was never reached", c.point)

	dbPath := filepath.Join(c.dstDir, crashTestDBName)
	db, err := sql.Open("sqlite", "file:"+dbPath)
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
	require.NoError(t, db.Close())
	require.NoError(t, RebindSyncRoot(t.Context(), dbPath, testLogger(t)))

	eng, syncRoot := newTestEngineInDir(t, t.Context(), c.remoteCopy.client(t), c.dstDir)

	return &syncHarness{eng: eng, remote: c.remoteCopy, syncRoot: syncRoot}
}

// crashRecoveryScenario is a synced tree and one round of edits that passes
// every crash point: a local rename (remote move), a remote rename (local
// move), a local create (upload), a remote create (download), a remote edit
// (an observation window), a remote delete (local delete into the trash)
// and a local delete (remote delete).
func crashRecoveryScenario(t *testing.T) (*moveScenarioRemote, *syncHarness) {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-a", "item-docs", "a.txt", "a\n")
	remote.addFile(t, "item-b", "item-docs", "b.txt", "b\n")
	remote.addFile(t, "item-e", "root", "e.txt", "e\n")
	remote.addFile(t, "item-f", "root", crashDeletedRemotelyPath, crashDeletedRemotelyContent)
	remote.addFile(t, crashDeletedLocallyItemID, "root", crashDeletedLocallyPath, "g\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	h := newSyncHarness(t, remote)
	h.eng.serializeWorkers = true
	runScenarioPass(t, h.eng)
	// The synced state reaches the main database file; the round's commits
	// stay in the WAL, which the power-loss variant then loses.
	_, err := h.eng.baseline.rawDB().ExecContext(t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)

	require.NoError(t, os.Rename(filepath.Join(h.syncRoot, "docs", "a.txt"), filepath.Join(h.syncRoot, "docs", "a2.txt")))
	remote.moveAt(t, "docs/b.txt", "docs/b2.txt")
	writeLocalFile(t, h.syncRoot, "c.txt", "c local\n")
	remote.putFileAt(t, "d.txt", "d remote\n")
	remote.putFileAt(t, "e.txt", "e edited\n")
	remote.deleteRemotely("item-f")
	require.NoError(t, os.Remove(filepath.Join(h.syncRoot, crashDeletedLocallyPath)))

	return remote, h
}

// The delete edits of crashRecoveryScenario: f.txt is deleted on OneDrive and
// so leaves the local tree for the trash; g.txt is deleted locally and so is
// deleted on OneDrive.
const (
	crashDeletedRemotelyPath    = "f.txt"
	crashDeletedRemotelyContent = "f\n"
	crashDeletedLocallyPath     = "g.txt"
	crashDeletedLocallyItemID   = "item-g"
)

// allCrashPoints is every crash point crashRecoveryScenario reaches.
func allCrashPoints() []string {
	return []string{
		crashPointRemoteMoveDone,
		crashPointLocalMoveDone,
		crashPointUploadRemoteDone,
		crashPointDownloadRenamed,
		crashPointLocalDeleteTrashed,
		crashPointRemoteDeleteDone,
		crashPointCommitObservation,
		crashPointCommitOutcome,
	}
}

// crashAndRecover runs the scenario round with a capture armed at point and
// returns the engine that recovered from it, converged.
func crashAndRecover(t *testing.T, point string, powerLoss bool) (*crashCapture, *syncHarness) {
	t.Helper()

	remote, h := crashRecoveryScenario(t)
	capture := &crashCapture{
		point: point, powerLoss: powerLoss,
		srcDir: filepath.Dir(h.syncRoot), dstDir: t.TempDir(), remote: remote,
	}
	h.eng.execCfg.crashAt = capture.hook
	h.eng.baseline.crashAt = capture.hook
	h.beginRound(t)
	_, err := h.eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
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
		report, err := h.eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
		require.NoError(t, err)
		if report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes+report.FolderCreates+report.Moves+
			report.Failed+report.HeldLocalDeletes+report.HeldRemoteDeletes == 0 {
			return
		}
	}
	require.Fail(t, "recovery did not converge", "state:%s", syncStateDump(t, h.eng))
}

func countOf(values []string, want string) int {
	n := 0
	for _, v := range values {
		if v == want {
			n++
		}
	}

	return n
}

func crashCaseName(point string, powerLoss bool) string {
	if powerLoss {
		return point + "/power-loss"
	}

	return point
}

// Validates: R-6.8.9, R-2.17.1, R-6.5.2
func TestCrashRecovery_EveryCrashPointConverges(t *testing.T) {
	t.Parallel()

	for _, point := range allCrashPoints() {
		for _, powerLoss := range []bool{false, true} {
			t.Run(crashCaseName(point, powerLoss), func(t *testing.T) {
				t.Parallel()

				_, recovered := crashAndRecover(t, point, powerLoss)
				assertSyncInvariants(t, recovered)
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
// Validates: R-6.5.2, R-6.4.9
func TestCrashRecovery_DeleteBoundariesDeleteOnceAndKeepTheTrashEntry(t *testing.T) {
	t.Parallel()

	for _, point := range []string{crashPointLocalDeleteTrashed, crashPointRemoteDeleteDone} {
		for _, powerLoss := range []bool{false, true} {
			t.Run(crashCaseName(point, powerLoss), func(t *testing.T) {
				t.Parallel()

				capture, recovered := crashAndRecover(t, point, powerLoss)
				require.True(t, assertSyncInvariants(t, recovered), "state:%s", syncStateDump(t, recovered.eng))

				local := harnessLocalTree(t, recovered.syncRoot)
				assert.NotContains(t, local, crashDeletedRemotelyPath, "the file deleted on OneDrive is not resurrected locally")
				assert.NotContains(t, local, crashDeletedLocallyPath, "the file deleted locally is not downloaded again")
				assert.NotContains(t, recovered.remote.tree(), crashDeletedRemotelyPath)
				assert.NotContains(t, recovered.remote.tree(), crashDeletedLocallyPath, "the file deleted locally is not re-uploaded")
				// The crashed engine finished its pass after the snapshot, so it
				// deleted g.txt once; the recovered engine deletes it only when
				// the snapshot still held it, never after the DELETE succeeded.
				assert.Equal(t, 1, countOf(capture.remote.deletes, crashDeletedLocallyItemID),
					"the crashed engine deleted the locally deleted file on OneDrive exactly once")
				recoveredDeletes := countOf(recovered.remote.deletes, crashDeletedLocallyItemID)
				assert.LessOrEqual(t, recoveredDeletes, 1, "the recovered engine deletes it at most once")
				if point == crashPointRemoteDeleteDone {
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
