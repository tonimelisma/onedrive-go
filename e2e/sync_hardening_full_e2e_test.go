//go:build e2e && e2e_full

package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/unicode/norm"
)

// Live sync coverage the hardening program found missing (D-097): sync moves,
// a naming corpus, two clients writing to one folder, a multi-chunk file
// carried between clients, and type changes both ways. Every test syncs an
// isolated shared root, a fresh folder removed afterwards, so live-drive
// activity elsewhere cannot touch its assertions. They run only in the
// e2e-full serial sync bucket; like the rest of the live suite they need the
// test account's credentials (TestMain refuses to start without them).

const (
	// hardeningLargeFileSize spans several upload-session fragments (the
	// client uploads in 10 MiB chunks), so the session path, not the simple
	// upload, carries it.
	hardeningLargeFileSize = 25*1024*1024 + 123
	hardeningSyncTimeout   = 2 * time.Minute
)

func remoteItemID(t *testing.T, cfgPath string, env map[string]string, remotePath string) string {
	t.Helper()

	stdout, _ := runCLIWithConfig(t, cfgPath, env, "stat", "--json", remotePath)
	var stat statIDJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &stat), "stat %s: %s", remotePath, stdout)
	require.NotEmpty(t, stat.ID, "stat %s has no item ID", remotePath)

	return stat.ID
}

func remoteNames(t *testing.T, cfgPath string, env map[string]string, remotePath string) []string {
	t.Helper()

	stdout, _ := runCLIWithConfig(t, cfgPath, env, "ls", "--json", remotePath)
	var items []remoteListJSONItem
	require.NoError(t, json.Unmarshal([]byte(stdout), &items), "ls %s: %s", remotePath, stdout)
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name)
	}

	return names
}

// contentTree is a local tree as relative path -> content, folders as "/";
// unlike snapshotLocalTree it ignores modes, which two clients may differ on.
func contentTree(t *testing.T, root string) map[string]string {
	t.Helper()

	tree := map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
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
			tree[rel] = "/"
			return nil
		}
		data, readErr := os.ReadFile(p) //nolint:gosec // the test's own sync dir
		if readErr != nil {
			return fmt.Errorf("read %s: %w", p, readErr)
		}
		tree[rel] = string(data)
		return nil
	}))

	return tree
}

func writeLocal(t *testing.T, root, rel, content string) {
	t.Helper()

	abs := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o700))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o600))
}

// syncUntil runs sync until ready holds for the local tree, as remote
// changes reach delta only eventually.
func syncUntil(t *testing.T, cfgPath string, env map[string]string, description string, ready func() bool) {
	t.Helper()

	requireSyncEventuallyConverges(t, cfgPath, env, hardeningSyncTimeout, description,
		func(r syncAttemptResult) bool { return r.Err == nil && ready() })
}

// Validates: R-2.17.1, R-2.17.2
//
// A local folder rename reaches OneDrive as a move of the same item, a
// OneDrive rename of a populated folder reaches the local tree as one move,
// and a second sync of each is quiet.
func TestE2E_Sync_FolderMovesBothWays(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
	writeLocal(t, syncDir, "proj/a.txt", "file a")
	writeLocal(t, syncDir, "proj/sub/b.txt", "file b")
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
	waitForRemoteReadContains(t, cfgPath, env, "", "b.txt", pollTimeout, "ls", "/proj/sub")
	folderID := remoteItemID(t, cfgPath, env, "/proj")
	runCLIWithConfig(t, cfgPath, env, "sync")

	// Local rename: OneDrive moves the item rather than re-creating it.
	require.NoError(t, os.Rename(filepath.Join(syncDir, "proj"), filepath.Join(syncDir, "proj-local")))
	runCLIWithConfig(t, cfgPath, env, "sync")
	waitForRemoteReadContains(t, cfgPath, env, "", "b.txt", pollTimeout, "ls", "/proj-local/sub")
	assert.Equal(t, folderID, remoteItemID(t, cfgPath, env, "/proj-local"), "a local folder rename is a remote move of the same item")
	assert.NotContains(t, remoteNames(t, cfgPath, env, "/"), "proj")
	assertSyncLeavesLocalTreeStable(t, cfgPath, env, syncDir, "sync")

	// OneDrive rename of the populated folder: the local tree moves too.
	runCLIWithConfig(t, cfgPath, env, "mv", "/proj-local", "/proj-online")
	syncUntil(t, cfgPath, env, "remote folder rename reaches the local tree", func() bool {
		_, err := os.Stat(filepath.Join(syncDir, "proj-online", "sub", "b.txt"))
		return err == nil
	})
	assert.NoDirExists(t, filepath.Join(syncDir, "proj-local"))
	assert.Equal(t, map[string]string{
		"proj-online": "/", "proj-online/a.txt": "file a", "proj-online/sub": "/", "proj-online/sub/b.txt": "file b",
	}, contentTree(t, syncDir))
	assert.Equal(t, folderID, remoteItemID(t, cfgPath, env, "/proj-online"))
	assertSyncLeavesLocalTreeStable(t, cfgPath, env, syncDir, "sync")
}

