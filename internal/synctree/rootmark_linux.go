package synctree

import "golang.org/x/sys/unix"

// errNoRootMark is the errno a missing extended attribute reads as.
const errNoRootMark = unix.ENODATA
