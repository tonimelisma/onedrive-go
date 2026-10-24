package retry

import (
	"fmt"
	"sync"
	"time"
)

// Notices remembers which retry announcements have already been shown, so a
// condition that every in-flight request hits at once (OneDrive unreachable,
// a long rate-limit wait) prints one line per episode instead of one per
// request (R-6.6.8). Share one Notices across every RetryTransport a process
// builds; the zero value is ready to use.
//
// An episode ends on evidence that the condition is over: any HTTP response
// ends an outage, and a successful (2xx) response ends a throttling episode.
// The next occurrence after that is announced again.
type Notices struct {
	// mu guards unreachableAnnounced and longWaitAnnounced. Transports of
	// several concurrent workers read and flip them; nothing is done under
	// the lock beyond the flag update.
	mu sync.Mutex
	// unreachableAnnounced is true from the first "can't reach OneDrive"
	// warning until some request receives an HTTP response.
	unreachableAnnounced bool
	// longWaitAnnounced is true from the first long-wait warning until some
	// request succeeds.
	longWaitAnnounced bool
}

// claimUnreachable reports whether the caller should announce an outage: true
// for the first caller of an episode, false for everyone after it.
func (n *Notices) claimUnreachable() bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.unreachableAnnounced {
		return false
	}

	n.unreachableAnnounced = true

	return true
}

// claimLongWait reports whether the caller should announce a long retry wait:
// true for the first caller of a throttling episode only.
func (n *Notices) claimLongWait() bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.longWaitAnnounced {
		return false
	}

	n.longWaitAnnounced = true

	return true
}

// markReachable ends the current outage episode: OneDrive answered.
func (n *Notices) markReachable() {
	n.mu.Lock()
	n.unreachableAnnounced = false
	n.mu.Unlock()
}

// markSucceeded ends both episodes: a request went through.
func (n *Notices) markSucceeded() {
	n.mu.Lock()
	n.unreachableAnnounced = false
	n.longWaitAnnounced = false
	n.mu.Unlock()
}

// HumanDuration renders a wait the way the output guidelines ask for at
// default verbosity: "2 s", "1 min 30 s", "5 min", rounded to whole seconds
// (and never below 1 s, so a short wait does not read as "0 s").
func HumanDuration(d time.Duration) string {
	seconds := int64(d.Round(time.Second) / time.Second)
	if seconds < 1 {
		seconds = 1
	}

	const secondsPerMinute = 60
	minutes, rest := seconds/secondsPerMinute, seconds%secondsPerMinute

	switch {
	case minutes == 0:
		return fmt.Sprintf("%d s", rest)
	case rest == 0:
		return fmt.Sprintf("%d min", minutes)
	default:
		return fmt.Sprintf("%d min %d s", minutes, rest)
	}
}
