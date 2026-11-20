package devtool

import (
	"bytes"
	"context"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.10.17, R-6.10.26
//
// The integration tier selects its tests by name, so a test in a file built
// with the integration tag under any other name would never run, and the tier
// would not say so.
func TestRunTestConventionsRequiresTheIntegrationPrefixInIntegrationFiles(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "internal", "graph", "integration_test.go"),
		[]byte(`//go:build integration

package graph

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(m.Run()) }

func TestIntegration_Me(t *testing.T) {}

func TestDrivesLive(t *testing.T) {}

func testingHelper(t *testing.T) {}
`),
	))
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "internal", "graph", "drives_test.go"),
		[]byte("package graph\n\nimport \"testing\"\n\nfunc TestDrives(t *testing.T) {}\n"),
	))

	err := runTestConventions(context.Background(), &fakeRunner{}, repoRoot, nil, &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal/graph/integration_test.go:14: TestDrivesLive")
	assert.NotContains(t, err.Error(), "TestMain")
	assert.NotContains(t, err.Error(), "TestIntegration_Me")
	assert.NotContains(t, err.Error(), "testingHelper")
	assert.NotContains(t, err.Error(), "drives_test.go", "an untagged file is the unit tiers'")
}

// Validates: R-6.10.17
//
// A test seam (a function whose name ends in ForTest) is declared in
// production code for the tests of another package; using one from
// production code is refused, from a test file or a sync test-helper package
// it is not.
func TestRunTestConventionsRefusesAProductionUseOfATestSeam(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "internal", "sync", "store.go"),
		[]byte(`package sync

type Store struct{ rows int }

func (s *Store) RowsForTest() int { return s.rows }

func NewStoreForTest() *Store { return &Store{} }

func count(s *Store) int { return s.RowsForTest() }
`),
	))
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "internal", "sync", "store_test.go"),
		[]byte("package sync\n\nfunc rows() int { return NewStoreForTest().RowsForTest() }\n"),
	))
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "internal", "sync", "internal", "syncfixture", "store.go"),
		[]byte("package syncfixture\n\nimport \"example/sync\"\n\nvar _ = sync.NewStoreForTest\n"),
	))

	err := runTestConventions(context.Background(), &fakeRunner{}, repoRoot, nil, &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal/sync/store.go:9: RowsForTest is a test seam")
	assert.NotContains(t, err.Error(), "store.go:5", "declaring a seam is how it exists")
	assert.NotContains(t, err.Error(), "store.go:7", "declaring a seam is how it exists")
	assert.NotContains(t, err.Error(), "store_test.go", "a test may use a seam")
	assert.NotContains(t, err.Error(), "syncfixture", "a sync test-helper package may use a seam")
}

// Validates: R-6.10.17
//
// A sync test-helper package calls seams on its importer's behalf, so a
// production file that imports one reaches a seam without naming it; the
// import is refused there, and allowed in a test file and between helpers.
func TestRunTestConventionsRefusesAProductionImportOfATestHelperPackage(t *testing.T) {
	t.Parallel()

	const fixture = repoModulePath + "/internal/sync/internal/syncfixture"
	const storeHelper = repoModulePath + "/internal/sync/internal/syncstoretest"
	repoRoot := t.TempDir()
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "internal", "sync", "engine.go"),
		[]byte("package sync\n\nimport (\n\t\"fmt\"\n\n\t\""+fixture+"\"\n)\n\nvar _ = fmt.Sprint(syncfixture.Store)\n"),
	))
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "internal", "sync", "engine_test.go"),
		[]byte("package sync\n\nimport \""+fixture+"\"\n\nvar _ = syncfixture.Store\n"),
	))
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "internal", "sync", "internal", "syncfixture", "store.go"),
		[]byte("package syncfixture\n\nimport \""+storeHelper+"\"\n\nvar Store = syncstoretest.Open\n"),
	))

	err := runTestConventions(context.Background(), &fakeRunner{}, repoRoot, nil, &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal/sync/engine.go:6: "+fixture+" is a sync test-helper package")
	assert.NotContains(t, err.Error(), "engine_test.go", "a test may import a test-helper package")
	assert.NotContains(t, err.Error(), "syncfixture/store.go", "a test-helper package may import another")
}

// Validates: R-6.10.26
//
// The rule follows the constraint, not the word: a tag on either side of an
// operator counts, and a file built only when the tag is off is not the
// integration tier's.
func TestFileBuildConstraintNames_FollowsNegationAndBothOperands(t *testing.T) {
	t.Parallel()

	cases := []struct {
		header string
		want   bool
	}{
		{header: "//go:build integration\n\n", want: true},
		{header: "//go:build integration && linux\n\n", want: true},
		{header: "//go:build linux || integration\n\n", want: true},
		{header: "//go:build !(!integration)\n\n", want: true},
		{header: "// A file comment.\n\n//go:build linux && (e2e || integration)\n\n", want: true},
		{header: "//go:build !integration\n\n", want: false},
		{header: "//go:build !(integration || e2e)\n\n", want: false},
		{header: "//go:build e2e\n\n", want: false},
		{header: "// integration is only a word here.\n", want: false},
		{header: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			t.Parallel()

			file, err := parser.ParseFile(token.NewFileSet(), "sample_test.go",
				tc.header+"package sample\n", parser.ParseComments)
			require.NoError(t, err)
			assert.Equal(t, tc.want, fileBuildConstraintNames(file, integrationBuildTag))
		})
	}
}

// Validates: R-6.10.17
func TestFindTestConventionViolations_RealRepoIsClean(t *testing.T) {
	t.Parallel()

	violations, err := findTestConventionViolations(repoRootForTest(t))
	require.NoError(t, err)
	assert.Empty(t, violations)
}
