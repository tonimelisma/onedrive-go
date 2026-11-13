//go:build e2e && e2e_full

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Kill-mid-transfer scenarios: SIGKILL a sync during a chunked upload and
// during a download, then show the next run resuming where the dead process
// stopped instead of starting over.
// ---------------------------------------------------------------------------

const (
	// killMidUploadSize spans three 10 MiB fragments, so the kill lands after
	// the first fragment is accepted and before the session is committed.
	killMidUploadSize = 25 * 1024 * 1024
	// killMidDownloadSize keeps one download in flight long enough for the
	// poll on the partial to see bytes arrive before the file is complete.
	killMidDownloadSize = 48 * 1024 * 1024
	// killMidTransferTimeout bounds the wait for the moment to kill at.
	killMidTransferTimeout = 3 * time.Minute

	stderrUploadChunkAccepted   = "intermediate chunk accepted"
	stderrUploadSessionResumed  = "resuming upload from offset"
	stderrUploadSessionExpired  = "upload session expired"
	stderrDownloadPartialResume = "resuming download from partial file"
	uploadSessionsSubdir        = "upload-sessions"
	downloadPartialGlob         = ".onedrive-go.*.partial"
)

// killMidTransferData fills size bytes with a cheap deterministic pattern
// that differs per seed, so the two scenarios never share content.
func killMidTransferData(size int, seed byte) []byte {
	data := make([]byte, size)
	x := uint32(seed) + 1
	for i := range data {
		x = x*1664525 + 1013904223
		data[i] = byte(x >> 24)
	}

	return data
}

// Validates: R-5.2.3, R-5.2.4
//
// A sync killed with SIGKILL after the first fragment of a chunked upload
// leaves the persisted upload session behind; the next sync resumes that
// session from the accepted offset, the file lands whole and hash-verified,
// and a third sync has nothing to do.
func TestE2E_Sync_KillMidUploadResumesSession(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-kill-upload-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })
	cfgPath, env := writeSyncConfigForFolder(t, syncDir, testFolder)

	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	data := killMidTransferData(killMidUploadSize, 1)
	localPath := filepath.Join(localDir, "kill-upload.bin")
	require.NoError(t, os.WriteFile(localPath, data, 0o600))

	first := startKillableSync(t, cfgPath, env, "sync", "--upload-only")
	waitForStderrContains(t, first.Stderr, stderrUploadChunkAccepted, killMidTransferTimeout)
	killSyncProcess(t, first.Cmd)
	t.Logf("killed the upload after %d accepted fragment(s)",
		strings.Count(first.Stderr.String(), stderrUploadChunkAccepted))

	sessions, err := filepath.Glob(filepath.Join(env["XDG_DATA_HOME"], "onedrive-go", uploadSessionsSubdir, "*"))
	require.NoError(t, err)
	require.NotEmpty(t, sessions, "the killed upload left no persisted upload session")

	// Single pass by design: the very next pass must resume the dead sync's
	// upload session; it may leave work a transient failure causes.
	_, stderr := runSyncPass(t, cfgPath, env, "--upload-only")
	assert.Contains(t, stderr, stderrUploadSessionResumed, "the second sync did not resume the dead sync's upload session")
	assert.NotContains(t, stderr, stderrUploadSessionExpired)

	remotePath := "/" + testFolder + "/kill-upload.bin"
	waitForRemoteStatSize(t, opsCfgPath, nil, remotePath, int64(killMidUploadSize), pollTimeout)
	assertRemoteContentEquals(t, opsCfgPath, remotePath, data)

	// Single pass by design: the one pass after the resumed upload must find
	// nothing to do, so it leaves nothing ("Up to date").
	out, _ := runSyncPass(t, cfgPath, env, "--upload-only")
	assert.Contains(t, out, "Up to date", "a resumed upload should leave nothing for the next sync")
}

// Validates: R-5.2.2, R-5.2.4
//
// A sync killed with SIGKILL while a download is writing its `.partial` file
// leaves the partial behind; the next sync resumes from the bytes it holds
// through a Range request, publishes the whole file with the right content,
// and removes the partial.
func TestE2E_Sync_KillMidDownloadResumesPartial(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-kill-download-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })
	cfgPath, env := writeSyncConfigForFolder(t, syncDir, testFolder)
	mkdirRemoteFolder(t, opsCfgPath, nil, "/"+testFolder)

	data := killMidTransferData(killMidDownloadSize, 2)
	srcPath := filepath.Join(t.TempDir(), "kill-download.bin")
	require.NoError(t, os.WriteFile(srcPath, data, 0o600))
	remotePath := "/" + testFolder + "/kill-download.bin"
	_, stderr := runCLIWithConfig(t, opsCfgPath, nil, "put", srcPath, remotePath)
	assert.Contains(t, stderr, "Uploaded")
	waitForRemoteStatSize(t, opsCfgPath, nil, remotePath, int64(killMidDownloadSize), pollTimeout)

	localDir := filepath.Join(syncDir, testFolder)
	targetPath := filepath.Join(localDir, "kill-download.bin")

	first := startKillableSync(t, cfgPath, env, "sync", "--download-only")
	partialPath, partialSize := waitForDownloadPartial(t, localDir, killMidTransferTimeout)
	killSyncProcess(t, first.Cmd)
	t.Logf("killed the download with %d bytes in %s", partialSize, filepath.Base(partialPath))

	_, statErr := os.Stat(targetPath)
	require.True(t, os.IsNotExist(statErr),
		"the download finished before the kill landed; this link is too fast for a %d byte file", killMidDownloadSize)
	info, err := os.Stat(partialPath)
	require.NoError(t, err, "the killed download left no partial behind")
	require.Positive(t, info.Size())

	// Single pass by design: the very next pass must resume from the dead
	// sync's partial; it may leave work a transient failure causes.
	_, stderr = runSyncPass(t, cfgPath, env, "--download-only")
	assert.Contains(t, stderr, stderrDownloadPartialResume, "the second sync did not resume from the dead sync's partial")

	assert.Equal(t, data, mustReadFile(t, targetPath), "the resumed download does not match the uploaded content")
	leftovers, err := filepath.Glob(filepath.Join(localDir, downloadPartialGlob))
	require.NoError(t, err)
	assert.Empty(t, leftovers, "the partial should be gone once the download is published")
}

