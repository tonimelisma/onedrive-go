package devtool

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
)

// The live suite's directory under the repository root, the build tags that
// select its tiers, and the start of every live test's name.
const (
	e2eDirName      = "e2e"
	e2eBuildTag     = "e2e"
	e2eFullBuildTag = "e2e_full"
	// goRaceToolTag is the tag go sets for a -race build, as the full tier's
	// buckets are.
	goRaceToolTag = "race"
	// e2eTestNamePrefix starts the name of a live test. In the full tier the
	// family follows it, up to the next underscore, and names the bucket.
	e2eTestNamePrefix = "TestE2E_"
	// goTestdataDirName is the directory name go ignores in a package pattern.
	goTestdataDirName = "testdata"
)

// e2eTier is one tier of the live suite: the test files of e2e/ that its tag
// set builds on this host and no smaller tag set does. The tag sets nest
// (none; e2e; e2e, e2e_full and race), so a file this host builds at all is
// in exactly one tier.
type e2eTier string

const (
	// e2eTierUntagged files build without tags: the unit tests of the e2e
	// helpers.
	e2eTierUntagged e2eTier = "untagged"
	// e2eTierFast files build with e2e and not without tags: the per-PR live
	// lane.
	e2eTierFast e2eTier = "fast"
	// e2eTierFull files build with e2e and e2e_full under -race, as the
	// buckets run, and not with e2e alone: the nightly full suite.
	e2eTierFull e2eTier = "full"
)

// e2eTiersByTagSet lists the tiers smallest tag set first, the order in which
// a file's tier is decided.
func e2eTiersByTagSet() []e2eTier {
	return []e2eTier{e2eTierUntagged, e2eTierFast, e2eTierFull}
}

// buildTags are the tags of the tier's tag set.
func (tier e2eTier) buildTags() []string {
	switch tier {
	case e2eTierUntagged:
		return nil
	case e2eTierFast:
		return []string{e2eBuildTag}
	case e2eTierFull:
		return []string{e2eBuildTag, e2eFullBuildTag}
	}

	return nil
}

// toolTags are the tags the flags of the tier's go test command set: the
// full tier's buckets run with -race.
func (tier e2eTier) toolTags() []string {
	if tier == e2eTierFull {
		return []string{goRaceToolTag}
	}

	return nil
}

// e2eTest is one top-level test of the live suite and the file, relative to
// the repository root, that declares it.
type e2eTest struct {
	name string
	file string
}

// String names the test and its file, as every message about one does.
func (test e2eTest) String() string {
	return test.name + " (" + test.file + ")"
}

// discoverE2ETests lists, in name order, the top-level tests of the tier's
// files in the packages ./e2e/... names under repoRoot: every function go
// test runs (tests, fuzz targets, examples), TestMain excepted. The tier of a
// file is what go/build decides for this host (MatchFile, the selection go
// test makes), never the text of its build line. A _test.go file that no
// tier's tag set builds here fails discovery with its name, whichever tier
// was asked for: no tier would run its tests.
func discoverE2ETests(repoRoot string, tier e2eTier) ([]e2eTest, error) {
	files, err := e2eTestFiles(repoRoot)
	if err != nil {
		return nil, err
	}

	tests := []e2eTest{}
	unbuilt := []string{}
	fset := token.NewFileSet()
	for _, file := range files {
		fileTier, built, tierErr := e2eTestFileTier(filepath.Join(repoRoot, filepath.Dir(file)), filepath.Base(file))
		if tierErr != nil {
			return nil, fmt.Errorf("discover e2e tests: %s: %w", file, tierErr)
		}
		if !built {
			unbuilt = append(unbuilt, file)
			continue
		}
		if fileTier != tier {
			continue
		}

		names, parseErr := topLevelGoTestNames(fset, filepath.Join(repoRoot, file))
		if parseErr != nil {
			return nil, fmt.Errorf("discover e2e tests: %s: %w", file, parseErr)
		}
		for _, name := range names {
			tests = append(tests, e2eTest{name: name, file: file})
		}
	}
	if len(unbuilt) > 0 {
		return nil, fmt.Errorf("discover e2e tests: no tag set builds %s on %s/%s, so no tier runs its tests here",
			strings.Join(unbuilt, ", "), build.Default.GOOS, build.Default.GOARCH)
	}

	slices.SortFunc(tests, func(a, b e2eTest) int {
		if byName := strings.Compare(a.name, b.name); byName != 0 {
			return byName
		}

		return strings.Compare(a.file, b.file)
	})

	return tests, nil
}

