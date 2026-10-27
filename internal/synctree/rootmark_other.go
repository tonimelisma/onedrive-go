//go:build unix && !linux && !darwin

package synctree

import (
	"os"

	"golang.org/x/sys/unix"
)

func getRootMarkXattr(*os.File) (string, error) { return "", nil }

func setRootMarkXattr(*os.File, string) error { return unix.ENOTSUP }
