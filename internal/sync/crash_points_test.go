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

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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
// move), a local create (upload), a remote create (download) and a remote
// edit (an observation window).
func crashRecoveryScenario(t *testing.T) (*moveScenarioRemote, *syncHarness) {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-a", "item-docs", "a.txt", "a\n")
	remote.addFile(t, "item-b", "item-docs", "b.txt", "b\n")
	remote.addFile(t, "item-e", "root", "e.txt", "e\n")
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

	return remote, h
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

// Validates: R-6.8.9, R-2.17.1
func TestCrashRecovery_EveryCrashPointConverges(t *testing.T) {
	t.Parallel()

	for _, point := range []string{
		crashPointRemoteMoveDone,
		crashPointLocalMoveDone,
		crashPointUploadRemoteDone,
		crashPointDownloadRenamed,
		crashPointCommitObservation,
		crashPointCommitOutcome,
	} {
		for _, powerLoss := range []bool{false, true} {
			name := point
			if powerLoss {
				name += "/power-loss"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()

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
				assertSyncInvariants(t, recovered)
			})
		}
	}
}
