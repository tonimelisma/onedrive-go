package sync

import (
	"math"
	"time"
)

// storedEpochNano is what the epoch stores: one nanosecond after it, because
// its own Unix nanoseconds, 0, are every time column's "no time".
const storedEpochNano = 1

// StoredTimeNano encodes t for a stored time that is compared by order (a
// deadline or a not-before time) and for a remote modification time: Unix
// nanoseconds, saturated at the ends of the int64 range. UnixNano is
// undefined outside 1678 to 2262: a server's Retry-After that saturates at
// the longest Duration asks for a trial near 2318, which would wrap to a time
// in the past and be due at once after a restart, and an item dated 2300 or
// 1601 (the Windows file-time epoch) would be downloaded with a time about
// 584 years off. A time past the range stores math.MaxInt64, which reads back
// as 2262, still the far future; a time before it stores math.MinInt64, which
// reads back as 1677, long past. The zero time stores 0, every such column's
// "no time", and the epoch stores storedEpochNano.
//
// Two remote times both past 2262 store the same value, so a change of only
// the time between them is not seen; a change of content still is.
//
// Local modification, change and birth times do not use it: they are compared
// only with themselves and never turned back into a time, UnixNano wraps a
// given time to the same value every time, and saturating would make two
// different far-future local times equal.
func StoredTimeNano(t time.Time) int64 {
	switch {
	case t.IsZero():
		return 0
	case t.After(time.Unix(0, math.MaxInt64)):
		return math.MaxInt64
	case t.Before(time.Unix(0, math.MinInt64)):
		return math.MinInt64
	}
	if nano := t.UnixNano(); nano != 0 {
		return nano
	}

	return storedEpochNano
}

// StoredTimeIsSet reports whether a value StoredTimeNano wrote is a time.
// Zero, and only zero, is "no time": a time before 1970 is negative and a
// time like any other.
func StoredTimeIsSet(stored int64) bool {
	return stored != 0
}

// RemoteModTime is the file time a stored remote modification time stands
// for, which a download and a folder create set on what they make. It is the
// zero time, which nothing sets, when no time is stored.
func RemoteModTime(stored int64) time.Time {
	if !StoredTimeIsSet(stored) {
		return time.Time{}
	}

	return time.Unix(0, stored)
}
