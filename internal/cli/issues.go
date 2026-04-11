package cli

import (
	"time"
)

// --- helpers ---

func truncateID(id string) string {
	if len(id) <= conflictIDPrefixLen {
		return id
	}

	return id[:conflictIDPrefixLen]
}

func formatNanoTimestamp(nanos int64) string {
	if nanos == 0 {
		return ""
	}

	return time.Unix(0, nanos).UTC().Format(time.RFC3339)
}
