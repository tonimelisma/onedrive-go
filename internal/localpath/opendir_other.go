//go:build !unix

package localpath

import "os"

// openDirFlags is a plain read-only open: this system has no directory-only
// open, so a path that is not a directory is refused at the first Next.
const openDirFlags = os.O_RDONLY
