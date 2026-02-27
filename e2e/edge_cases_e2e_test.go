//go:build e2e && e2e_full

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// §10: E2E Edge Case Tests (require live OneDrive account)
//
// These tests validate real API behaviors that cannot be unit-tested.
// Tagged e2e,e2e_full — runs in nightly/manual CI only (30-min timeout).
// ---------------------------------------------------------------------------

// TestE2E_ZeroByteFileSync validates that zero-byte files can be uploaded
// and downloaded with correct metadata via the OneDrive API.
func TestE2E_ZeroByteFileSync(t *testing.T) {
	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-zero-byte-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create a zero-byte file locally and upload.
	// Two syncs needed: first creates the remote folder (commits baseline),
	// second uploads the file (can now resolve parent ID).
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "empty.txt"), []byte{}, 0o644))
	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")
	assert.Contains(t, stderr, "Mode: upload-only")

	// Verify it exists remotely.
	stdout, _ := runCLI(t, "ls", "/"+testFolder)
	assert.Contains(t, stdout, "empty.txt")

	// Download via get (bypasses sync state) and verify zero-byte content.
	downloadPath := filepath.Join(t.TempDir(), "empty-downloaded.txt")
	runCLI(t, "get", "/"+testFolder+"/empty.txt", downloadPath)

	info, err := os.Stat(downloadPath)
	require.NoError(t, err)
	assert.Equal(t, int64(0), info.Size(), "zero-byte file should arrive as zero bytes")
}

// TestE2E_UnicodeFilenameRoundtrip validates that files with accented
// characters (NFC normalization) survive a full upload-download cycle.
func TestE2E_UnicodeFilenameRoundtrip(t *testing.T) {
	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-unicode-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create file with accented characters and upload.
	// Two syncs needed: first creates the remote folder (commits baseline),
	// second uploads the file (can now resolve parent ID).
	unicodeName := "café résumé.txt"
	content := "Unicode content: àéîõü"
	require.NoError(t, os.WriteFile(filepath.Join(localDir, unicodeName), []byte(content), 0o644))
	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")
	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")

	// Verify it exists remotely via ls.
	stdout, _ := runCLI(t, "ls", "/"+testFolder)
	assert.Contains(t, stdout, "caf")

	// Download via get and verify content roundtrip.
	downloadPath := filepath.Join(t.TempDir(), unicodeName)
	runCLI(t, "get", "/"+testFolder+"/"+unicodeName, downloadPath)

	data, err := os.ReadFile(downloadPath)
	require.NoError(t, err)
	assert.Equal(t, content, string(data), "unicode file content should roundtrip")
}

// TestE2E_InvalidFilenameRejection validates that OneDrive-invalid filenames
// are not uploaded and produce warnings.
func TestE2E_InvalidFilenameRejection(t *testing.T) {
	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-invalid-name-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Create a valid file (for folder structure).
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "valid.txt"), []byte("valid"), 0o644))
	// Create an invalid file (CON is a Windows reserved name).
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "CON"), []byte("invalid"), 0o644))

	// Upload — two syncs to ensure folder creation commits before file upload.
	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")

	// The debug stderr should mention skipping the invalid name.
	assert.Contains(t, stderr, "skipping invalid OneDrive name",
		"should log warning about invalid filename")

	// Verify only valid.txt appeared remotely.
	stdout, _ := runCLI(t, "ls", "/"+testFolder)
	assert.Contains(t, stdout, "valid.txt")
	assert.NotContains(t, stdout, "CON",
		"invalid filename should not be uploaded")
}

// TestE2E_RapidFileChurn validates that creating, modifying, deleting,
// and re-creating a file in quick succession results in the correct
// final state after sync.
func TestE2E_RapidFileChurn(t *testing.T) {
	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-churn-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	filePath := filepath.Join(localDir, "churn.txt")

	// Create → modify → modify → delete → re-create with final content.
	require.NoError(t, os.WriteFile(filePath, []byte("v1"), 0o644))
	require.NoError(t, os.WriteFile(filePath, []byte("v2"), 0o644))
	require.NoError(t, os.WriteFile(filePath, []byte("v3"), 0o644))
	require.NoError(t, os.Remove(filePath))
	finalContent := "final version after churn"
	require.NoError(t, os.WriteFile(filePath, []byte(finalContent), 0o644))

	// Upload — two syncs to ensure folder creation commits before file upload.
	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")
	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")

	// Verify final content remotely.
	remoteContent := getRemoteFile(t, "/"+testFolder+"/churn.txt")
	assert.Equal(t, finalContent, remoteContent,
		"final state should be the last written content")
}

// TestE2E_BigDeleteProtection validates that deleting a large fraction
// of files locally triggers the big-delete protection mechanism.
//
// Note: This test is unreliable with the shared global state DB because
// the big-delete percentage is calculated against ALL baseline entries
// (not just the test folder). When many entries from other tests exist,
// 20 deletes may fall below the 50% threshold. The unit test
// TestPlan_BigDeleteBlocked in planner_edge_test.go covers this logic
// with precise control over the baseline.
func TestE2E_BigDeleteProtection(t *testing.T) {
	t.Skip("unreliable with shared global state DB — big-delete threshold depends on total baseline count from all E2E tests")
}

// TestE2E_ConflictDetectionAndResolution validates the full conflict
// lifecycle: create conflicting files, detect the conflict, resolve it.
func TestE2E_ConflictDetectionAndResolution(t *testing.T) {
	syncDir := t.TempDir()
	cfgPath := writeSyncConfig(t, syncDir)

	testFolder := fmt.Sprintf("e2e-conflict-%d", time.Now().UnixNano())
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o755))

	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	// Step 1: Create file and sync to establish baseline.
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "shared.txt"), []byte("original"), 0o644))
	runCLIWithConfig(t, cfgPath, "sync", "--upload-only", "--force")

	// Step 2: Modify remote side directly.
	putRemoteFile(t, "/"+testFolder+"/shared.txt", "remote version")

	// Step 3: Modify local side.
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "shared.txt"), []byte("local version"), 0o644))

	// Step 4: Bidirectional sync → should detect conflict.
	_, stderr := runCLIWithConfig(t, cfgPath, "sync", "--force")
	assert.Contains(t, stderr, "conflict",
		"sync should report conflict")

	// Step 5: Check conflicts list.
	stdout, _ := runCLIWithConfig(t, cfgPath, "conflicts")
	assert.Contains(t, stdout, "shared.txt",
		"conflicts list should include the conflicting file")

	// Step 6: Resolve the conflict.
	runCLIWithConfig(t, cfgPath, "resolve", testFolder+"/shared.txt", "--keep-local")

	// Step 7: Verify conflict is resolved.
	stdout, _ = runCLIWithConfig(t, cfgPath, "conflicts")
	assert.NotContains(t, stdout, "shared.txt",
		"conflicts list should be empty after resolution")
}
