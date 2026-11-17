package devtool

import (
	"bytes"
	"context"
	"fmt"
	"go/build"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fullE2ETagConstraint is the build line of a full-tier test file.
const fullE2ETagConstraint = "e2e && e2e_full"

// fastE2ELiveTestNames are the live tests of the per-PR lane: the TestE2E_
// tests of the fast-tagged files, each one a smoke test that runs on every
// pull request. Adding a live test to that lane is an edit of this list.
func fastE2ELiveTestNames() []string {
	return []string{
		authPreflightTestName,
		"TestE2E_FileOpsSmokeCRUD",
		"TestE2E_FixturePreflight_Fast",
		"TestE2E_ShortcutSmoke_DownloadOnlyProjectsChildMount",
		"TestE2E_SyncWatch_WebsocketDisabledLongPollRegression",
		fastDownloadTestName,
		"TestE2E_Sync_UploadOnly",
		"TestE2E_TrackedFilesNameNoTestAccount",
	}
}

// fastE2ELiveTestsMatchTheAllowList compares the TestE2E_ tests of the fast
// tier with fastE2ELiveTestNames and names, with its file, each one the list
// lacks, and each name of the list no fast-tagged file declares. The fast
// tier's other tests, the e2e helpers' unit tests and TestMain, are outside
// the rule.
func fastE2ELiveTestsMatchTheAllowList(tests []e2eTest) error {
	allowed := fastE2ELiveTestNames()
	problems := []string{}
	found := map[string]bool{}
	for _, test := range tests {
		if !strings.HasPrefix(test.name, e2eTestNamePrefix) {
			continue
		}
		found[test.name] = true
		if !slices.Contains(allowed, test.name) {
			problems = append(problems, test.String()+" is not on the fast lane's allow-list")
		}
	}
	for _, name := range allowed {
		if !found[name] {
			problems = append(problems, name+" is on the fast lane's allow-list, but no fast-tagged file declares it")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("fast e2e tier: %s", strings.Join(problems, "; "))
	}

	return nil
}

// writeE2ETestFile writes repoRoot/e2e/name, which may name a directory
// below e2e, a file of package e2e under the build line constraint (none
// when it is empty) that declares an empty top-level function of each name.
func writeE2ETestFile(t *testing.T, repoRoot string, name string, constraint string, tests ...string) {
	t.Helper()

	var source strings.Builder
	if constraint != "" {
		source.WriteString("//go:build " + constraint + "\n\n")
	}
	source.WriteString("package e2e\n\nimport \"testing\"\n")
	for _, test := range tests {
		source.WriteString("\nfunc " + test + "(t *testing.T) { t.Helper() }\n")
	}

	path := filepath.Join(repoRoot, e2eDirName, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(source.String()), 0o600))
}

// fakeFullE2ETestFile is the one full-tier file of the e2e tree
// newFakeE2ERepo writes.
const fakeFullE2ETestFile = "fake_full_e2e_test.go"

// fakeFullE2ETests are the tests of fakeFullE2ETestFile: the full fixture
// preflight and at least one test of the suite for each bucket, so a fake go
// test output that fails one reads as the suite's would.
func fakeFullE2ETests() []string {
	return []string{
		fullE2EFixturePreflight,
		"TestE2E_Edge_InvalidFilenameRejection",
		"TestE2E_Edge_RapidFileChurn",
		"TestE2E_Sync_DeletePropagation",
		"TestE2E_Orchestrator_WatchSimultaneous",
		"TestE2E_SyncWatch_PauseResume",
		websocketRestartTestName,
	}
}

// newFakeE2ERepo returns a temporary repository root whose e2e directory
// holds fakeFullE2ETests in one full-tier file, for a fake-runner run of the
// e2e-full profile.
func newFakeE2ERepo(t *testing.T) string {
	t.Helper()

	repoRoot := t.TempDir()
	writeFakeE2ETree(t, repoRoot)

	return repoRoot
}

// writeFakeE2ETree writes the e2e directory of newFakeE2ERepo into repoRoot.
func writeFakeE2ETree(t *testing.T, repoRoot string) {
	t.Helper()

	writeE2ETestFile(t, repoRoot, fakeFullE2ETestFile, fullE2ETagConstraint, fakeFullE2ETests()...)
}

// fakeFullE2EBuckets are the buckets the e2e-full profile runs over the tree
// writeFakeE2ETree writes, placed by the function discovery places with.
func fakeFullE2EBuckets(t *testing.T) []fullE2EBucketSpec {
	t.Helper()

	tests := make([]e2eTest, 0, len(fakeFullE2ETests()))
	for _, name := range fakeFullE2ETests() {
		tests = append(tests, e2eTest{name: name, file: filepath.Join(e2eDirName, fakeFullE2ETestFile)})
	}
	buckets, err := placeFullE2ETests(tests)
	require.NoError(t, err)

	return buckets
}

// fakeFullE2EBucket is the bucket of fakeFullE2EBuckets with this name.
func fakeFullE2EBucket(t *testing.T, name verifyStepName) fullE2EBucketSpec {
	t.Helper()

	for _, bucket := range fakeFullE2EBuckets(t) {
		if bucket.Name == name {
			return bucket
		}
	}
	require.Failf(t, "no such bucket", "bucket %s", name)

	return fullE2EBucketSpec{}
}

// otherGOOS is an operating system go/build knows that this host is not.
func otherGOOS() string {
	for _, goos := range []string{"plan9", "aix"} {
		if goos != build.Default.GOOS {
			return goos
		}
	}

	return ""
}

// recordedArgs are the arguments of each recorded command, without the
// environment, which a failure message would otherwise print.
func recordedArgs(commands []recordedCommand) [][]string {
	args := make([][]string, 0, len(commands))
	for _, command := range commands {
		args = append(args, command.args)
	}

	return args
}

func e2eTestNames(tests []e2eTest) []string {
	names := make([]string, 0, len(tests))
	for _, test := range tests {
		names = append(names, test.name)
	}

	return names
}

// Validates: R-6.10.28
func TestE2ETestFamilyIsTheNameUpToTheNextUnderscore(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		family  string
		refusal string
	}{
		{name: "TestE2E_Sync_X", family: "Sync"},
		{name: "TestE2E_Status_LiveOverlay_ConfigTolerance", family: "Status"},
		{name: "TestE2E_RoundTrip", refusal: "has no family"},
		{name: "TestE2E__Spins", refusal: "has no family"},
		// A top-level test of a full-tier file that is not a live test.
		{name: "TestWidgetHelper", refusal: "is not named " + e2eTestNamePrefix},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			family, err := e2eTestFamily(tc.name)
			if tc.refusal != "" {
				require.ErrorContains(t, err, tc.refusal)
				assert.Empty(t, family)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.family, family)
		})
	}
}

