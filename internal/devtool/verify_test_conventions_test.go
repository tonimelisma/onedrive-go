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

// Validates: R-6.10.17
func TestRunTestConventionsRejectsStdlibTestingFailures(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "bad_test.go"),
		[]byte(`package sample

import "testing"

func TestBad(t *testing.T) {
	t.Fatal("stop")
	t.Errorf("bad")
}
`),
	))

	stdout := &bytes.Buffer{}
	err := runTestConventions(context.Background(), &fakeRunner{}, repoRoot, nil, stdout, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "test convention check failed")
	assert.Contains(t, err.Error(), "bad_test.go:6: direct t.Fatal")
	assert.Contains(t, err.Error(), "bad_test.go:7: direct t.Errorf")
	assert.Contains(t, stdout.String(), "==> test conventions")
}

// Validates: R-6.10.17
func TestRunTestConventionsRejectsDirectSleepOutsideLiveE2EHelper(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "sleepy_test.go"),
		[]byte(`package sample

import (
	"testing"
	"time"
)

func TestSleepy(t *testing.T) {
	time.Sleep(time.Second)
}
`),
	))

	err := runTestConventions(context.Background(), &fakeRunner{}, repoRoot, nil, &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sleepy_test.go:9: direct time.Sleep")
}

// Validates: R-6.10.17
func TestRunTestConventionsRejectsAliasedDirectSleep(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "sleepy_test.go"),
		[]byte(`package sample

import (
	"testing"
	tm "time"
)

func TestSleepy(t *testing.T) {
	tm.Sleep(tm.Second)
}
`),
	))

	err := runTestConventions(context.Background(), &fakeRunner{}, repoRoot, nil, &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sleepy_test.go:9: direct tm.Sleep")
}

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

// Validates: R-6.10.17
func TestRunTestConventionsAllowsCentralizedLiveE2ESleepHelper(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, allowedDirectSleepTestFile),
		[]byte(`package e2e

import (
	"testing"
	"time"
)

func TestHelper(t *testing.T) {
	time.Sleep(time.Millisecond)
}
`),
	))

	err := runTestConventions(context.Background(), &fakeRunner{}, repoRoot, nil, &bytes.Buffer{}, &bytes.Buffer{})
	require.NoError(t, err)
}
