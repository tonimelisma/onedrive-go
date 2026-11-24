package syncobserve

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// changedValue is what a test writes over an input after construction.
const changedValue = "changed"

// An observer is built whole from its inputs and keeps its own copies: a
// caller changing the slices it passed cannot change what a running scan or
// watch reads.
//
// Validates: R-6.10.29
func TestNewLocalObserver_KeepsItsOwnCopyOfItsInputs(t *testing.T) {
	t.Parallel()

	ignored := []string{"build"}
	roots := []syncshortcut.ProtectedRoot{{Path: "Shared/Team", BindingID: "binding-1"}}
	flags := []string{"Docs/nas"}
	obs := NewLocalObserver(&LocalObserverInputs{
		Baseline:           syncfixture.EmptyBaseline(),
		Logger:             synctest.TestLogger(t),
		ContentFilter:      syncdomain.ContentFilterConfig{IgnoredDirs: ignored},
		ProtectedRoots:     roots,
		MountBoundaryFlags: flags,
	})
	ignored[0] = changedValue
	roots[0].Path = changedValue
	flags[0] = changedValue

	assert.Equal(t, []string{"build"}, obs.filterConfig.IgnoredDirs)
	assert.Equal(t, "Shared/Team", obs.protectedRoots[0].Path)
	assert.Contains(t, *obs.mountBoundaryFlags.Load(), "Docs/nas")
	assert.Equal(t, syncdomain.RealClock{}, obs.clock, "no clock given is the real clock")

	obs.ReplaceMountBoundaryFlags([]string{"Media/usb"})
	assert.Equal(t, map[string]struct{}{"Media/usb": {}}, *obs.mountBoundaryFlags.Load(),
		"the runtime update replaces the set whole")
}

// Validates: R-6.10.29
func TestNewRemoteObserver_WiresTheConverterFromItsInputs(t *testing.T) {
	t.Parallel()

	roots := []syncshortcut.ProtectedRoot{{Path: "Shared/Team", BindingID: "binding-1"}}
	items := &syncfixture.EngineMockClient{}
	obs := NewRemoteObserver(&RemoteObserverInputs{
		Baseline:            syncfixture.EmptyBaseline(),
		DriveID:             driveid.New(syncfixture.EngineTestDriveID),
		Logger:              synctest.TestLogger(t),
		Items:               items,
		RemotePathLookup:    func(string) (string, bool) { return "known.txt", true },
		ShortcutNamespaceID: "namespace-1",
		ProtectedRoots:      roots,
	})
	roots[0].Path = changedValue

	assert.Same(t, items, obs.Converter.Items)
	path, found := obs.Converter.RemotePathByID("gone")
	assert.True(t, found)
	assert.Equal(t, "known.txt", path)
	assert.Equal(t, "namespace-1", obs.shortcutNamespaceID)
	assert.Equal(t, "Shared/Team", obs.protectedRoots[0].Path)
	assert.Contains(t, obs.Converter.ProtectedRootBindings, "binding-1")
	assert.Equal(t, syncdomain.RealClock{}, obs.clock, "no clock given is the real clock")
}