// Validates: R-6.10.26, R-6.10.28
//
// Every test of the full tier runs once a night: in the one bucket its family
// names, or as the full fixture preflight ahead of the buckets.
func TestFullE2EExecutionManifestCoversTaggedTestsExactlyOnce(t *testing.T) {
	t.Parallel()

	repoRoot := repoRootFromDevtoolPackage(t)
	tests, err := discoverE2ETests(repoRoot, e2eTierFull)
	require.NoError(t, err)
	require.NotEmpty(t, tests)
	buckets, err := discoverFullE2EBuckets(repoRoot)
	require.NoError(t, err)

	assigned := map[string]int{fullE2EFixturePreflight: 1}
	for _, bucket := range buckets {
		for _, name := range bucket.TestNames {
			assigned[name]++
		}
	}
	for _, test := range tests {
		assert.Equalf(t, 1, assigned[test.name], "full-tier test %s runs exactly once", test)
	}
	names := e2eTestNames(tests)
	for name, count := range assigned {
		assert.Containsf(t, names, name, "the run names %s, which no full-tier file declares", name)
		assert.Equalf(t, 1, count, "the run names %s once", name)
	}
}

// Validates: R-6.10.28
//
// A test the table cannot place is named with its file, every one of them
// in one error, and no bucket is built.
func TestFullE2EBucketsRefuseATestNoFamilyPlaces(t *testing.T) {
	t.Parallel()

	buckets, err := placeFullE2ETests([]e2eTest{
		{name: "TestE2E_Status_Fine", file: "e2e/fine_test.go"},
		{name: "TestE2E_Widget_Spins", file: "e2e/widget_test.go"},
		{name: "TestE2E_Spins", file: "e2e/spin_test.go"},
		{name: "TestWidgetHelper", file: "e2e/widget_test.go"},
	})
	require.Error(t, err)
	assert.Nil(t, buckets)
	for _, want := range []string{
		"TestE2E_Widget_Spins (e2e/widget_test.go) has family Widget, which no bucket holds",
		"TestE2E_Spins (e2e/spin_test.go) has no family",
		"TestWidgetHelper (e2e/widget_test.go) is not named " + e2eTestNamePrefix,
	} {
		require.ErrorContains(t, err, want)
	}
	assert.NotContains(t, err.Error(), "TestE2E_Status_Fine")
}