// Validates: R-2.17.1, R-2.17.6
//
// Moves on both sides in one pass: a file moved into a folder locally while
// OneDrive moves another file out of it. Both moves land, nothing is copied
// or lost.
func TestE2E_Sync_CrossMoves(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
	writeLocal(t, syncDir, "top.txt", "top")
	writeLocal(t, syncDir, "box/inner.txt", "inner")
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
	waitForRemoteReadContains(t, cfgPath, env, "", "inner.txt", pollTimeout, "ls", "/box")
	runCLIWithConfig(t, cfgPath, env, "sync")

	require.NoError(t, os.Rename(filepath.Join(syncDir, "top.txt"), filepath.Join(syncDir, "box", "top.txt")))
	runCLIWithConfig(t, cfgPath, env, "mv", "/box/inner.txt", "/inner.txt")
	want := map[string]string{"box": "/", "box/top.txt": "top", "inner.txt": "inner"}
	syncUntil(t, cfgPath, env, "both moves land", func() bool {
		return assert.ObjectsAreEqual(want, contentTree(t, syncDir))
	})
	waitForRemoteReadContains(t, cfgPath, env, "", "top.txt", pollTimeout, "ls", "/box")
	assert.NotContains(t, remoteNames(t, cfgPath, env, "/"), "top.txt")
	assertSyncLeavesLocalTreeStable(t, cfgPath, env, syncDir, "sync")
}

// namingCorpus is names OneDrive and the client must carry unchanged: URL
// metacharacters, words Graph paths use, non-ASCII forms, emoji sequences and
// spacing. The NFD name differs from the NFC one by more than its form, so
// both can live in one folder.
func namingCorpus() []string {
	return []string{
		"50% off.txt", "a+b.txt", "%20literal.txt", "$cost.txt", "#hash.txt", "semi;colon.txt",
		"v1.0", "root", "delta", "children", "content",
		"straße.txt", "café nfc.txt", "café nfd.txt",
		"\U0001F469\u200D\U0001F4BB team.txt", "double  space.txt", "Test：1.jpg",
		"日本語/ファイル.txt",
	}
}

