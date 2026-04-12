//go:build e2e

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

// Validates: R-1.1, R-1.2, R-1.3
func TestE2E_FileOpsSmokeCRUD(t *testing.T) {
	registerLogDump(t)

	cfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("onedrive-go-e2e-smoke-%d", time.Now().UnixNano())
	remoteFolder := "/" + testFolder
	remotePath := remoteFolder + "/smoke.txt"
	content := []byte("fast file-ops smoke\n")

	t.Cleanup(func() {
		cleanupRemoteFolder(t, testFolder)
	})

	_, stderr := runCLIWithConfig(t, cfgPath, nil, "mkdir", remoteFolder)
	assert.Contains(t, stderr, "Created")

	tmpFile, err := os.CreateTemp("", "e2e-smoke-upload-*")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.Write(content)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	_, stderr = runCLIWithConfig(t, cfgPath, nil, "put", tmpFile.Name(), remotePath)
	assert.Contains(t, stderr, "Uploaded")
	waitForRemoteFixtureSeedVisible(t, cfgPath, nil, drive, remotePath)

	localPath := filepath.Join(t.TempDir(), "smoke.txt")
	_, stderr = runCLIWithConfig(t, cfgPath, nil, "get", remotePath, localPath)
	assert.Contains(t, stderr, "Downloaded")

	downloaded, err := os.ReadFile(localPath)
	require.NoError(t, err)
	assert.Equal(t, content, downloaded)

	_, stderr = runCLIWithConfig(t, cfgPath, nil, "rm", remotePath)
	assert.Contains(t, stderr, "Deleted")
	waitForRemoteDeleteDisappearance(t, cfgPath, nil, drive, "smoke.txt", "ls", remoteFolder)
}