// Validates: R-6.10.28
//
// The table holds no family the suite does not use, and every bucket has
// tests to run.
func TestFullE2EFamilyTableNamesOnlyFamiliesTheSuiteUses(t *testing.T) {
	t.Parallel()

	repoRoot := repoRootFromDevtoolPackage(t)
	tests, err := discoverE2ETests(repoRoot, e2eTierFull)
	require.NoError(t, err)
	used := map[string]bool{}
	for _, test := range tests {
		if family, familyErr := e2eTestFamily(test.name); familyErr == nil {
			used[family] = true
		}
	}
	for family := range fullE2EFamilyBuckets() {
		assert.Truef(t, used[family], "family %s of the table names no full-tier test", family)
	}

	buckets, err := discoverFullE2EBuckets(repoRoot)
	require.NoError(t, err)
	require.Len(t, buckets, len(fullE2EBucketShapes()))
	for _, bucket := range buckets {
		assert.NotEmptyf(t, bucket.TestNames, "bucket %s has no test", bucket.Name)
	}
}

// Validates: R-6.10.28
//
// Each bucket of the table is a bucket of the run, and every bucket of the
// run holds a family of the table.
func TestFullE2EFamilyTableNamesEveryBucketAndNoOther(t *testing.T) {
	t.Parallel()

	named := map[verifyStepName]bool{}
	for _, bucket := range fullE2EFamilyBuckets() {
		named[bucket] = true
	}
	shapes := []verifyStepName{}
	for _, shape := range fullE2EBucketShapes() {
		shapes = append(shapes, shape.Name)
		assert.Emptyf(t, shape.TestNames, "bucket %s gets its tests from discovery", shape.Name)
	}
	assert.ElementsMatch(t, shapes, slices.Collect(maps.Keys(named)))
	assert.NotContains(t, fullE2EFamilyBuckets(), "FixturePreflight", "the full preflight belongs to no bucket")
}

