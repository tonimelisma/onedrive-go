//go:build !linux && !darwin

package localpath

func renameNoReplace(src, dst string) error {
	return linkOrCopyNoReplace(src, dst, defaultNoReplaceOps())
}
