//go:build !darwin

package config

import "os"

func chmodManagedTempPath(path string, mode os.FileMode) error {
	return os.Chmod(path, mode) //nolint:wrapcheck // Caller adds config-file-specific context around this platform shim.
}

func renameManagedTempPath(src, dst string) error {
	return os.Rename(src, dst) //nolint:wrapcheck // Caller adds config-file-specific context around this platform shim.
}
