//go:build !linux

package synctree

import (
	"errors"
	"fmt"
)

// The in-memory volume needs memfd, which only Linux has; elsewhere
// OpenMemory fails and no Root carries a volume.

type memoryVolume struct{}

// OpenMemory reports that in-memory volumes are unavailable here.
func OpenMemory(Capabilities) (*Root, error) {
	return nil, fmt.Errorf("opening an in-memory sync folder: %w", errors.ErrUnsupported)
}

func (*memoryVolume) rename(_, _ string, _ bool) error { return errors.ErrUnsupported }

func (*memoryVolume) removeVerified(_, _ string, _ VerifyFunc) error { return errors.ErrUnsupported }
