//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type sharedItemE2E struct {
	Selector      string `json:"selector"`
	Type          string `json:"type"`
	Name          string `json:"name"`
	AccountEmail  string `json:"account_email"`
	SharedByEmail string `json:"shared_by_email"`
	RemoteDriveID string `json:"remote_drive_id"`
	RemoteItemID  string `json:"remote_item_id"`
}

type sharedListE2EOutput struct {
	Items []sharedItemE2E `json:"items"`
}

type sharedStatE2EOutput struct {
	Name           string `json:"name"`
	AccountEmail   string `json:"account_email"`
	RemoteDriveID  string `json:"remote_drive_id"`
	RemoteItemID   string `json:"remote_item_id"`
	SharedSelector string `json:"shared_selector"`
}

type driveListE2EOutput struct {
	Configured []struct {
		CanonicalID string `json:"canonical_id"`
		SyncDir     string `json:"sync_dir"`
	} `json:"configured"`
}

func requireSharedFileLink(t *testing.T) string {
	t.Helper()

	rawLink := os.Getenv("ONEDRIVE_TEST_SHARED_LINK")
	require.NotEmpty(t, rawLink,
		"shared-file fixture missing: set ONEDRIVE_TEST_SHARED_LINK in exported env, root .env, or .testdata/fixtures.env")

	return rawLink
}

func expandHomePath(path string, env map[string]string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}

	home := env["HOME"]
	if home == "" {
		home = os.Getenv("HOME")
	}
	if home == "" {
		return path
	}

	return filepath.Join(home, path[2:])
}

func runCLIWithoutDrive(t *testing.T, cfgPath string, env map[string]string, args ...string) (string, string) {
	t.Helper()

	stdout, stderr, err := runCLICore(t, cfgPath, env, "", args...)
	require.NoErrorf(t, err, "CLI command %v failed\nstdout: %s\nstderr: %s", args, stdout, stderr)

	return stdout, stderr
}

func sharedListForRecipient(t *testing.T, cfgPath string, env map[string]string, recipientEmail string) sharedListE2EOutput {
	t.Helper()

	stdout, _ := runCLIWithoutDrive(t, cfgPath, env, "--account", recipientEmail, "shared", "--json")

	var parsed sharedListE2EOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))

	return parsed
}

func statSharedTargetJSON(t *testing.T, cfgPath string, env map[string]string, args ...string) sharedStatE2EOutput {
	t.Helper()

	fullArgs := append([]string{"stat", "--json"}, args...)
	stdout, _ := runCLIWithoutDrive(t, cfgPath, env, fullArgs...)

	var parsed sharedStatE2EOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))

	return parsed
}

func getSharedTargetContent(t *testing.T, cfgPath string, env map[string]string, args ...string) string {
	t.Helper()

	localPath := filepath.Join(t.TempDir(), "downloaded")
	fullArgs := append([]string{"get"}, args...)
	fullArgs = append(fullArgs, localPath)
	runCLIWithoutDrive(t, cfgPath, env, fullArgs...)

	data, err := os.ReadFile(localPath)
	require.NoError(t, err)

	return string(data)
}

func writeTempContentFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "upload.txt")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func eventuallySharedContentEquals(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	expected string,
	args ...string,
) {
	t.Helper()

	require.Eventually(t, func() bool {
		localPath := filepath.Join(t.TempDir(), "downloaded")
		fullArgs := append([]string{"get"}, args...)
		fullArgs = append(fullArgs, localPath)

		_, _, err := runCLICore(t, cfgPath, env, "", fullArgs...)
		if err != nil {
			return false
		}

		data, readErr := os.ReadFile(localPath)
		if readErr != nil {
			return false
		}

		return string(data) == expected
	}, pollTimeout, 2*time.Second)
}

func findSharedItemByRemoteIDs(t *testing.T, items []sharedItemE2E, driveID, itemID, itemType string) sharedItemE2E {
	t.Helper()

	for i := range items {
		if items[i].RemoteDriveID == driveID && items[i].RemoteItemID == itemID && items[i].Type == itemType {
			return items[i]
		}
	}

	require.Failf(t, "shared item not found", "drive=%s item=%s type=%s", driveID, itemID, itemType)
	return sharedItemE2E{}
}

func findSharedItemByNameAndType(t *testing.T, items []sharedItemE2E, name, itemType string) sharedItemE2E {
	t.Helper()

	for i := range items {
		if items[i].Name == name && items[i].Type == itemType {
			return items[i]
		}
	}

	require.Failf(t, "shared item not found", "name=%s type=%s", name, itemType)
	return sharedItemE2E{}
}
