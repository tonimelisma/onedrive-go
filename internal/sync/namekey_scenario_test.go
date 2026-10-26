package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.12.3, R-5.6.14
func TestRunOnce_FullCaseFoldingVariantNeverOverwritesTheSyncedFile(t *testing.T) {
	t.Parallel()

	// Manual test: STRASSE.txt was uploaded with replace onto straße.txt,
	// which OneDrive treats as the same name, and the original content was
	// gone. strings.ToLower does not fold ß to ss.
	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-strasse", "root", "straße.txt", "original\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "straße.txt"))

	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "STRASSE.txt"), []byte("other\n"), 0o600))
	runScenarioPass(t, eng)

	assert.Zero(t, remote.uploads, "the variant must be held as a collision, not uploaded")
	assert.Equal(t, map[string]string{"straße.txt": "original\n"}, remote.tree())
	requireScenarioIssue(t, eng, "STRASSE.txt", issueCaseCollision)
}

// Validates: R-2.12.3
func TestRunOnce_EmojiOnlyVariantIsACollisionNotAnEndlessConflict(t *testing.T) {
	t.Parallel()

	// Manual test: a.txt retried "graph: conflict" forever against a synced
	// a😀.txt, because OneDrive ignores the emoji when comparing names (L-30).
	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-emoji", "root", "a\U0001F600.txt", "emoji\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "a.txt"), []byte("plain\n"), 0o600))
	runScenarioPass(t, eng)

	assert.Zero(t, remote.uploads)
	assert.Equal(t, map[string]string{"a\U0001F600.txt": "emoji\n"}, remote.tree())
	requireScenarioIssue(t, eng, "a.txt", issueCaseCollision)
}

func requireScenarioIssue(t *testing.T, eng *testEngine, path, issueType string) {
	t.Helper()

	issues, err := eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	for i := range issues {
		if issues[i].Path == path && issues[i].IssueType == issueType {
			return
		}
	}
	require.Failf(t, "missing issue", "%s on %s; have %+v", issueType, path, issues)
}

// Validates: R-2.12.4
func TestContentFilter_SpellingRenameArtifactIsAlwaysExcluded(t *testing.T) {
	t.Parallel()

	// A crash between the two renames of a spelling-only rename must not
	// leave a file that syncs under the temporary name, whatever the filter
	// settings (junk filtering and dotfile filtering both off here).
	filter := newContentFilter(ContentFilterConfig{})
	assert.False(t, filter.Visible(spellingRenameArtifactPrefix+"EDIT.txt", ItemTypeFile))
	assert.False(t, filter.Visible("docs/"+spellingRenameArtifactPrefix+"EDIT.txt-1", ItemTypeFile))
	assert.True(t, filter.Visible("docs/EDIT.txt", ItemTypeFile))
}
