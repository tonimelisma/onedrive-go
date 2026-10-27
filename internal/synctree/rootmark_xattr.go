//go:build linux || darwin

package synctree

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func getRootMarkXattr(dir *os.File) (string, error) {
	buf := make([]byte, rootMarkMaxBytes)
	var size int
	err := withFd(dir, func(fd int) error {
		var getErr error
		size, getErr = unix.Fgetxattr(fd, RootMarkAttribute, buf)
		return getErr
	})
	if err != nil {
		if errors.Is(err, errNoRootMark) {
			return "", nil
		}
		return "", fmt.Errorf("reading %s: %w", RootMarkAttribute, err)
	}
	return string(buf[:size]), nil
}

func setRootMarkXattr(dir *os.File, token string) error {
	return withFd(dir, func(fd int) error {
		return unix.Fsetxattr(fd, RootMarkAttribute, []byte(token), 0)
	})
}