// Validates: R-2.11.4, R-2.12.2, R-2.12.3, R-2.13.2
//
// Each corpus name uploads, comes back from OneDrive's listing as itself (up
// to Unicode form), arrives under its own name on a second client, and a
// second sync of either changes nothing. Names OneDrive cannot hold (a
// trailing dot or space, a leading space) stay local, and two local names
// that differ only in case are both held back as a collision.
func TestE2E_Sync_NamingCorpusRoundtrip(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env, root := writeIsolatedSharedRootWriterConfig(t, syncDir, "", nil)
	for i, name := range namingCorpus() {
		writeLocal(t, syncDir, "corpus/"+name, fmt.Sprintf("corpus %d", i))
	}
	for _, name := range []string{"trailing-dot.", "trailing-space ", " leading-space.txt"} {
		writeLocal(t, syncDir, "rejected/"+name, "stays local")
	}
	writeLocal(t, syncDir, "cases/Mixed.txt", "upper")
	writeLocal(t, syncDir, "cases/mixed.txt", "lower")

	runCLIBestEffort(t, cfgPath, env, resolveDriveSelection(env, ""), "sync", "--upload-only")
	waitForRemoteReadContains(t, cfgPath, env, "", "children", pollTimeout, "ls", "/corpus")

	listed := map[string]bool{}
	for _, name := range remoteNames(t, cfgPath, env, "/corpus") {
		listed[norm.NFC.String(name)] = true
	}
	for _, name := range namingCorpus() {
		top := strings.SplitN(name, "/", 2)[0]
		assert.True(t, listed[norm.NFC.String(top)], "OneDrive lists %q", top)
	}
	nested := strings.Split(namingCorpus()[len(namingCorpus())-1], "/")
	assert.Contains(t, remoteNames(t, cfgPath, env, "/corpus/"+nested[0]), nested[1])

	assert.Empty(t, remoteNames(t, cfgPath, env, "/rejected"), "names OneDrive cannot hold are not uploaded")
	assert.Empty(t, remoteNames(t, cfgPath, env, "/cases"), "neither of two case-variant names is uploaded (R-2.12.2)")

	// Download direction: the same names arriving from OneDrive.
	otherDir := t.TempDir()
	otherCfg, otherEnv, _ := writeIsolatedSharedRootWriterConfig(t, otherDir, "", root)
	runCLIBestEffort(t, otherCfg, otherEnv, resolveDriveSelection(otherEnv, ""), "sync", "--download-only")
	downloaded := contentTree(t, filepath.Join(otherDir, "corpus"))
	for i, name := range namingCorpus() {
		found := false
		for rel, content := range downloaded {
			if norm.NFC.String(rel) == norm.NFC.String(name) {
				found = assert.Equal(t, fmt.Sprintf("corpus %d", i), content, "%q", name)
			}
		}
		assert.True(t, found, "%q arrives under its own name", name)
	}

	assertSyncLeavesLocalTreeStable(t, cfgPath, env, filepath.Join(syncDir, "corpus"), "sync")
	assertSyncLeavesLocalTreeStable(t, otherCfg, otherEnv, filepath.Join(otherDir, "corpus"), "sync")
}

// Validates: R-2.3.1, R-2.17.6
//
// Two clients on one folder (L-24 asks the same of three writers): a file
// one creates reaches the other; both edit one file between syncs and both
// versions survive on both, one as a conflict copy; renames by one and edits
// by the other compose; and the two trees end equal.
func TestE2E_Sync_TwoWriters(t *testing.T) {
	registerLogDump(t)

	dirA := t.TempDir()
	cfgA, envA, root := writeIsolatedSharedRootWriterConfig(t, dirA, "", nil)
	dirB := t.TempDir()
	cfgB, envB, _ := writeIsolatedSharedRootWriterConfig(t, dirB, "", root)

	writeLocal(t, dirA, "shared.txt", "from A")
	writeLocal(t, dirA, "doc/plan.txt", "plan v1")
	runCLIWithConfig(t, cfgA, envA, "sync")
	syncUntil(t, cfgB, envB, "B sees A's files", func() bool {
		return contentTree(t, dirB)["doc/plan.txt"] == "plan v1"
	})

	// Both edit shared.txt; A syncs first, B meets a remote change and its
	// own: both versions must survive.
	writeLocal(t, dirA, "shared.txt", "edited by A")
	writeLocal(t, dirB, "shared.txt", "edited by B")
	runCLIWithConfig(t, cfgA, envA, "sync")
	syncUntil(t, cfgB, envB, "B reconciles the concurrent edit", func() bool {
		return hasContent(contentTree(t, dirB), "edited by A") && hasContent(contentTree(t, dirB), "edited by B")
	})

	// A renames the folder while B edits a file inside it.
	require.NoError(t, os.Rename(filepath.Join(dirA, "doc"), filepath.Join(dirA, "docs-renamed")))
	writeLocal(t, dirB, "doc/plan.txt", "plan v2 by B")
	runCLIWithConfig(t, cfgA, envA, "sync")
	syncUntil(t, cfgB, envB, "B applies A's rename and uploads its edit", func() bool {
		return contentTree(t, dirB)["docs-renamed/plan.txt"] == "plan v2 by B"
	})

	syncUntil(t, cfgA, envA, "A converges with B", func() bool {
		return assert.ObjectsAreEqual(contentTree(t, dirB), contentTree(t, dirA))
	})
	for _, content := range []string{"edited by A", "edited by B", "plan v2 by B"} {
		assert.True(t, hasContent(contentTree(t, dirA), content), "%q survives", content)
	}
	assertSyncLeavesLocalTreeStable(t, cfgA, envA, dirA, "sync")
	assertSyncLeavesLocalTreeStable(t, cfgB, envB, dirB, "sync")
}

