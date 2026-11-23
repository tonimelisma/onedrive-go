package synctree

import (
	"errors"
)

// Capabilities says how an in-memory volume compares names. The zero value
// is a byte-exact volume, as ext4 is: two spellings of one name are two
// entries.
type Capabilities struct {
	// FoldCase makes names case-insensitive and case-preserving, as the
	// macOS default volume is: a name finds an entry that differs from it in
	// case only, and the entry keeps the spelling it was created with.
	FoldCase bool
	// FoldNormalization makes the composed (NFC) and decomposed (NFD)
	// spellings of a name find one entry, as APFS does.
	FoldNormalization bool
}

// ErrOutsideMemoryVolume reports an operation that would reach a place
// outside an in-memory sync tree (a trash folder, the source of a file moved
// in). An in-memory tree has no real folder around it.
var ErrOutsideMemoryVolume = errors.New("an in-memory sync folder has nothing outside it")
