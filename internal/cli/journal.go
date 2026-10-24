package cli

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// journalPriority values are the syslog levels systemd-journald reads from a
// "<N>" line prefix on a service's stdout/stderr (sd-daemon(3)).
const (
	journalPriorityErr     = 3
	journalPriorityWarning = 4
	journalPriorityInfo    = 6
	journalPriorityDebug   = 7
)

// fileIsJournalStream reports whether f is the stream systemd connected to the
// journal. systemd sets $JOURNAL_STREAM to the "device:inode" of the stream it
// hands a service; comparing it with the file's own identity is the documented
// check (systemd.exec(5)), and it fails safely when the variable was inherited
// by a process whose output was redirected elsewhere.
func fileIsJournalStream(f *os.File, journalStream string) bool {
	if f == nil || journalStream == "" {
		return false
	}

	devText, inoText, ok := strings.Cut(journalStream, ":")
	if !ok {
		return false
	}

	dev, devErr := strconv.ParseUint(devText, 10, 64)
	ino, inoErr := strconv.ParseUint(inoText, 10, 64)
	if devErr != nil || inoErr != nil {
		return false
	}

	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return false
	}

	//nolint:unconvert // Stat_t field widths differ by platform; both are identifiers, not quantities.
	return uint64(st.Dev) == dev && uint64(st.Ino) == ino
}
