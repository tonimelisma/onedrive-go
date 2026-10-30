package multisync

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireTrigger(t *testing.T, triggers <-chan string, want string) {
	t.Helper()

	select {
	case got := <-triggers:
		assert.Equal(t, want, got)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no reload trigger for "+want)
	}
}

func requireNoTrigger(t *testing.T, triggers <-chan string, wait time.Duration) {
	t.Helper()

	select {
	case got := <-triggers:
		require.FailNow(t, "unexpected reload trigger: "+got)
	case <-time.After(wait):
	}
}

// Regression: a running watch reloaded the config only on control-socket
// requests, so hand edits (ignored_dirs, websocket = true) were silently
// ignored until restart.
//
// Validates: R-4.4.3
func TestWatchConfigReloadTriggers_FileEditsAndSIGHUP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("# v1\n"), 0o600))

	triggers, stop := watchConfigReloadTriggers(t.Context(), path, 20*time.Millisecond)
	defer stop()

	requireNoTrigger(t, triggers, 100*time.Millisecond)

	require.NoError(t, os.WriteFile(path, []byte("# v2, longer\n"), 0o600))
	requireTrigger(t, triggers, "config file changed")
	requireNoTrigger(t, triggers, 100*time.Millisecond)

	// An atomic replace with the same size still counts (new inode).
	replacement := path + ".tmp"
	require.NoError(t, os.WriteFile(replacement, []byte("# v3, longer\n"), 0o600))
	require.NoError(t, os.Rename(replacement, path))
	requireTrigger(t, triggers, "config file changed")

	// A brief disappearance does not reload until the file is back.
	require.NoError(t, os.Remove(path))
	requireNoTrigger(t, triggers, 100*time.Millisecond)
	require.NoError(t, os.WriteFile(path, []byte("# v4\n"), 0o600))
	requireTrigger(t, triggers, "config file changed")

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGHUP))
	requireTrigger(t, triggers, "SIGHUP")
}

// Regression: a poll landing between an in-place write's truncate and its
// write reloaded the empty file, warned that it did not parse, and reloaded
// again once the write finished (stress-run flake of the test above).
//
// Validates: R-4.4.3
func TestConfigFilePoll_ReloadsOnlySettledVersions(t *testing.T) {
	t.Parallel()

	v1 := configFileStamp{exists: true, size: 5, mtime: time.Unix(1, 0), inode: 7}
	truncated := configFileStamp{exists: true, size: 0, mtime: time.Unix(2, 0), inode: 7}
	v2 := configFileStamp{exists: true, size: 13, mtime: time.Unix(2, 0), inode: 7}

	poll := configFilePoll{last: v1}
	assert.False(t, poll.observe(v1), "an unchanged file never reloads")
	assert.False(t, poll.observe(truncated), "a first sighting waits one poll")
	assert.False(t, poll.observe(v2), "a version that changed again waits one more poll")
	assert.True(t, poll.observe(v2), "a version seen on two polls reloads")
	assert.False(t, poll.observe(v2), "a reloaded version does not reload again")

	assert.False(t, poll.observe(configFileStamp{}), "a missing file never reloads")
	assert.False(t, poll.observe(v2), "the same version back after a disappearance changed nothing")

	poll.reset(v1)
	assert.False(t, poll.observe(v2))
	assert.True(t, poll.observe(v2), "after SIGHUP reloaded v1, v2 is new again")
}
