//go:build unix && !linux && !darwin

package synctree

import "errors"

// errNoExclusiveRename is why a system without a no-replace rename call
// always takes the portable fallback.
var errNoExclusiveRename = errors.New("this system has no no-replace rename")

// renameExclusiveAt reports the flag refused: FreeBSD and the other unix
// systems the package builds for have no rename that fails on an existing
// destination, so the portable fallback moves every entry.
func renameExclusiveAt(int, string, int, string) (bool, error) {
	return true, errNoExclusiveRename
}