// Validates: R-6.10.28
//
// Discovery asks go/build which files each tag set builds on this host, so a
// build line written in another order, in parentheses or with this host's
// GOOS selects its file as the build does.
func TestDiscoverE2ETestsAsksTheBuildForEachTagSet(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeE2ETestFile(t, repoRoot, "reversed_test.go", "e2e_full && e2e", "TestE2E_Sync_Reversed")
	writeE2ETestFile(t, repoRoot, "grouped_test.go", "(e2e && e2e_full)", "TestE2E_Sync_Grouped")
	writeE2ETestFile(t, repoRoot, "host_test.go", fullE2ETagConstraint+" && "+build.Default.GOOS, "TestE2E_Sync_OnThisHost")
	writeE2ETestFile(t, repoRoot, "fast_test.go", e2eBuildTag, "TestE2E_Fast_Smoke")
	writeE2ETestFile(t, repoRoot, "helper_test.go", "", "TestHelperRule")
	writeE2ETestFile(t, repoRoot, "helper.go", fullE2ETagConstraint, "TestNotInATestFile")

	full, err := discoverE2ETests(repoRoot, e2eTierFull)
	require.NoError(t, err)
	assert.Equal(t, []e2eTest{
		{name: "TestE2E_Sync_Grouped", file: filepath.Join(e2eDirName, "grouped_test.go")},
		{name: "TestE2E_Sync_OnThisHost", file: filepath.Join(e2eDirName, "host_test.go")},
		{name: "TestE2E_Sync_Reversed", file: filepath.Join(e2eDirName, "reversed_test.go")},
	}, full)

	fast, err := discoverE2ETests(repoRoot, e2eTierFast)
	require.NoError(t, err)
	assert.Equal(t, []string{"TestE2E_Fast_Smoke"}, e2eTestNames(fast))

	untagged, err := discoverE2ETests(repoRoot, e2eTierUntagged)
	require.NoError(t, err)
	assert.Equal(t, []string{"TestHelperRule"}, e2eTestNames(untagged))
}

// Validates: R-6.10.28
//
// The buckets run ./e2e/..., so discovery reads every package that pattern
// names, and skips the directories go ignores.
func TestDiscoverE2ETestsReadsEveryPackageTheBucketsRun(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeE2ETestFile(t, repoRoot, "top_test.go", fullE2ETagConstraint, "TestE2E_Sync_Top")
	writeE2ETestFile(t, repoRoot, filepath.Join("nested", "deeper", "below_test.go"), fullE2ETagConstraint, "TestE2E_Sync_Below")
	for _, ignored := range []string{goTestdataDirName, ".hidden", "_parked"} {
		writeE2ETestFile(t, repoRoot, filepath.Join(ignored, "ignored_test.go"), fullE2ETagConstraint, "TestE2E_Sync_Ignored")
	}

	tests, err := discoverE2ETests(repoRoot, e2eTierFull)
	require.NoError(t, err)
	assert.Equal(t, []e2eTest{
		{name: "TestE2E_Sync_Below", file: filepath.Join(e2eDirName, "nested", "deeper", "below_test.go")},
		{name: "TestE2E_Sync_Top", file: filepath.Join(e2eDirName, "top_test.go")},
	}, tests)
}

// Validates: R-6.10.28
//
// A test file no tag set builds on this host would hold tests no tier runs
// here, so discovery fails and names it, by a build line or a file name.
func TestDiscoverE2ETestsFailsOnATestFileNoTagSetBuildsHere(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		file       string
		constraint string
	}{
		{name: "build line", file: "elsewhere_test.go", constraint: fullE2ETagConstraint + " && " + otherGOOS()},
		{name: "file name", file: "elsewhere_" + otherGOOS() + "_test.go", constraint: fullE2ETagConstraint},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repoRoot := t.TempDir()
			writeE2ETestFile(t, repoRoot, "fine_test.go", fullE2ETagConstraint, "TestE2E_Sync_Fine")
			writeE2ETestFile(t, repoRoot, tc.file, tc.constraint, "TestE2E_Sync_Elsewhere")

			for _, tier := range []e2eTier{e2eTierUntagged, e2eTierFast, e2eTierFull} {
				tests, err := discoverE2ETests(repoRoot, tier)
				require.ErrorContainsf(t, err, filepath.Join(e2eDirName, tc.file), "tier %s", tier)
				assert.Nil(t, tests)
			}
		})
	}
}

// Validates: R-6.10.28
//
// The buckets compile with -race, which sets the race tag, so discovery
// selects full-tier files as that build does: a file only the race build
// takes is found, and a file the race build leaves out would hold tests no
// bucket runs, so it fails discovery by name.
func TestDiscoverE2ETestsSelectsFullTierFilesAsTheRaceBuildDoes(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeE2ETestFile(t, repoRoot, "race_test.go", fullE2ETagConstraint+" && race", "TestE2E_Sync_UnderRace")

	found, err := discoverE2ETests(repoRoot, e2eTierFull)
	require.NoError(t, err)
	assert.Equal(t, []e2eTest{
		{name: "TestE2E_Sync_UnderRace", file: filepath.Join(e2eDirName, "race_test.go")},
	}, found)

	writeE2ETestFile(t, repoRoot, "norace_test.go", fullE2ETagConstraint+" && !race", "TestE2E_Sync_WithoutRace")

	tests, err := discoverE2ETests(repoRoot, e2eTierFull)
	require.ErrorContains(t, err, filepath.Join(e2eDirName, "norace_test.go"))
	assert.Nil(t, tests)
}

