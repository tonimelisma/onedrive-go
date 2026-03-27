//go:build darwin

package tokenfile

import "os"

func chmodManagedTempPath(path string, mode os.FileMode) error {
	//nolint:gosec // Path is an os.CreateTemp-managed temp file in the token directory.
	return os.Chmod(path, mode) //nolint:wrapcheck // Caller adds tokenfile-specific context around this platform shim.
}

func renameManagedTempPath(src, dst string) error {
	//nolint:gosec // Source and destination are managed atomic-write paths in the token directory.
	return os.Rename(src, dst) //nolint:wrapcheck // Caller adds tokenfile-specific context around this platform shim.
}
