package multisync

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func startupTestDrive(name string) MountIdentity {
	canonicalID := driveid.MustCanonicalID("personal:" + name + "@example.com")

	return MountIdentity{
		MountID:        canonicalID.String(),
		ProjectionKind: MountProjectionStandalone,
		CanonicalID:    canonicalID,
	}
}

func startupTestSharedFolder(drive *MountIdentity, binding string) MountIdentity {
	return MountIdentity{
		MountID:        config.ChildMountID(drive.MountID, binding),
		ParentMountID:  drive.MountID,
		ProjectionKind: MountProjectionChild,
	}
}

// Validates: R-6.6.22
//
// A watch whose every mount failed to start names the one failure, or counts
// the drives and the shared folders below them apart, each once, with the
// singular for one: a drive whose shared folders could not be set up has a
// second result beside its own and is still one drive. A result that names no
// mount, as a drive whose configuration could not be resolved has, is a drive
// of its own for each place in the selection.
func TestWatchStartupError_CountsDrivesAndSharedFoldersApart(t *testing.T) {
	t.Parallel()

	work := startupTestDrive("work")
	home := startupTestDrive("home")
	docs := startupTestSharedFolder(&work, "binding-docs")
	photos := startupTestSharedFolder(&work, "binding-photos")
	const details = " could not start syncing; run 'onedrive-go status' for details"

	for _, tc := range []struct {
		name   string
		failed []MountIdentity
		want   string
	}{
		{name: "one drive", failed: []MountIdentity{home}, want: home.MountID + " could not start syncing: store locked"},
		{name: "a drive and its shared-folder setup", failed: []MountIdentity{work, work}, want: "1 drive" + details},
		{name: "drives", failed: []MountIdentity{work, home}, want: "2 drives" + details},
		{name: "a shared folder twice", failed: []MountIdentity{docs, docs}, want: "1 shared folder" + details},
		{name: "shared folders", failed: []MountIdentity{docs, photos}, want: "2 shared folders" + details},
		{name: "a drive and a shared folder", failed: []MountIdentity{home, docs}, want: "1 drive and 1 shared folder" + details},
		{
			name:   "drives and shared folders",
			failed: []MountIdentity{work, home, docs, photos, work},
			want:   "2 drives and 2 shared folders" + details,
		},
		{name: "results that name no mount", failed: []MountIdentity{{}, {}}, want: "2 drives" + details},
		{
			name:   "a result that names no mount beside a drive and its shared-folder setup",
			failed: []MountIdentity{{}, home, home},
			want:   "2 drives" + details,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var summary StartupSelectionSummary
			for i := range tc.failed {
				summary.Results = append(summary.Results, MountStartupResult{
					SelectionIndex: i,
					Identity:       tc.failed[i],
					Status:         MountStartupFatal,
					Err:            errors.New("store locked"),
				})
			}

			assert.Equal(t, tc.want, (&WatchStartupError{Summary: summary}).Error())
		})
	}
}