// Validates: R-6.10.28
//
// go test runs a full-tier file's fuzz targets and examples as well as its
// tests, and no bucket's -run list would name one, so discovery finds them
// and placement refuses each by name. A name go test does not run is not
// found.
func TestDiscoverFullE2EBucketsRefusesAFuzzTargetOrAnExample(t *testing.T) {
	t.Parallel()

	repoRoot := newFakeE2ERepo(t)
	writeE2ETestFile(t, repoRoot, "other_kinds_test.go", fullE2ETagConstraint,
		"FuzzE2E_Sync_Names", "Example_sync", "Fuzzy", "Exampled")

	buckets, err := discoverFullE2EBuckets(repoRoot)
	require.Error(t, err)
	assert.Nil(t, buckets)
	file := filepath.Join(e2eDirName, "other_kinds_test.go")
	for _, name := range []string{"FuzzE2E_Sync_Names", "Example_sync"} {
		require.ErrorContains(t, err, name+" ("+file+") is not named "+e2eTestNamePrefix)
	}
	assert.NotContains(t, err.Error(), "Fuzzy")
	assert.NotContains(t, err.Error(), "Exampled")
}

// Validates: R-6.10.28
func TestFastE2ELiveTestsAreTheAllowList(t *testing.T) {
	t.Parallel()

	tests, err := discoverE2ETests(repoRootFromDevtoolPackage(t), e2eTierFast)
	require.NoError(t, err)
	require.NoError(t, fastE2ELiveTestsMatchTheAllowList(tests))
}

// Validates: R-6.10.28
//
// A fast-tagged live test the allow-list lacks, and a listed name no
// fast-tagged file declares, are named; the e2e helpers' unit tests are not.
func TestFastE2ELiveTestsOutsideTheAllowListAreNamed(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	listed := fastE2ELiveTestNames()
	missing := listed[len(listed)-1]
	tests := append(slices.Clone(listed[:len(listed)-1]), "TestE2E_Status_Extra", "TestHelperOutsideTheRule", "TestMain")
	writeE2ETestFile(t, repoRoot, "smoke_test.go", e2eBuildTag, tests...)

	discovered, err := discoverE2ETests(repoRoot, e2eTierFast)
	require.NoError(t, err)
	err = fastE2ELiveTestsMatchTheAllowList(discovered)
	require.ErrorContains(t, err, "TestE2E_Status_Extra ("+filepath.Join(e2eDirName, "smoke_test.go")+") is not on")
	require.ErrorContains(t, err, missing+" is on the fast lane's allow-list")
	assert.NotContains(t, err.Error(), "TestHelperOutsideTheRule")
	assert.NotContains(t, err.Error(), "TestMain")
}

// runE2EFullOverTree runs the e2e-full profile against the fake runner with
// repoRoot as the repository and returns the summary, what it printed on
// stderr and its error.
func runE2EFullOverTree(t *testing.T, runner *fakeRunner, repoRoot string) (VerifySummary, string, error) {
	t.Helper()

	summaryPath := filepath.Join(t.TempDir(), "verify-summary.json")
	stderr := &bytes.Buffer{}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:        repoRoot,
		Profile:         VerifyE2EFull,
		E2ELogDir:       filepath.Join(t.TempDir(), "e2e-logs"),
		SummaryJSONPath: summaryPath,
		Stdout:          &bytes.Buffer{},
		Stderr:          stderr,
	})

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)

	return summary, stderr.String(), err
}

