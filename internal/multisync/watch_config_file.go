package multisync

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// configFilePollInterval is how often a running watch checks config.toml for
// edits. Reload already diffs mounts, so an edit that changes nothing a mount
// cares about restarts nothing.
const configFilePollInterval = 5 * time.Second

// configFileStamp identifies one version of the config file on disk. Atomic
// writes replace the file, so the inode changes even when size and mtime
// happen to match.
type configFileStamp struct {
	exists bool
	size   int64
	mtime  time.Time
	inode  uint64
}

func readConfigFileStamp(path string) configFileStamp {
	info, err := localpath.Stat(path)
	if err != nil {
		return configFileStamp{}
	}

	stamp := configFileStamp{exists: true, size: info.Size(), mtime: info.ModTime()}
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		stamp.inode = sys.Ino
	}

	return stamp
}

// watchConfigReloadTriggers delivers one value whenever config.toml changes
// on disk and the change has settled (configFilePoll), or the process
// receives SIGHUP (systemd's ExecReload). Before this,
// only control-socket requests (pause, resume, drive add) reloaded the config:
// a hand edit such as new ignored_dirs or websocket = true was silently
// ignored until the watch restarted.
func watchConfigReloadTriggers(ctx context.Context, path string, interval time.Duration) (<-chan string, func()) {
	triggers := make(chan string, 1)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		poll := configFilePoll{last: readConfigFileStamp(path)}
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				poll.reset(readConfigFileStamp(path))
				sendLatest(triggers, "SIGHUP")
			case <-ticker.C:
				if path == "" {
					continue
				}
				if poll.observe(readConfigFileStamp(path)) {
					sendLatest(triggers, "config file changed")
				}
			}
		}
	}()

	return triggers, func() {
		signal.Stop(hup)
		cancel()
		<-done
	}
}

// configFilePoll decides, one poll at a time, when an edit is complete. An
// editor or os.WriteFile truncates and then writes in place, so a poll can
// land between the two; reloading there parsed an empty or half-written file,
// warned, and reloaded again on the next poll. A new version must therefore
// read the same on two consecutive polls before it counts.
type configFilePoll struct {
	last    configFileStamp
	pending configFileStamp
}

// observe records one poll and reports whether to reload.
func (p *configFilePoll) observe(current configFileStamp) bool {
	// A missing file is a write in progress or a mistake; reload once it is
	// back rather than on the disappearance.
	if !current.exists || current == p.last {
		p.pending = configFileStamp{}
		return false
	}
	if current != p.pending {
		p.pending = current
		return false
	}

	p.reset(current)

	return true
}

// reset makes current the loaded version, as after a reload.
func (p *configFilePoll) reset(current configFileStamp) {
	p.last = current
	p.pending = configFileStamp{}
}

// sendLatest coalesces triggers: one pending reload covers every change
// before it runs.
func sendLatest(ch chan string, reason string) {
	select {
	case ch <- reason:
	default:
	}
}