// e2eTestFiles lists, relative to repoRoot, the _test.go files of the
// directories the pattern ./e2e/... names: e2e and every directory below it
// except testdata and those whose name starts with "." or "_", which go
// ignores. It follows the go command's pattern rules, not the repository
// rules' skip set (skipRepositoryRuleDir), since it asks what go test runs.
func e2eTestFiles(repoRoot string) ([]string, error) {
	root := filepath.Join(repoRoot, e2eDirName)
	files := []string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (name == goTestdataDirName || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}

			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return fmt.Errorf("relativize %s: %w", path, relErr)
		}
		files = append(files, rel)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover e2e tests: walk %s: %w", root, err)
	}

	return files, nil
}

// e2eTestFileTier reports the tier of the file name in dir: the first tag
// set, smallest first, whose build on this host, with the flags the tier's go
// test command passes, selects it. built is false when none does.
func e2eTestFileTier(dir string, name string) (tier e2eTier, built bool, err error) {
	for _, candidate := range e2eTiersByTagSet() {
		buildContext := build.Default
		buildContext.BuildTags = candidate.buildTags()
		buildContext.ToolTags = slices.Concat(build.Default.ToolTags, candidate.toolTags())
		matched, matchErr := buildContext.MatchFile(dir, name)
		if matchErr != nil {
			return "", false, fmt.Errorf("match build constraints: %w", matchErr)
		}
		if matched {
			return candidate, true, nil
		}
	}

	return "", false, nil
}

// topLevelGoTestNames names the functions of the Go file at path that go test
// runs, in source order: its tests, TestMain excepted, its fuzz targets and
// its examples.
func topLevelGoTestNames(fset *token.FileSet, path string) ([]string, error) {
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	names := []string{}
	for _, decl := range file.Decls {
		fn, isFunc := decl.(*ast.FuncDecl)
		if !isFunc || fn.Recv != nil || !isGoTestRunName(fn.Name.Name) || fn.Name.Name == testMainName {
			continue
		}
		names = append(names, fn.Name.Name)
	}

	return names, nil
}

// isGoTestRunName reports whether name is one go test runs: a test, a fuzz
// target (its seed corpus) or an example. Go names each by its prefix under
// one rule, the rule isGoTestName holds for Test.
func isGoTestRunName(name string) bool {
	if isGoTestName(name) {
		return true
	}
	for _, prefix := range []string{"Fuzz", "Example"} {
		if rest, ok := strings.CutPrefix(name, prefix); ok && isGoTestName("Test"+rest) {
			return true
		}
	}

	return false
}

// The two ways a full-tier test's name can lack a family.
var (
	errE2ETestNotLive  = errors.New("is not named " + e2eTestNamePrefix + "<Family>_<Case>")
	errE2ETestNoFamily = errors.New("has no family between " + e2eTestNamePrefix + " and the next _")
)

// e2eTestFamily returns the family of a full-tier test's name: the text
// between e2eTestNamePrefix and the next underscore.
func e2eTestFamily(name string) (string, error) {
	rest, isLive := strings.CutPrefix(name, e2eTestNamePrefix)
	if !isLive {
		return "", errE2ETestNotLive
	}
	family, _, hasCase := strings.Cut(rest, "_")
	if !hasCase || family == "" {
		return "", errE2ETestNoFamily
	}

	return family, nil
}