func hasContent(tree map[string]string, content string) bool {
	for _, have := range tree {
		if have == content {
			return true
		}
	}

	return false
}

// Validates: R-5.2.1, R-5.3.1, R-2.3.1
//
// A file spanning several upload-session fragments goes up through sync,
// comes down byte-identical on a second client, and an edit made there comes
// back.
func TestE2E_Sync_LargeFileBetweenClients(t *testing.T) {
	registerLogDump(t)

	dirA := t.TempDir()
	cfgA, envA, root := writeIsolatedSharedRootWriterConfig(t, dirA, "", nil)
	data := make([]byte, hardeningLargeFileSize)
	_, err := rand.Read(data)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dirA, "large.bin"), data, 0o600))
	runCLIWithConfig(t, cfgA, envA, "sync", "--upload-only")
	waitForRemoteStatSize(t, cfgA, envA, "", "/large.bin", int64(len(data)), hardeningSyncTimeout)

	dirB := t.TempDir()
	cfgB, envB, _ := writeIsolatedSharedRootWriterConfig(t, dirB, "", root)
	runCLIWithConfig(t, cfgB, envB, "sync", "--download-only")
	got := mustReadFile(t, filepath.Join(dirB, "large.bin"))
	assert.Equal(t, sha256.Sum256(data), sha256.Sum256(got), "the downloaded copy is byte-identical")

	edited := bytes.Clone(data)
	copy(edited[len(edited)/2:], "edited in the middle by B")
	require.NoError(t, os.WriteFile(filepath.Join(dirB, "large.bin"), edited, 0o600))
	runCLIWithConfig(t, cfgB, envB, "sync")
	syncUntil(t, cfgA, envA, "A downloads B's edit", func() bool {
		back, readErr := os.ReadFile(filepath.Join(dirA, "large.bin")) //nolint:gosec // a file inside the test's own sync dir
		return readErr == nil && bytes.Equal(back, edited)
	})
}

// Validates: R-2.17.10
//
// A file replaced by a folder locally becomes a folder on OneDrive, and a
// folder replaced by a file on OneDrive becomes a file locally, with nothing
// left of the old type on either side.
func TestE2E_Sync_TypeChangesBothWays(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
	writeLocal(t, syncDir, "was-file", "plain file")
	writeLocal(t, syncDir, "was-folder/child.txt", "child")
	runCLIWithConfig(t, cfgPath, env, "sync", "--upload-only")
	waitForRemoteReadContains(t, cfgPath, env, "", "child.txt", pollTimeout, "ls", "/was-folder")
	runCLIWithConfig(t, cfgPath, env, "sync")

	// Local: file -> folder.
	require.NoError(t, os.Remove(filepath.Join(syncDir, "was-file")))
	writeLocal(t, syncDir, "was-file/inside.txt", "now a folder")
	runCLIWithConfig(t, cfgPath, env, "sync")
	waitForRemoteReadContains(t, cfgPath, env, "", "inside.txt", pollTimeout, "ls", "/was-file")

	// OneDrive: folder -> file.
	runCLIWithConfig(t, cfgPath, env, "rm", "-r", "/was-folder")
	waitForRemoteDeleteDisappearance(t, cfgPath, env, "", "was-folder", "ls", "/")
	putRemoteFile(t, cfgPath, env, "/was-folder", "now a file")
	syncUntil(t, cfgPath, env, "the remote type change reaches the local tree", func() bool {
		return contentTree(t, syncDir)["was-folder"] == "now a file"
	})

	assert.Equal(t, map[string]string{
		"was-file": "/", "was-file/inside.txt": "now a folder", "was-folder": "now a file",
	}, contentTree(t, syncDir))
	assertSyncLeavesLocalTreeStable(t, cfgPath, env, syncDir, "sync")
}
