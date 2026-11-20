package sync

import (
	"time"
)

// MinPollInterval is the shortest remote poll interval; a shorter configured
// interval is raised to it.
const MinPollInterval = 30 * time.Second
