package synctree

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// removeEmptyDirRooted removes name with rmdir(2) semantics through a rooted
// parent descriptor.
//
// os.Root exposes no AT_REMOVEDIR primitive: its Remove falls back to
// unlinking regular files, so using it here would silently delete a file that
// replaced the directory between the emptiness check and the removal. Doing
// the unlinkat ourselves against a contained parent descriptor keeps both
// properties that matter — the removal cannot escape the sync root, and it
// refuses anything that is not a directory, which is what makes the final
// syscall the race guard RemoveEmptyDirNoFollow documents.
func removeEmptyDirRooted(root rootHandle, name string) error {
	parent := filepath.Dir(name)
	base := filepath.Base(name)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return fmt.Errorf("%w: refusing to remove sync root", ErrUnsafePath)
	}

	dir, err := root.Open(parent)
	if err != nil {
		return fmt.Errorf("opening parent directory %s: %w", parent, err)
	}
	defer dir.Close()

	conn, err := dir.SyscallConn()
	if err != nil {
		return fmt.Errorf("accessing parent directory %s: %w", parent, err)
	}

	// unlinkErr is written inside Control and read after it returns; Control
	// runs fn synchronously, so no additional synchronization is needed.
	var unlinkErr error

	if ctrlErr := conn.Control(func(fd uintptr) {
		unlinkErr = unix.Unlinkat(int(fd), base, unix.AT_REMOVEDIR)
	}); ctrlErr != nil {
		return fmt.Errorf("controlling parent directory %s: %w", parent, ctrlErr)
	}

	if unlinkErr != nil {
		return &os.PathError{Op: "rmdir", Path: name, Err: unlinkErr}
	}

	return nil
}
