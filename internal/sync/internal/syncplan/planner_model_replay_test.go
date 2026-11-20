package syncplan

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
)

// Validates: R-6.10.25
//
// A failing seed of the planner family logs exactly one replay line, and the
// line is a complete command run from this package's own test binary: its
// assignments are the planner's variable and the seed, it carries -race
// exactly when this binary has the race detector, it names the planner's
// top-level test, and its package path is this package from the module root.
// A passing seed logs nothing.
func TestLogSeedReplayOnFailure_ThePlannerFamilyReplaysInThisPackage(t *testing.T) {
	t.Parallel()

	const (
		test = "TestPlannerModel_RandomTriplesConverge"
		seed = 7003
	)
	envName := syncfixture.PlannerSeedVars().Single
	assert.Equal(t, syncfixture.PlannerModelSeedEnv, envName, "the variable the family passes the writer")

	passing := &syncfixture.FakeSeedReplayTB{TestName: test + "/seed=7003"}
	syncfixture.LogSeedReplayOnFailure(passing, envName, seed)
	passing.End()
	assert.Empty(t, passing.Logs(), "a passing seed logs no replay line")

	failing := &syncfixture.FakeSeedReplayTB{TestName: test + "/seed=7003", HasFailed: true}
	syncfixture.LogSeedReplayOnFailure(failing, envName, seed)
	failing.End()
	require.Len(t, failing.Logs(), 1)
	command, found := strings.CutPrefix(failing.Logs()[0], syncfixture.SeedReplayPrefix)
	require.True(t, found, "the line starts with %q", syncfixture.SeedReplayPrefix)

	replay := syncfixture.ParseSeedReplayCommand(t, command)
	want := []string{envName + "=7003"}
	if order := os.Getenv(syncfixture.DispatchOrderEnv); order != "" {
		want = append(want, syncfixture.DispatchOrderEnv+"="+order)
	}
	assert.Equal(t, want, replay.Assignments)
	assert.Equal(t, syncfixture.RaceDetectorEnabled, replay.Race)
	assert.Equal(t, "^"+test+"$", replay.RunPattern)
	assert.Contains(t, syncfixture.PackageTestSources(t), "func "+test+"(t *testing.T)", "the replay names a test of this package")
	assert.Equal(t, "./internal/sync/internal/syncplan", replay.Pkg)
	syncfixture.AssertIsThisPackage(t, replay.Pkg)
}
