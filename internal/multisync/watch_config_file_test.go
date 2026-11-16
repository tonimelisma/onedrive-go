package multisync

import (
	"os"
	"path/filepath"
	gosync "sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// configPollsToSettle is how many polls must read a new version before it
// reloads (configFilePoll), so a check that no reload happens ends at least
// that many polls: one could not show a reload that should not happen.
const configPollsToSettle = 2

// endConfigPolls ends polls polls of a watcher whose ticks channel is
// unbuffered. The watcher takes a tick only between polls, so once polls+1
// ticks are taken the first polls of them have ended, any trigger they sent
// included.
func endConfigPolls(t *testing.T, ticks chan<- time.Time, polls int) {
	t.Helper()

	for range polls + 1 {
		select {
		case ticks <- time.Time{}:
		case <-time.After(eventWait):
			require.FailNow(t, "the config watcher stopped taking ticks")
		}
	}
}

// requireTrigger ticks the watcher until it sends a reload trigger, and
// requires that trigger to be want.
func requireTrigger(t *testing.T, triggers <-chan string, ticks chan<- time.Time, want string) {
	t.Helper()

	deadline := time.After(eventWait)
	for {
		select {
		case got := <-triggers:
			assert.Equal(t, want, got)
			return
		case ticks <- time.Time{}:
		case <-deadline:
			require.FailNow(t, "no reload trigger for "+want)
		}
	}
}

// requireNoTrigger ends configPollsToSettle polls and requires that none of
// them sent a reload trigger. A poll still running when it returns may read
// the file the test writes next; that changes nothing a later wait reads.
func requireNoTrigger(t *testing.T, triggers <-chan string, ticks chan<- time.Time) {
	t.Helper()

	endConfigPolls(t, ticks, configPollsToSettle)
	select {
	case got := <-triggers:
		require.FailNow(t, "unexpected reload trigger: "+got)
	default:
	}
}

// Regression: a running watch reloaded the config only on control-socket
// requests, so hand edits (ignored_dirs, websocket = true) were silently
// ignored until restart. The test drives the polls itself, so a check that
// an unchanged file does not reload reads the triggers of polls that ended.
//
// Validates: R-4.4.3, R-6.10.10
func TestWatchConfigReloadTriggers_FileEditsAndSIGHUP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("# v1\n"), 0o600))

	ticks := make(chan time.Time)
	triggers, stop := watchConfigReloadTriggers(t.Context(), configReloadWatch{path: path, ticks: ticks})
	defer stop()

	requireNoTrigger(t, triggers, ticks)

	require.NoError(t, os.WriteFile(path, []byte("# v2, longer\n"), 0o600))
	requireTrigger(t, triggers, ticks, "config file changed")
	requireNoTrigger(t, triggers, ticks)

	// An atomic replace with the same size still counts (new inode).
	replacement := path + ".tmp"
	require.NoError(t, os.WriteFile(replacement, []byte("# v3, longer\n"), 0o600))
	require.NoError(t, os.Rename(replacement, path))
	requireTrigger(t, triggers, ticks, "config file changed")

	// A brief disappearance does not reload until the file is back.
	require.NoError(t, os.Remove(path))
	requireNoTrigger(t, triggers, ticks)
	require.NoError(t, os.WriteFile(path, []byte("# v4\n"), 0o600))
	requireTrigger(t, triggers, ticks, "config file changed")

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGHUP))
	requireTrigger(t, triggers, ticks, "SIGHUP")
}

// A watch injects no ticks, so its watcher polls on a ticker of its interval.
//
// Validates: R-4.4.3
func TestWatchConfigReloadTriggers_PollsOnItsIntervalWithoutInjectedTicks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("# v1\n"), 0o600))

	// The first read is the version the watcher starts from; an edit before
	// it would be that version and never reload.
	firstRead := make(chan struct{})
	signalFirstRead := gosync.OnceFunc(func() { close(firstRead) })
	triggers, stop := watchConfigReloadTriggers(t.Context(), configReloadWatch{
		path:     path,
		interval: time.Millisecond,
		readStamp: func(p string) configFileStamp {
			stamp := readConfigFileStamp(p)
			signalFirstRead()
			return stamp
		},
	})
	defer stop()

	select {
	case <-firstRead:
	case <-time.After(eventWait):
		require.FailNow(t, "the config watcher did not read the version it starts from")
	}
	require.NoError(t, os.WriteFile(path, []byte("# v2, longer\n"), 0o600))

	select {
	case got := <-triggers:
		assert.Equal(t, "config file changed", got)
	case <-time.After(eventWait):
		require.FailNow(t, "no reload trigger for config file changed")
	}
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
