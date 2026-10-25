// Package sdnotify sends systemd service notifications (sd_notify(3)) for a
// Type=notify unit: readiness, one-line status, reload and stop transitions,
// and watchdog keepalives. It owns the process's only access to
// $NOTIFY_SOCKET and the watchdog environment, and talks the datagram
// protocol directly with the standard library instead of linking libsystemd.
//
// A nil *Notifier is valid and does nothing, so callers outside systemd (or
// in tests) need no branches.
package sdnotify

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Environment variables systemd sets for the service process.
const (
	EnvNotifySocket = "NOTIFY_SOCKET"
	EnvWatchdogUSec = "WATCHDOG_USEC"
	EnvWatchdogPID  = "WATCHDOG_PID"
)

// watchdogPingDivisor follows sd_watchdog_enabled(3): ping at half the
// configured timeout so one late ping does not trip the watchdog.
const watchdogPingDivisor = 2

// writeTimeout bounds one datagram send. The notify socket is local and
// systemd drains it promptly; a stuck manager must not stall the sync loop.
const writeTimeout = 2 * time.Second

// ErrUnsupportedSocket reports a $NOTIFY_SOCKET address this implementation
// does not speak (anything but an absolute path or an abstract "@name";
// systemd's "vsock:" addresses are for VMs and are not used by user units).
var ErrUnsupportedSocket = errors.New("sdnotify: unsupported NOTIFY_SOCKET address")

// Notifier sends notifications to the service manager. It is safe for
// concurrent use: each message is one datagram write on a connected socket.
type Notifier struct {
	conn *net.UnixConn
	// watchdogInterval is how often to send WATCHDOG=1; zero when the unit
	// has no WatchdogSec= or the watchdog is meant for another process.
	watchdogInterval time.Duration
	// monotonic reads CLOCK_MONOTONIC for RELOADING's MONOTONIC_USEC.
	monotonic func() (time.Duration, error)
}

// Open connects to the service manager named by $NOTIFY_SOCKET. It returns
// (nil, nil) when the variable is unset, so a run outside systemd gets the
// no-op nil Notifier. pid is this process's ID, checked against
// $WATCHDOG_PID so a watchdog meant for a parent is not answered by a child.
func Open(getenv func(string) string, pid int) (*Notifier, error) {
	address := getenv(EnvNotifySocket)
	if address == "" {
		//nolint:nilnil // The nil *Notifier is the documented no-op outside systemd.
		return nil, nil
	}

	// An abstract socket is "@name"; net maps a leading '@' to the
	// abstract namespace's leading NUL byte on Linux.
	if !strings.HasPrefix(address, "/") && !strings.HasPrefix(address, "@") {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedSocket, address)
	}

	interval, err := watchdogInterval(getenv, pid)
	if err != nil {
		return nil, err
	}

	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: address, Net: "unixgram"})
	if err != nil {
		return nil, fmt.Errorf("sdnotify: connect %s: %w", address, err)
	}

	return &Notifier{conn: conn, watchdogInterval: interval, monotonic: monotonicNow}, nil
}

func watchdogInterval(getenv func(string) string, pid int) (time.Duration, error) {
	rawUSec := getenv(EnvWatchdogUSec)
	if rawUSec == "" {
		return 0, nil
	}

	if rawPID := getenv(EnvWatchdogPID); rawPID != "" {
		watchdogPID, err := strconv.Atoi(rawPID)
		if err != nil {
			return 0, fmt.Errorf("sdnotify: parse %s %q: %w", EnvWatchdogPID, rawPID, err)
		}
		if watchdogPID != pid {
			return 0, nil
		}
	}

	usec, err := strconv.ParseUint(rawUSec, 10, 63)
	if err != nil || usec == 0 {
		return 0, fmt.Errorf("sdnotify: parse %s %q: invalid timeout", EnvWatchdogUSec, rawUSec)
	}

	return time.Duration(usec) * time.Microsecond / watchdogPingDivisor, nil
}

func monotonicNow() (time.Duration, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, fmt.Errorf("sdnotify: read monotonic clock: %w", err)
	}

	return time.Duration(ts.Nano()), nil
}

// WatchdogInterval is how often the caller should send Watchdog, or zero when
// no watchdog is expected.
func (n *Notifier) WatchdogInterval() time.Duration {
	if n == nil {
		return 0
	}

	return n.watchdogInterval
}

// Ready reports that startup finished, with a one-line status.
func (n *Notifier) Ready(status string) error {
	return n.send("READY=1", status)
}

// Status updates the one-line status `systemctl status` shows.
func (n *Notifier) Status(status string) error {
	return n.send("", status)
}

// Reloading reports that a configuration reload started. systemd requires
// MONOTONIC_USEC with RELOADING=1 for Type=notify-reload, and accepts it for
// Type=notify; Ready must follow once the reload finished, whatever its
// outcome.
func (n *Notifier) Reloading(status string) error {
	if n == nil {
		return nil
	}

	now, err := n.monotonic()
	if err != nil {
		return err
	}

	return n.send("RELOADING=1\nMONOTONIC_USEC="+strconv.FormatInt(now.Microseconds(), 10), status)
}

// Stopping reports that shutdown started.
func (n *Notifier) Stopping(status string) error {
	return n.send("STOPPING=1", status)
}

// Watchdog sends one keepalive.
func (n *Notifier) Watchdog() error {
	return n.send("WATCHDOG=1", "")
}

// Close releases the socket. Notifications after Close fail.
func (n *Notifier) Close() error {
	if n == nil {
		return nil
	}

	if err := n.conn.Close(); err != nil {
		return fmt.Errorf("sdnotify: close: %w", err)
	}

	return nil
}

func (n *Notifier) send(fields, status string) error {
	if n == nil {
		return nil
	}

	message := fields
	if status != "" {
		if message != "" {
			message += "\n"
		}
		// STATUS= is one line; a newline would start a new assignment.
		message += "STATUS=" + strings.Join(strings.Fields(status), " ")
	}

	if err := n.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("sdnotify: set write deadline: %w", err)
	}
	if _, err := n.conn.Write([]byte(message)); err != nil {
		return fmt.Errorf("sdnotify: send: %w", err)
	}

	return nil
}
