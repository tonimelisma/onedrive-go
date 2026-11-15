//go:build unix

package localpath

import (
	"os"
	"syscall"
)

// openDirFlags open a directory read-only, refuse anything else with
// ENOTDIR, and never block: a plain open of a FIFO waits for a writer for
// ever. O_NONBLOCK has no effect on a directory.
const openDirFlags = os.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NONBLOCK
