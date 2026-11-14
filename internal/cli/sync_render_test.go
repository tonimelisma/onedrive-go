package cli

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func renderTestDrive(name string) multisync.MountIdentity {
	return testStandaloneMountIdentity(driveid.MustCanonicalID("personal:" + name + "@example.com"))
}

func renderTestSharedFolder(drive *multisync.MountIdentity, binding string) multisync.MountIdentity {
	return multisync.MountIdentity{
		MountID:        config.ChildMountID(drive.MountID, binding),
		ParentMountID:  drive.MountID,
		ProjectionKind: multisync.MountProjectionChild,
	}
}

// runOnceOver is the result of a pass over the mounts: each has a runnable
// startup result and a report, whose error is failed's entry for its mount.
func runOnceOver(mounts []multisync.MountIdentity, failed map[string]error) multisync.RunOnceResult {
	var result multisync.RunOnceResult
	for i := range mounts {
		result.Startup.Results = append(result.Startup.Results, multisync.MountStartupResult{
			SelectionIndex: i,
			Identity:       mounts[i],
			Status:         multisync.MountStartupRunnable,
		})
		report := &multisync.MountReport{SelectionIndex: i, Identity: mounts[i], Report: &syncengine.Report{}}
		if err := failed[mounts[i].MountID]; err != nil {
			report.Report = nil
			report.Err = err
		}
		result.Reports = append(result.Reports, report)
	}

	return result
}

// Validates: R-6.6.28
//
// The closing error of a run counts what the user selected: configured
// drives. A shared folder below a drive is counted apart and never called a
// drive, so one drive whose shared folder cannot be read is not "1 of 3
// selected drives". The exit status stays 1 for left work.
func TestRunOnceResultError_CountsSharedFoldersApartFromDrives(t *testing.T) {
	t.Parallel()

	work := renderTestDrive("work")
	home := renderTestDrive("home")
	docs := renderTestSharedFolder(&work, "binding-docs")
	photos := renderTestSharedFolder(&work, "binding-photos")
	leftOne := syncengine.LeftWork{StillOpen: 1}

	cases := []struct {
		name   string
		mounts []multisync.MountIdentity
		left   map[string]syncengine.LeftWork
		want   string
	}{
		{
			name:   "only a shared folder of the one drive left files",
			mounts: []multisync.MountIdentity{work, docs, photos},
			left:   map[string]syncengine.LeftWork{photos.MountID: leftOne},
			want:   "1 of 2 shared folders in 1 selected drive left files unsynced (see above)",
		},
		{
			name:   "the one drive itself left files",
			mounts: []multisync.MountIdentity{work, docs, photos},
			left:   map[string]syncengine.LeftWork{work.MountID: leftOne},
			want:   "1 of 1 selected drive left files unsynced (see above)",
		},
		{
			name:   "a drive and a shared folder left files",
			mounts: []multisync.MountIdentity{work, docs, home},
			left:   map[string]syncengine.LeftWork{home.MountID: leftOne, docs.MountID: leftOne},
			want:   "1 of 2 selected drives and 1 of 1 shared folder left files unsynced (see above)",
		},
		{
			name:   "drives without shared folders",
			mounts: []multisync.MountIdentity{work, home},
			left:   map[string]syncengine.LeftWork{home.MountID: leftOne},
			want:   "1 of 2 selected drives left files unsynced (see above)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := runOnceResultError(runOnceOver(tc.mounts, nil), tc.left)

			requireSyncLeftWork(t, err, tc.want)
		})
	}
}

// Validates: R-6.6.28, R-6.6.22
//
// The closing error of a run with mounts that could not sync counts them the
// same way: drives as selected drives, each once, and shared folders apart.
// A drive whose shared folders could not be set up has a second startup
// result of its own, and is still one of one selected drives. Results that
// name no mount are counted once each, and the verb follows the count.
func TestRunOnceDrivesError_CountsSharedFoldersApartFromDrives(t *testing.T) {
	t.Parallel()

	work := renderTestDrive("work")
	docs := renderTestSharedFolder(&work, "binding-docs")
	photos := renderTestSharedFolder(&work, "binding-photos")
	errUnreadable := errors.New("shared folder read failed")

	t.Run("a shared folder of the one drive failed", func(t *testing.T) {
		t.Parallel()

		result := runOnceOver([]multisync.MountIdentity{work, docs, photos}, map[string]error{docs.MountID: errUnreadable})
		err := runOnceResultError(result, map[string]syncengine.LeftWork{})

		require.ErrorIs(t, err, errUnreadable)
		assert.Equal(t, "1 of 2 shared folders in 1 selected drive failed or was skipped (see above)", err.Error())
	})

	t.Run("both shared folders of the one drive failed", func(t *testing.T) {
		t.Parallel()

		result := runOnceOver([]multisync.MountIdentity{work, docs, photos},
			map[string]error{docs.MountID: errUnreadable, photos.MountID: errUnreadable})
		err := runOnceResultError(result, map[string]syncengine.LeftWork{})

		require.ErrorIs(t, err, errUnreadable)
		assert.Equal(t, "2 of 2 shared folders in 1 selected drive failed or were skipped (see above)", err.Error())
	})

	t.Run("results that name no mount count once each", func(t *testing.T) {
		t.Parallel()

		home := renderTestDrive("home")
		result := runOnceOver([]multisync.MountIdentity{work, home}, nil)
		for i, report := range result.Reports {
			result.Reports[i] = &multisync.MountReport{SelectionIndex: report.SelectionIndex, Err: errUnreadable}
		}
		err := runOnceResultError(result, map[string]syncengine.LeftWork{})

		require.ErrorIs(t, err, errUnreadable)
		assert.Equal(t, "2 of 2 selected drives failed or were skipped (see above)", err.Error())
	})

	t.Run("the shared folders of the one drive could not be set up", func(t *testing.T) {
		t.Parallel()

		errSetup := errors.New("set up shared-folder shortcuts: incomplete")
		result := runOnceOver([]multisync.MountIdentity{work}, nil)
		result.Startup.Results = append(result.Startup.Results, multisync.MountStartupResult{
			Identity: work,
			Status:   multisync.MountStartupFatal,
			Err:      errSetup,
		})
		err := runOnceResultError(result, map[string]syncengine.LeftWork{})

		require.ErrorIs(t, err, errSetup)
		assert.Equal(t, "1 of 1 selected drive failed or was skipped (see above)", err.Error())
	})
}