// Validates: R-6.10.26, R-6.10.28
//
// A full-tier test no bucket takes stops the run before its first live call,
// the auth preflight, and the summary still lists every bucket, as not run.
func TestRunVerifyE2EFullStopsBeforeItsFirstLiveCallOnATestNoBucketTakes(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeE2ETestFile(t, repoRoot, "full_test.go", fullE2ETagConstraint,
		fullE2EFixturePreflight, "TestE2E_Status_Fine", "TestE2E_Widget_Spins", "TestE2E_Spins")
	runner := &fakeRunner{}

	summary, stderr, err := runE2EFullOverTree(t, runner, repoRoot)
	file := filepath.Join(e2eDirName, "full_test.go")
	require.ErrorContains(t, err, "TestE2E_Widget_Spins ("+file+")")
	require.ErrorContains(t, err, "TestE2E_Spins ("+file+")")
	assert.Empty(t, recordedArgs(runner.runCommands), "no go test started")
	assert.Empty(t, recordedArgs(runner.combinedCommands), "no go test started")

	assert.Equal(t, verifySummaryStatusFail, requireVerifySummaryStep(t, summary, fullE2EDiscoveryStepName).Status)
	require.Len(t, summary.E2EFullBuckets, len(fullE2EBucketShapes()))
	for i, shape := range fullE2EBucketShapes() {
		assert.Equal(t, string(shape.Name), summary.E2EFullBuckets[i].Name)
		assert.Equal(t, verifySummaryStatusNotRun, summary.E2EFullBuckets[i].Status)
		assert.Contains(t, summary.E2EFullBuckets[i].Error, "discovery")
	}
	assert.Contains(t, stderr, failedStepMarker(string(VerifyE2EFull), fullE2EDiscoveryStepName))
}

// Validates: R-6.10.28
//
// Each bucket runs exactly the tests discovery placed in it, named in order:
// a new test runs with no list to edit, and a fast-tagged test that the full
// build also compiles is not run again.
func TestRunVerifyE2EFullRunsTheTestsDiscoveryPlaced(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeE2ETestFile(t, repoRoot, "preflight_test.go", fullE2ETagConstraint, fullE2EFixturePreflight)
	writeE2ETestFile(t, repoRoot, "misc_test.go", "e2e_full && e2e", "TestE2E_Status_Widget", "TestE2E_Edge_Widget")
	writeE2ETestFile(t, repoRoot, "sync_test.go", fullE2ETagConstraint+" && "+build.Default.GOOS,
		"TestE2E_Sync_Widget", "TestE2E_SyncFull_Widget")
	writeE2ETestFile(t, repoRoot, "watch_test.go", "(e2e && e2e_full)", "TestE2E_Shortcut_Widget")
	writeE2ETestFile(t, repoRoot, "smoke_test.go", e2eBuildTag, "TestE2E_Sync_UploadOnly")
	runner := &fakeRunner{}

	summary, _, err := runE2EFullOverTree(t, runner, repoRoot)
	require.NoError(t, err)
	require.Len(t, runner.runCommands, e2eFullCommandsBeforeBuckets+len(fullE2EBucketShapes()))
	patterns := []string{}
	for _, command := range runner.runCommands[e2eFullCommandsBeforeBuckets:] {
		for _, arg := range command.args {
			if strings.HasPrefix(arg, "-run=") {
				patterns = append(patterns, arg)
			}
		}
	}
	assert.Equal(t, []string{
		"-run=^(TestE2E_Edge_Widget|TestE2E_Status_Widget)$",
		"-run=^(TestE2E_SyncFull_Widget|TestE2E_Sync_Widget)$",
		"-run=^(TestE2E_Shortcut_Widget)$",
	}, patterns)
	assert.Equal(t, verifySummaryStatusPass, requireVerifySummaryStep(t, summary, fullE2EDiscoveryStepName).Status)
	assert.Equal(t, "^(TestE2E_Edge_Widget|TestE2E_Status_Widget)$", summary.E2EFullBuckets[0].RunPattern)
}
