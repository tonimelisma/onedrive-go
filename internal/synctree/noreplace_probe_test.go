package synctree_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// A filesystem with hard links lets the put-back of a set-aside file link it
// back without a window, and the probe leaves nothing in the sync folder.
//
// Validates: R-6.4.8
func TestRoot_ProbeNoReplaceSupport_FindsHardLinksAndLeavesNothing(t *testing.T) {
	t.Parallel()
	synctest.SkipWithoutHardLinks(t)

	dir := t.TempDir()
	tree, err := synctree.Open(dir)
	require.NoError(t, err)

	support, err := tree.ProbeNoReplaceSupport()

	require.NoError(t, err)
	assert.True(t, support.HardLinks)
	assert.False(t, support.PutBackCanReplace())
	assert.Empty(t, namesIn(t, dir), "the probe removes its files")
}

// A filesystem with neither the no-replace flag nor hard links (FAT-family)
// leaves the put-back its check and plain rename: the probe says so, and
// leaves nothing in the sync folder.
//
// Validates: R-6.4.8
func TestRoot_ProbeNoReplaceSupport_FindsNeitherWhereTheSystemOffersNeither(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tree, err := synctree.Open(dir)
	require.NoError(t, err)

	support, err := tree.WithoutNoReplaceFlagOrHardLinks().ProbeNoReplaceSupport()

	require.NoError(t, err)
	assert.Equal(t, synctree.NoReplaceSupport{}, support)
	assert.True(t, support.PutBackCanReplace())
	assert.Empty(t, namesIn(t, dir), "the probe removes its files")
}

// The probe's file is named so that observation ignores it, should a crash
// leave it.
//
// Validates: R-6.4.8
func TestIsProbeArtifactName(t *testing.T) {
	t.Parallel()

	assert.True(t, synctree.IsProbeArtifactName(".onedrive-go.0123456789abcdef.probe"))
	assert.False(t, synctree.IsProbeArtifactName("notes.probe"))
	assert.False(t, synctree.IsProbeArtifactName(".onedrive-go.0123456789abcdef.detach"))
}

// A run that died inside the probe left its empty file; the next probe
// removes every such leftover before it makes its own, and nothing else.
//
// Validates: R-6.4.8
func TestRoot_ProbeNoReplaceSupport_RemovesWhatAnEarlierProbeLeft(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	leftover := ".onedrive-go.0123abcd.probe"
	require.NoError(t, os.WriteFile(filepath.Join(dir, leftover), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.probe"), []byte("mine"), 0o600))
	tree, err := synctree.Open(dir)
	require.NoError(t, err)

	_, err = tree.ProbeNoReplaceSupport()

	require.NoError(t, err)
	assert.Equal(t, []string{"keep.probe"}, namesIn(t, dir))
}
