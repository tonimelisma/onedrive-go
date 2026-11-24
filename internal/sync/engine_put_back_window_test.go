package sync

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// putBackWindowWarnings are the warnings of capture that name the put-back
// window.
func putBackWindowWarnings(capture *synctest.LogCapture) []synctest.CapturedRecord {
	var out []synctest.CapturedRecord
	for _, record := range capture.AtLeast(slog.LevelWarn) {
		if strings.Contains(record.Message, putBackWindowWarning) {
			out = append(out, record)
		}
	}

	return out
}

// On a sync folder whose filesystem offers neither hard links nor the
// no-replace rename (FAT-family), putting a set-aside file back is a check
// and a plain rename, and a save landing between the two is replaced. The
// engine says so once, at start, naming the sync folder.
//
// Validates: R-6.4.8
func TestEngine_WarnsOnceAtStartWhenAPutBackCanReplaceASave(t *testing.T) {
	t.Parallel()

	capture, logger := synctest.NewLogCapture()
	eng, syncRoot := newTestEngineInDirWith(t, t.Context(), &syncfixture.EngineMockClient{}, t.TempDir(), func(inputs *engineInputs) {
		inputs.Logger = logger
		inputs.hooks.adaptSyncTree = (*synctree.Root).WithoutNoReplaceFlagOrHardLinks
	})
	require.NotNil(t, eng)

	warnings := putBackWindowWarnings(capture)
	require.Len(t, warnings, 1)
	assert.Equal(t, syncRoot, warnings[0].Attrs["path"])
	assert.Empty(t, syncfixture.LocalNames(t, syncRoot), "the probe leaves nothing in the sync folder")
}

// A filesystem with hard links puts a file back by a link, which never
// replaces: no warning.
//
// Validates: R-6.4.8
func TestEngine_NoPutBackWarningWhereTheFilesystemHasHardLinks(t *testing.T) {
	t.Parallel()
	synctest.SkipWithoutHardLinks(t)

	capture, logger := synctest.NewLogCapture()
	_, _ = newTestEngineInDirWith(t, t.Context(), &syncfixture.EngineMockClient{}, t.TempDir(), func(inputs *engineInputs) {
		inputs.Logger = logger
	})

	assert.Empty(t, putBackWindowWarnings(capture))
}