// fullE2EFamilyBuckets is the one table that places a full-tier test: the
// family of the test's name names its bucket, by exact match, with no prefix
// rule, exception list or fallback bucket. A family is in full-parallel-misc
// only when every test of it is safe to run beside every test of that bucket
// under its -parallel value. The full fixture preflight
// (fullE2EFixturePreflight) runs on its own ahead of the buckets, so its
// family is in none. A new family is an edit of this table.
func fullE2EFamilyBuckets() map[string]verifyStepName {
	return map[string]verifyStepName{
		"CLI":                          fullE2EParallelMiscBucketName,
		"Cp":                           fullE2EParallelMiscBucketName,
		"DriveList":                    fullE2EParallelMiscBucketName,
		"Edge":                         fullE2EParallelMiscBucketName,
		"FileOps":                      fullE2EParallelMiscBucketName,
		"InternalBaselineVerification": fullE2EParallelMiscBucketName,
		"Logout":                       fullE2EParallelMiscBucketName,
		"Mv":                           fullE2EParallelMiscBucketName,
		"Pause":                        fullE2EParallelMiscBucketName,
		"RecycleBin":                   fullE2EParallelMiscBucketName,
		"Resume":                       fullE2EParallelMiscBucketName,
		"SharedRead":                   fullE2EParallelMiscBucketName,
		"Status":                       fullE2EParallelMiscBucketName,
		"SyncIsolated":                 fullE2EParallelMiscBucketName,
		"Sync":                         fullE2ESerialSyncBucketName,
		"SyncFull":                     fullE2ESerialSyncBucketName,
		"Orchestrator":                 fullE2ESerialWatchSharedBucketName,
		"Shared":                       fullE2ESerialWatchSharedBucketName,
		"SharedFolder":                 fullE2ESerialWatchSharedBucketName,
		"Shortcut":                     fullE2ESerialWatchSharedBucketName,
		"SyncWatch":                    fullE2ESerialWatchSharedBucketName,
	}
}

// fullE2EBucketShapes are the full suite's buckets in the order they run,
// each without its tests, which placeFullE2ETests adds.
func fullE2EBucketShapes() []fullE2EBucketSpec {
	return []fullE2EBucketSpec{
		{
			Name:     fullE2EParallelMiscBucketName,
			Kind:     fullE2EBucketKindParallelMisc,
			Parallel: fullE2EParallelMiscParallel,
			Timeout:  fullE2EParallelMiscTimeout,
		},
		{
			Name:     fullE2ESerialSyncBucketName,
			Kind:     fullE2EBucketKindSerialSync,
			Parallel: fullE2ESerialParallel,
			Timeout:  fullE2ESerialSyncTimeout,
		},
		{
			Name:     fullE2ESerialWatchSharedBucketName,
			Kind:     fullE2EBucketKindSerialWatchShared,
			Parallel: fullE2ESerialParallel,
			Timeout:  fullE2ESerialWatchSharedTimeout,
		},
	}
}

// placeFullE2ETests builds the full suite's buckets from the full tier's
// tests: each test in the bucket its family names, each bucket's tests in
// name order, the full fixture preflight in none. A test it cannot place
// fails the placement, which names every such test with its file and builds
// no bucket.
func placeFullE2ETests(tests []e2eTest) ([]fullE2EBucketSpec, error) {
	table := fullE2EFamilyBuckets()
	byBucket := map[verifyStepName][]string{}
	unplaced := []string{}
	for _, test := range tests {
		if test.name == fullE2EFixturePreflight {
			continue
		}
		family, err := e2eTestFamily(test.name)
		if err != nil {
			unplaced = append(unplaced, test.String()+" "+err.Error())
			continue
		}
		bucket, held := table[family]
		if !held {
			unplaced = append(unplaced, fmt.Sprintf("%s has family %s, which no bucket holds", test, family))
			continue
		}
		byBucket[bucket] = append(byBucket[bucket], test.name)
	}
	if len(unplaced) > 0 {
		return nil, fmt.Errorf(
			"full e2e: %d full-tier tests are in no bucket: %s. A full-tier test is named %s<Family>_<Case>, "+
				"its family one of fullE2EFamilyBuckets (internal/devtool/verify_e2e_tiers.go); a new family is an edit of that table",
			len(unplaced), strings.Join(unplaced, "; "), e2eTestNamePrefix)
	}

	buckets := fullE2EBucketShapes()
	for i := range buckets {
		names := byBucket[buckets[i].Name]
		slices.Sort(names)
		buckets[i].TestNames = names
	}

	return buckets, nil
}

// discoverFullE2EBuckets discovers the full tier of repoRoot's live suite and
// places its tests in their buckets.
func discoverFullE2EBuckets(repoRoot string) ([]fullE2EBucketSpec, error) {
	tests, err := discoverE2ETests(repoRoot, e2eTierFull)
	if err != nil {
		return nil, err
	}

	return placeFullE2ETests(tests)
}
