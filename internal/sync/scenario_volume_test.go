package sync

import (
	stdsync "sync"
	"testing"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// The move-and-recreate scenarios (newRecreateScenarioEngine) run on the
// host's volume, which on Linux is byte-exact. Several of them make a name
// pair OneDrive takes for one name, such as a folder renamed here to x2
// while OneDrive makes X2, and on a case-insensitive volume (macOS by
// default) those two names are one entry here too, which changes what the
// engine meets. Run on Linux alone, a defect that shows only there reached
// main and failed the macOS nightly. A test can run a scenario on an
// emulated case-insensitive volume instead (synctest.CaseInsensitive), as it
// runs one under a dispatch order (runUnderDispatchOrder).

//nolint:gochecknoglobals // a registry test engines consult by test name, as registeredDispatchOrders is
var registeredScenarioVolumes stdsync.Map

// runOnCaseInsensitiveVolume runs fn as t with every scenario engine it
// builds on an emulated case-insensitive volume.
func runOnCaseInsensitiveVolume(t *testing.T, fn func(*testing.T)) {
	t.Helper()

	registeredScenarioVolumes.Store(t.Name(), true)
	t.Cleanup(func() { registeredScenarioVolumes.Delete(t.Name()) })
	fn(t)
}

// registeredScenarioVolume is the filesystem emulation registered for t or an
// ancestor, nil for the host's volume.
func registeredScenarioVolume(t *testing.T) func(synctree.Handle) synctree.Handle {
	t.Helper()

	for name := t.Name(); name != ""; name = parentTestName(name) {
		if _, ok := registeredScenarioVolumes.Load(name); ok {
			return synctest.CaseInsensitive
		}
	}

	return nil
}

// Validates: R-2.17.6
//
// The two scenarios that failed on macOS, run on an emulated
// case-insensitive volume in every tier: a folder OneDrive made at a name
// that, here, a folder whose rename waits holds in another case is not made
// into that folder.
func TestCaseInsensitiveVolume_AFolderOneDriveMadeAtAHeldFoldersNameWaits(t *testing.T) {
	t.Parallel()

	for _, scenario := range []func(*testing.T){
		TestRunOnce_AFileEditedHereThatBothSidesMovedStaysOneItem,
		TestRunOnce_AFileWhoseMoveWaitsKeepsItsSyncedRowAndIsNeverCopied,
	} {
		t.Run(testFuncName(scenario), func(t *testing.T) {
			runOnCaseInsensitiveVolume(t, scenario)
		})
	}
}

// caseInsensitiveSampleStride is the share of the scenarios a run outside the
// full tier re-runs on the emulated volume (syncfixture.FullTier), as
// reverseDispatchSampleStride is for the reversed re-run.
const caseInsensitiveSampleStride = 8

// Validates: R-2.17.6, R-2.13.2
func TestCaseInsensitiveVolume_RunOnceScenariosPass(t *testing.T) {
	syncfixture.SkipInShort(t)
	t.Parallel()

	stride := 1
	if !syncfixture.FullTier() {
		stride = caseInsensitiveSampleStride
	}
	for i, scenario := range runOnceScenarios() {
		if i%stride != 0 {
			continue
		}
		t.Run(testFuncName(scenario), func(t *testing.T) {
			runOnCaseInsensitiveVolume(t, scenario)
		})
	}
}
