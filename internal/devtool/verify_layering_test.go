package devtool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// layeringFixture builds a repo shaped like the real one: two design docs that
// declare a rank and own one file each, plus the Go package they govern.
func layeringFixture(t *testing.T, lowSrc, highSrc string, docExtras ...string) string {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, specDesignDir), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "internal", "demo"), 0o750))

	lowDoc := "# Low\n\nGOVERNS: internal/demo/low.go\n\nLAYER: 0\n\nImplements: R-1 [verified]\n"
	highDoc := "# High\n\nGOVERNS: internal/demo/high.go\n\nLAYER: 1\n\nImplements: R-1 [verified]\n"
	if len(docExtras) > 0 {
		lowDoc = docExtras[0]
	}

	require.NoError(t, os.WriteFile(filepath.Join(root, specDesignDir, "low.md"), []byte(lowDoc), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, specDesignDir, "high.md"), []byte(highDoc), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "internal", "demo", "low.go"), []byte(lowSrc), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "internal", "demo", "high.go"), []byte(highSrc), 0o600))

	return root
}

// Validates: R-6.10.20
func TestFindLayeringViolations_RejectsUpwardReference(t *testing.T) {
	t.Parallel()

	root := layeringFixture(t,
		"package demo\n\nfunc lowUsesHigh() highThing { return highThing{} }\n",
		"package demo\n\ntype highThing struct{}\n",
	)

	violations, err := findLayeringViolations(root)
	require.NoError(t, err)
	require.Len(t, violations, 1, "a layer-0 file reading a layer-1 symbol is a violation")
	assert.Equal(t, "highThing", violations[0].symbol)
	assert.Equal(t, 0, violations[0].fromRank)
	assert.Equal(t, 1, violations[0].toRank)
	assert.Contains(t, violations[0].String(), "may not depend upward")
}

// Validates: R-6.10.20
func TestFindLayeringViolations_AcceptsDownwardReference(t *testing.T) {
	t.Parallel()

	root := layeringFixture(t,
		"package demo\n\ntype lowThing struct{}\n",
		"package demo\n\nfunc highUsesLow() lowThing { return lowThing{} }\n",
	)

	violations, err := findLayeringViolations(root)
	require.NoError(t, err)
	assert.Empty(t, violations, "a higher layer may depend on a lower one")
}

// A family that declares no rank is not yet part of the check, in either
// direction. The set of unranked families is the remaining work and is visible
// in the docs, which is a more honest instrument than a growing allowlist.
//
// Validates: R-6.10.20
func TestFindLayeringViolations_UnrankedFamilyIsNotChecked(t *testing.T) {
	t.Parallel()

	root := layeringFixture(t,
		"package demo\n\nfunc lowUsesHigh() highThing { return highThing{} }\n",
		"package demo\n\ntype highThing struct{}\n",
		"# Low\n\nGOVERNS: internal/demo/low.go\n\nImplements: R-1 [verified]\n",
	)

	violations, err := findLayeringViolations(root)
	require.NoError(t, err)
	assert.Empty(t, violations, "an unranked family opts out until it declares a rank")
}

// Same-rank families may reference each other: two families can be genuinely
// mutually recursive without either sitting above the other.
//
// Validates: R-6.10.20
func TestFindLayeringViolations_SameRankReferenceIsAllowed(t *testing.T) {
	t.Parallel()

	root := layeringFixture(t,
		"package demo\n\nfunc lowUsesHigh() highThing { return highThing{} }\n",
		"package demo\n\ntype highThing struct{}\n",
		"# Low\n\nGOVERNS: internal/demo/low.go\n\nLAYER: 1\n\nImplements: R-1 [verified]\n",
	)

	violations, err := findLayeringViolations(root)
	require.NoError(t, err)
	assert.Empty(t, violations)
}

// Validates: R-6.10.20
func TestReadLayerRanks_ReadsOnlyDocsThatDeclareOne(t *testing.T) {
	t.Parallel()

	root := layeringFixture(t, "package demo\n", "package demo\n",
		"# Low\n\nGOVERNS: internal/demo/low.go\n\nImplements: R-1 [verified]\n")

	ranks, err := readLayerRanks(root)
	require.NoError(t, err)
	require.Len(t, ranks, 1)
	assert.Equal(t, 1, ranks[filepath.Join(specDesignDir, "high.md")])
}
