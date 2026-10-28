package sync

import (
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
// after a disk that never honoured the fsyncs, or a restore from a copy taken
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
	if _, err := db.Exec(`VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("snapshot state: %w", err)
	}

	return nil
}

// copyTreePreservingLinks copies root to dst, except the state database and
// its WAL, keeping modes, mtimes and hard links, and returns which new
// identity each old one became.
func copyTreePreservingLinks(root, dst string) (map[synctree.FileIdentity]synctree.FileIdentity, error) {
	identities := map[synctree.FileIdentity]synctree.FileIdentity{}
	firstPath := map[synctree.FileIdentity]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		if base := filepath.Base(rel); rel != "." && (base == crashTestDBName || base == crashTestDBName+"-wal" || base == crashTestDBName+"-shm") {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		target := filepath.Join(dst, rel)
		old, _ := synctree.IdentityFromFileInfo(info)
		switch {
		case d.IsDir():
			if rel != "." {
				if mkErr := os.Mkdir(target, info.Mode().Perm()); mkErr != nil {
					return mkErr
				}
			}
		case !info.Mode().IsRegular():
			return nil
		case firstPath[old] != "":
			return os.Link(firstPath[old], target)
		default:
			if copyErr := copyFileContent(p, target); copyErr != nil {
				return copyErr
			}
			if timeErr := os.Chtimes(target, info.ModTime(), info.ModTime()); timeErr != nil {
				return timeErr
			}
			firstPath[old] = target
		}
		newInfo, statErr := os.Lstat(target)
		if statErr != nil {
			return statErr
		}
		if renewed, ok := synctree.IdentityFromFileInfo(newInfo); ok {
			identities[old] = renewed
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("copy %s: %w", root, err)
	}
	// Folder mtimes changed as their children were copied in.
	return identities, restoreDirTimes(root, dst)
}

func restoreDirTimes(root, dst string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		return os.Chtimes(filepath.Join(dst, rel), info.ModTime(), info.ModTime())
	})
}

func copyFileContent(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // test copies its own temp tree
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm()) //nolint:gosec // test copies its own temp tree
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}

	return out.Close()
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
	for _, table := range []string{"baseline", "local_state"} {
		for old, renewed := range c.identities {
			_, err := db.ExecContext(t.Context(),
				`UPDATE `+table+` SET local_device = ?, local_inode = ?
				WHERE local_has_identity = 1 AND local_device = ? AND local_inode = ?`,
				int64(renewed.Device), int64(renewed.Inode), int64(old.Device), int64(old.Inode)) //nolint:gosec // identities fit the stored columns
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