// writeSyncConfigForFolder is writeSyncConfig narrowed to one drive folder
// with included_dirs, so a test's syncs move only their own folder and a
// download poll cannot mistake another folder's file for the one under test.
func writeSyncConfigForFolder(t *testing.T, syncDir, folder string) (string, map[string]string) {
	t.Helper()

	perTestData := t.TempDir()
	perTestHome := t.TempDir()

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	require.NoError(t, os.MkdirAll(perTestDataDir, 0o700))
	copyTokenFile(t, testDataDir, perTestDataDir)

	content := fmt.Sprintf("[%q]\nsync_dir = %q\nincluded_dirs = [%q]\n", drive, syncDir, folder)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

	return cfgPath, map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}
}

// killableSync is a sync subprocess the test may SIGKILL at a moment of its
// choosing; Stderr collects the --debug log the moment is read from.
type killableSync struct {
	Cmd    *exec.Cmd
	Stderr *syncBuffer
}

// startKillableSync starts the suite binary with --debug and returns before
// it finishes, so the test can watch its stderr and kill it mid-transfer.
func startKillableSync(t *testing.T, cfgPath string, env map[string]string, args ...string) *killableSync {
	t.Helper()

	fullArgs := []string{"--config", cfgPath, "--debug"}
	fullArgs = append(fullArgs, args...)
	cmd := makeCmd(fullArgs, env)

	var stdout syncBuffer
	stderr := &syncBuffer{}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr

	require.NoError(t, cmd.Start(), "failed to start sync subprocess")

	t.Cleanup(func() {
		killSyncProcess(t, cmd)
		logCLIExecution(t, fullArgs, stdout.String(), stderr.String())
	})

	return &killableSync{Cmd: cmd, Stderr: stderr}
}

// killSyncProcess sends SIGKILL and reaps the process; a process that has
// already been reaped is left alone.
func killSyncProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()

	if cmd.Process == nil || cmd.ProcessState != nil {
		return
	}
	if err := cmd.Process.Kill(); err != nil && !strings.Contains(err.Error(), "process already finished") {
		t.Logf("killing sync subprocess: %v", err)
	}
	waitForDaemonExit(t, cmd)
}

// waitForStderrContains polls the subprocess's stderr until needle appears,
// finely enough that the kill lands within the fragment that follows.
func waitForStderrContains(t *testing.T, buf *syncBuffer, needle string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for !strings.Contains(buf.String(), needle) {
		require.Less(t, time.Now(), deadline, "stderr never showed %q within %s:\n%s", needle, timeout, buf.String())
		sleepForLiveTestPropagation(10 * time.Millisecond)
	}
}

// waitForDownloadPartial polls dir for a download partial with bytes in it
// and returns its path and the size seen.
func waitForDownloadPartial(t *testing.T, dir string, timeout time.Duration) (string, int64) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		matches, err := filepath.Glob(filepath.Join(dir, downloadPartialGlob))
		require.NoError(t, err)
		for _, match := range matches {
			if info, statErr := os.Stat(match); statErr == nil && info.Size() > 0 {
				return match, info.Size()
			}
		}
		require.Less(t, time.Now(), deadline, "no download partial with bytes appeared in %s within %s", dir, timeout)
		sleepForLiveTestPropagation(5 * time.Millisecond)
	}
}

// assertRemoteContentEquals downloads remotePath with the ops config, which
// verifies OneDrive's hash on the way, and compares the bytes with want.
func assertRemoteContentEquals(t *testing.T, opsCfgPath, remotePath string, want []byte) {
	t.Helper()

	localPath := filepath.Join(t.TempDir(), "remote-copy.bin")
	stdout, _ := runCLIWithConfig(t, opsCfgPath, nil, "--json", "get", remotePath, localPath)

	var got struct {
		HashVerified bool `json:"hash_verified"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), "get --json output: %s", stdout)
	assert.True(t, got.HashVerified, "OneDrive's hash of the resumed upload did not verify")
	assert.Equal(t, want, mustReadFile(t, localPath), "the content on OneDrive differs from the local file")
}
