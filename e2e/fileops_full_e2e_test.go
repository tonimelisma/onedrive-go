//go:build e2e && e2e_full

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These extended direct file-operation checks moved to the full battery so
// the per-PR live lane keeps only the minimum CRUD smoke.
func TestE2E_RoundTrip(t *testing.T) {
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("onedrive-go-e2e-%d", time.Now().UnixNano())
	testSubfolder := testFolder + "/subfolder"
	testFile := testFolder + "/test.txt"
	testContent := []byte("Hello from onedrive-go E2E test!\n")

	t.Cleanup(func() {
		cleanupRemoteFolder(t, testFolder)
	})

	t.Run("whoami", func(t *testing.T) {
		stdout, _ := pollCLIWithConfigRetryingTransientGraphFailures(
			t, cfgPath, nil, drive, transientGraphRetryTimeout, "whoami", "--json",
		)

		var out map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(stdout), &out))
		assert.Contains(t, out, "user")
		assert.Contains(t, out, "drives")

		drives, ok := out["drives"].([]interface{})
		require.True(t, ok)
		assert.NotEmpty(t, drives)
	})

	t.Run("ls_root", func(t *testing.T) {
		stdout, _ := runCLIWithConfig(t, cfgPath, nil, "ls", "/")
		assert.Contains(t, stdout, "NAME")
	})

	t.Run("mkdir", func(t *testing.T) {
		_, stderr := runCLIWithConfig(t, cfgPath, nil, "mkdir", "/"+testSubfolder)
		assert.Contains(t, stderr, "Created")
	})

	t.Run("put", func(t *testing.T) {
		tmpFile, err := os.CreateTemp("", "e2e-upload-*")
		require.NoError(t, err)
		defer os.Remove(tmpFile.Name())

		_, err = tmpFile.Write(testContent)
		require.NoError(t, err)
		require.NoError(t, tmpFile.Close())

		_, stderr := runCLIWithConfig(t, cfgPath, nil, "put", tmpFile.Name(), "/"+testFile)
		assert.Contains(t, stderr, "Uploaded")
	})

	t.Run("ls_folder", func(t *testing.T) {
		stdout, _ := waitForRemoteParentListingContains(t, cfgPath, nil, drive, "/"+testFolder, "test.txt")
		assert.Contains(t, stdout, "subfolder")
	})

	t.Run("stat", func(t *testing.T) {
		stdout, _ := waitForRemoteExactStatVisible(t, cfgPath, nil, drive, "/"+testFile)
		assert.Contains(t, stdout, fmt.Sprintf("%d bytes", len(testContent)))
	})

	t.Run("get", func(t *testing.T) {
		tmpDir := t.TempDir()
		localPath := filepath.Join(tmpDir, "downloaded.txt")

		_, stderr := runCLIWithConfig(t, cfgPath, nil, "get", "/"+testFile, localPath)
		assert.Contains(t, stderr, "Downloaded")

		downloaded, err := os.ReadFile(localPath)
		require.NoError(t, err)
		assert.Equal(t, testContent, downloaded)
	})

	t.Run("rm_file", func(t *testing.T) {
		_, stderr := runCLIWithConfig(t, cfgPath, nil, "rm", "/"+testFile)
		assert.Contains(t, stderr, "Deleted")
		waitForRemoteDeleteDisappearance(t, cfgPath, nil, drive, "test.txt", "ls", "/"+testFolder)
	})

	t.Run("rm_subfolder", func(t *testing.T) {
		_, stderr := runCLIWithConfig(t, cfgPath, nil, "rm", "-r", "/"+testSubfolder)
		assert.Contains(t, stderr, "Deleted")
		waitForRemoteDeleteDisappearance(t, cfgPath, nil, drive, "subfolder", "ls", "/"+testFolder)
	})

	t.Run("rm_permanent", func(t *testing.T) {
		tmpFile, err := os.CreateTemp("", "e2e-perm-*")
		require.NoError(t, err)
		defer os.Remove(tmpFile.Name())

		_, err = tmpFile.Write([]byte("permanent delete test\n"))
		require.NoError(t, err)
		require.NoError(t, tmpFile.Close())

		permFile := testFolder + "/perm-test.txt"
		_, stderr := runCLIWithConfig(t, cfgPath, nil, "put", tmpFile.Name(), "/"+permFile)
		assert.Contains(t, stderr, "Uploaded")

		waitForRemoteFixtureSeedVisible(t, cfgPath, nil, drive, "/"+permFile)

		_, stderr = runCLIWithConfig(t, cfgPath, nil, "rm", "--permanent", "/"+permFile)
		assert.Contains(t, stderr, "Permanently deleted")
		waitForRemoteDeleteDisappearance(t, cfgPath, nil, drive, "perm-test.txt", "ls", "/"+testFolder)
	})

	t.Run("whoami_text", func(t *testing.T) {
		stdout, _ := pollCLIWithConfigRetryingTransientGraphFailures(
			t, cfgPath, nil, drive, transientGraphRetryTimeout, "whoami",
		)

		email := strings.SplitN(drive, ":", 2)[1]
		assert.Contains(t, stdout, email, "whoami text output should contain the account email")
	})

	t.Run("status", func(t *testing.T) {
		stdout, _ := runCLIWithConfig(t, cfgPath, nil, "status")
		assert.Contains(t, stdout, drive, "status should show the configured drive section")
		assert.Contains(t, stdout, "Auth:", "status should show auth state")
		assert.Contains(t, stdout, "State DB:  missing", "status should surface missing state DB for unsynced drives")
	})
}

