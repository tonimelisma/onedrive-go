package devtool

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nightlyTestUbuntuLeg is the Linux leg of the verify-full job in the fixtures.
const nightlyTestUbuntuLeg = "verify-full (ubuntu-latest)"

// nightlyTestPlannerFailure is the raw go test output of one failed test,
// internal/sync.TestPlanner.
const nightlyTestPlannerFailure = "--- FAIL: TestPlanner (0.10s)\nFAIL\t" + repoModulePath + "/internal/sync\t3.000s\n"

// Validates: R-6.10.23
//
// A job whose log names no failed test, a lint or repository-rule failure
// among them, is keyed by every failed step its marker lines name, each once,
// as step-<job>-<step> with the job's matrix leg in it; the job itself is not
// keyed then.
func TestNightlyFailureKeysKeyAFailedStepByItsMarker(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionFailure)
	remote.jobs = nightlyRunView(t, map[string]string{nightlyTestUbuntuLeg: checkConclusionFailure, "verify": checkConclusionFailure})
	remote.jobLogs["10"] = "2026-11-09T10:20:00Z ==> golangci-lint\n" +
		"2026-11-09T10:21:00Z internal/sync/a.go:1:1: an issue the linter found\n" +
		"2026-11-09T10:21:01Z " + verifyStepFailedPrefix + "public lint\n" +
		"2026-11-09T10:21:30Z ==> spec governs\n" +
		"2026-11-09T10:21:31Z " + verifyStepFailedPrefix + "public spec-governs\n" +
		verifyStepFailedPrefix + "public spec-governs\n"

	assert.Equal(t, map[string][]string{
		"step-verify-full--ubuntu-latest-lint":         {"verify-full"},
		"step-verify-full--ubuntu-latest-spec-governs": {"verify-full"},
	}, nightlyTestFailures(t, remote))
}

// Validates: R-6.10.23
//
// A failed go test step prints its marker too, but its failed tests name the
// failure better: a log that names failed tests and only the marker of a go
// test step is keyed by the tests alone.
func TestNightlyFailureKeysPreferATestKeyToAStepMarker(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionFailure)
	remote.jobs = nightlyRunView(t, map[string]string{nightlyTestUbuntuLeg: checkConclusionFailure, "verify": checkConclusionFailure})
	remote.jobLogs["10"] = nightlyTestPlannerFailure + verifyStepFailedPrefix + "public unit-tests\n"

	assert.Equal(t, map[string][]string{"internal/sync.TestPlanner": {"verify-full"}}, nightlyTestFailures(t, remote))
}

// Validates: R-6.10.23
//
// A step that runs no go test fails in a way no failed test can name, so its
// marker is keyed whenever the log holds it, beside the failed tests of the
// same log, wherever the two land in it; the marker of the go test step
// whose tests the log names is not.
func TestNightlyFailureKeysKeyAStepOutsideGoTestBesideAFailedTest(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionFailure)
	remote.jobs = nightlyRunView(t, map[string]string{nightlyTestUbuntuLeg: checkConclusionFailure, "verify": checkConclusionFailure})
	remote.jobLogs["10"] = nightlyTestPlannerFailure +
		verifyStepFailedPrefix + "public lint\n" +
		verifyStepFailedPrefix + "public unit-tests\n"

	assert.Equal(t, map[string][]string{
		"internal/sync.TestPlanner":            {"verify-full"},
		"step-verify-full--ubuntu-latest-lint": {"verify-full"},
	}, nightlyTestFailures(t, remote))
}

// Validates: R-6.10.23
//
// A package that failed without naming a test is a failure no test names
// even beside a failed test: the job's failed steps key it in the job's
// place, beside the test.
func TestNightlyFailureKeysKeyAPackageFailureWithoutATestByItsStep(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionFailure)
	remote.jobs = nightlyRunView(t, map[string]string{"stress": checkConclusionFailure})
	remote.jobLogs["13"] = "--- FAIL: TestWatchSoak (1.00s)\nFAIL\t" + repoModulePath + "/internal/multisync\t2.000s\n" +
		verifyStepFailedPrefix + "stress multisync-race-repeated\n" +
		"FAIL\t" + repoModulePath + "/internal/sync [build failed]\n" +
		verifyStepFailedPrefix + "stress sync-tree-race-repeated\n"

	assert.Equal(t, map[string][]string{
		"internal/multisync.TestWatchSoak":    {"stress"},
		"step-stress-multisync-race-repeated": {"stress"},
		"step-stress-sync-tree-race-repeated": {"stress"},
	}, nightlyTestFailures(t, remote))
}

// Validates: R-6.10.23, R-6.10.24
//
// Only a marker line as the step runner prints it counts: the prefix at the
// start of the line, a verify profile and a step of the closed set. Anything
// else in a log, a test's output quoting a marker or a line naming some other
// word, is text, so a key and the public issue never carry it; a job whose
// log holds no marker of the set is keyed by the job.
func TestNightlyFailureKeysReadOnlyMarkersOfTheClosedSet(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionFailure)
	remote.jobs = nightlyRunView(t, map[string]string{"stress": checkConclusionFailure})
	remote.jobLogs["13"] = verifyStepFailedPrefix + "stress not-a-step\n" +
		verifyStepFailedPrefix + "nightly lint\n" +
		verifyStepFailedPrefix + "public lint extra\n" +
		"    " + verifyStepFailedPrefix + "public lint\n" +
		`{"Action":"output","Package":"` + repoModulePath + `/internal/sync","Output":"` + verifyStepFailedPrefix + `public lint\n"}` + "\n"

	assert.Equal(t, map[string][]string{"job-stress": {"stress"}}, nightlyTestFailures(t, remote))
}

// Validates: R-6.10.23
//
// A step key is its own claim ref name, a valid ref component, for every step
// of the closed set in every job the night requires, and no two pairs of a
// job and a step share one, so two failures never merge into one claim. The
// check claims a failed step under that key and reads the key back from the
// claim record unchanged.
func TestNightlyCheckClaimsAFailedStepUnderItsOwnKey(t *testing.T) {
	t.Parallel()

	jobs := append([]string{nightlyTestUbuntuLeg, "verify-full (macos-latest)"}, nightlyRequiredJobs()...)
	pairOfKey := map[string]string{}
	for _, job := range jobs {
		for _, step := range verifyStepNames() {
			key := nightlyStepKey(job, step)
			assert.NotContains(t, pairOfKey, key, "job %q step %q shares its key with %s", job, step, pairOfKey[key])
			pairOfKey[key] = job + " " + string(step)
			assert.Equal(t, key, nightlyClaimRefName(key), "job %q step %q", job, step)
			assert.Empty(t, refComponentViolation(key), "job %q step %q", job, step)
		}
	}

	remote := newClaimRemote(t, checkConclusionFailure)
	remote.jobs = nightlyRunView(t, map[string]string{"stress": checkConclusionFailure})
	remote.jobLogs["13"] = "2026-11-09T12:00:00Z " + verifyStepFailedPrefix + "stress sync-tree-race-repeated\n"

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	const key = "step-stress-sync-tree-race-repeated"
	assert.Equal(t, []string{key}, remote.claimKeys())
	assert.Contains(t, remote.refs, key, "the key is its claim's ref name")
	claim, held := remote.claim(key)
	require.True(t, held)
	assert.Equal(t, key, claim.Key, "the claim record reads the key back unchanged")
	assert.Equal(t, nightlyTestOwner, claim.Owner)
	assert.Contains(t, out, key)
}
