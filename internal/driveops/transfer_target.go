package driveops

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// TargetFS is the filesystem capability a download writes through: its
// partial file, the rename over the target and the sync of the parent
// directory. Names are whatever the implementation resolves (absolute paths
// for the CLI, sync-root-relative paths for sync).
//
// Sync implements it over its rooted sync tree and never creates parents:
// the old download path ran MkdirAll on the target's parent and used
// absolute paths for every write, so a local rename between planning and
// dispatch re-created an empty folder holding the download, and a parent
// swapped for a symlink redirected the write outside the sync root (D-043).
type TargetFS interface {
	// PrepareParent makes sure dir can hold the download. The CLI creates
	// it; sync requires that it already exists as the plan observed it.
	PrepareParent(dir string) error
	OpenFile(name string, flag int, perm os.FileMode) (*os.File, error)
	Open(name string) (*os.File, error)
	Stat(name string) (os.FileInfo, error)
	Remove(name string) error
	Rename(oldName, newName string) error
	Chtimes(name string, atime, mtime time.Time) error
	// SyncDir flushes dir's entries, making a rename into it durable.
	SyncDir(dir string) error
	// Abs is the absolute path of name, for free-space checks and messages.
	Abs(name string) (string, error)
}

// localTarget is the CLI's TargetFS: arbitrary local paths through
// localpath, creating missing parents.
type localTarget struct{}

// LocalTarget returns the TargetFS for arbitrary local paths (CLI get). It
// creates missing parent directories with 0777 minus the umask.
func LocalTarget() TargetFS { return localTarget{} }

// newDirPerms is the mode requested for a created parent; the kernel applies
// the umask, as for any other program creating a directory (P-8).
const newDirPerms = 0o777

func (localTarget) PrepareParent(dir string) error {
	if err := localpath.MkdirAll(dir, newDirPerms); err != nil {
		return fmt.Errorf("creating parent dir %s: %w", dir, err)
	}

	return nil
}

func (localTarget) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return localpath.OpenFile(name, flag, perm) //nolint:wrapcheck // localpath already names the path
}

func (localTarget) Open(name string) (*os.File, error) {
	return localpath.Open(name) //nolint:wrapcheck // localpath already names the path
}

func (localTarget) Stat(name string) (os.FileInfo, error) {
	return localpath.Stat(name) //nolint:wrapcheck // localpath already names the path
}

func (localTarget) Remove(name string) error {
	return localpath.Remove(name) //nolint:wrapcheck // localpath already names the path
}

func (localTarget) Rename(oldName, newName string) error {
	return localpath.Rename(oldName, newName) //nolint:wrapcheck // localpath already names the paths
}

func (localTarget) Chtimes(name string, atime, mtime time.Time) error {
	return localpath.Chtimes(name, atime, mtime) //nolint:wrapcheck // localpath already names the path
}

func (localTarget) SyncDir(dir string) error {
	return localpath.SyncDir(dir) //nolint:wrapcheck // localpath already names the path
}

func (localTarget) Abs(name string) (string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", name, err)
	}

	return abs, nil
}

// newFileMode is the mode for a downloaded file that replaces nothing:
// 0666 minus the process umask, what any other program would create (P-8).
// Partials stay 0600 while they are written, because they may hold personal
// data before the rename publishes them. The umask is probed once per
// TransferManager.
func (tm *TransferManager) newFileMode() os.FileMode {
	tm.umaskOnce.Do(func() { tm.umask = probeUmask() })

	return newFilePerms &^ tm.umask
}

// newFilePerms is the mode requested for a new file before the umask.
const newFilePerms = 0o666

// defaultUmask is assumed when the umask cannot be read.
const defaultUmask = 0o022

// procSelfStatus is where Linux reports the process umask without changing
// it ("Umask:\t0022", Linux 4.7+).
const procSelfStatus = "/proc/self/status"

// probeUmask reads the umask without changing it: syscall.Umask can only be
// read by setting it, which races every other goroutine creating files. On
// Linux /proc/self/status reports it; elsewhere a file created with mode
// 0666 carries exactly 0666 minus the umask.
func probeUmask() os.FileMode {
	if status, err := localpath.ReadFile(procSelfStatus); err == nil {
		if umask, ok := parseProcUmask(string(status)); ok {
			return umask
		}
	}

	return probeUmaskWithFile()
}

// parseProcUmask finds the "Umask:" line of /proc/self/status.
func parseProcUmask(status string) (os.FileMode, bool) {
	for _, line := range strings.Split(status, "\n") {
		value, found := strings.CutPrefix(line, "Umask:")
		if !found {
			continue
		}

		umask, err := strconv.ParseUint(strings.TrimSpace(value), 8, 32)
		if err != nil || umask > uint64(os.ModePerm) {
			return 0, false
		}

		return os.FileMode(umask), true
	}

	return 0, false
}

func probeUmaskWithFile() os.FileMode {
	name := filepath.Join(os.TempDir(), fmt.Sprintf(".onedrive-go-umask-%d-%d", os.Getpid(), time.Now().UnixNano()))

	f, err := localpath.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, newFilePerms)
	if err != nil {
		return defaultUmask
	}

	info, statErr := f.Stat()
	// The probe is an empty temp file: failing to close or remove it is
	// harmless, and the mode it was created with is the answer either way.
	f.Close()              //nolint:errcheck // see above
	localpath.Remove(name) //nolint:errcheck // see above

	if statErr != nil {
		return defaultUmask
	}

	return newFilePerms &^ info.Mode().Perm()
}

// downloadedFileMode is the mode the finished download gets: the replaced
// target's permission bits (Perm drops setuid, setgid and sticky, which new
// bytes must not inherit), so +x and group access survive every remote edit,
// or the umask default for a new file (D-044, P-8). Downloads used to be
// 0600 always, and a replaced file lost its mode on every remote edit.
func (tm *TransferManager) downloadedFileMode(target TargetFS, targetName string) (os.FileMode, error) {
	info, err := target.Stat(targetName)
	switch {
	case err == nil && info.Mode().IsRegular():
		return info.Mode().Perm(), nil
	case err == nil || errors.Is(err, os.ErrNotExist):
		return tm.newFileMode(), nil
	default:
		return 0, fmt.Errorf("checking the file being replaced: %w", err)
	}
}

// hashTargetFile hashes a file through the target, for a resumed partial.
func hashTargetFile(target TargetFS, name string) (string, error) {
	f, err := target.Open(name)
	if err != nil {
		return "", fmt.Errorf("opening %s for hashing: %w", name, err)
	}
	defer f.Close() //nolint:errcheck // read-only; nothing buffered to flush

	h := quickxorhash.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing %s: %w", name, err)
	}

	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}