func TestE2E_ErrorCases(t *testing.T) {
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	t.Run("ls_not_found", func(t *testing.T) {
		output := runCLIWithConfigExpectError(t, cfgPath, nil, "ls", "/nonexistent-uuid-path-12345")
		assert.Contains(t, output, "nonexistent-uuid-path-12345")
	})

	t.Run("get_not_found", func(t *testing.T) {
		output := runCLIWithConfigExpectError(t, cfgPath, nil, "get", "/nonexistent-uuid-path-12345")
		assert.Contains(t, output, "nonexistent-uuid-path-12345")
	})

	t.Run("rm_not_found", func(t *testing.T) {
		output := runCLIWithConfigExpectError(t, cfgPath, nil, "rm", "/nonexistent-uuid-path-12345")
		assert.Contains(t, output, "nonexistent-uuid-path-12345")
	})

	t.Run("rm_folder_without_recursive", func(t *testing.T) {
		testFolder := fmt.Sprintf("onedrive-go-e2e-rmfolder-%d", time.Now().UnixNano())

		t.Cleanup(func() {
			cleanupRemoteFolder(t, testFolder)
		})

		runCLIWithConfig(t, cfgPath, nil, "mkdir", "/"+testFolder)
		output := runCLIWithConfigExpectError(t, cfgPath, nil, "rm", "/"+testFolder)
		assert.Contains(t, output, "recursive")
	})
}

func TestE2E_JSONOutput(t *testing.T) {
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	t.Run("ls_json", func(t *testing.T) {
		stdout, _ := runCLIWithConfig(t, cfgPath, nil, "ls", "--json", "/")

		var items []map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(stdout), &items),
			"ls --json output should be valid JSON array, got: %s", stdout)

		require.NotEmpty(t, items, "expected at least one item in root listing")

		for i, item := range items {
			assert.Contains(t, item, "name", "item %d missing 'name' key", i)
			assert.Contains(t, item, "id", "item %d missing 'id' key", i)
		}
	})

	t.Run("stat_json", func(t *testing.T) {
		stdout, _ := runCLIWithConfig(t, cfgPath, nil, "stat", "--json", "/")

		var obj map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(stdout), &obj),
			"stat --json output should be valid JSON object, got: %s", stdout)

		assert.Contains(t, obj, "name", "stat JSON missing 'name' key")
		assert.Contains(t, obj, "id", "stat JSON missing 'id' key")
	})
}

func TestE2E_QuietFlag(t *testing.T) {
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	t.Run("put_quiet_suppresses_output", func(t *testing.T) {
		testFolder := fmt.Sprintf("onedrive-go-e2e-quiet-%d", time.Now().UnixNano())
		remotePath := "/" + testFolder + "/quiet-test.txt"

		t.Cleanup(func() {
			cleanupRemoteFolder(t, testFolder)
		})

		runCLIWithConfig(t, cfgPath, nil, "mkdir", "/"+testFolder)

		tmpFile, err := os.CreateTemp("", "e2e-quiet-*")
		require.NoError(t, err)
		defer os.Remove(tmpFile.Name())

		_, err = tmpFile.Write([]byte("quiet test content\n"))
		require.NoError(t, err)
		require.NoError(t, tmpFile.Close())

		_, stderr := runCLIWithConfig(t, cfgPath, nil, "put", "--quiet", tmpFile.Name(), remotePath)
		assert.Empty(t, stderr, "expected no stderr output with --quiet flag")
	})
}
