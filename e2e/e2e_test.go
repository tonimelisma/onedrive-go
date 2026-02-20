//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	binaryPath string
	profile    string
)

func TestMain(m *testing.M) {
	// Build binary to temp dir.
	tmpDir, err := os.MkdirTemp("", "onedrive-e2e-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	binaryPath = filepath.Join(tmpDir, "onedrive-go")

	cmd := exec.Command("go", "build", "-o", binaryPath, ".")
	cmd.Dir = findModuleRoot()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "building binary: %v\n", err)
		os.Exit(1)
	}

	profile = os.Getenv("ONEDRIVE_TEST_PROFILE")
	if profile == "" {
		profile = "default"
	}

	os.Exit(m.Run())
}

// findModuleRoot walks up from the current dir to find go.mod.
func findModuleRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Fallback to ".." — e2e/ is one level below module root.
			return ".."
		}

		dir = parent
	}
}

func runCLI(t *testing.T, args ...string) (string, string) {
	t.Helper()

	fullArgs := append([]string{"--profile", profile}, args...)
	cmd := exec.Command(binaryPath, fullArgs...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		t.Fatalf("CLI command %v failed: %v\nstdout: %s\nstderr: %s", args, err, stdout.String(), stderr.String())
	}

	return stdout.String(), stderr.String()
}

func runCLIExpectError(t *testing.T, args ...string) (string, string) {
	t.Helper()

	fullArgs := append([]string{"--profile", profile}, args...)
	cmd := exec.Command(binaryPath, fullArgs...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	_ = cmd.Run()

	return stdout.String(), stderr.String()
}

func TestE2E_RoundTrip(t *testing.T) {
	testFolder := fmt.Sprintf("onedrive-go-e2e-%d", time.Now().UnixNano())
	testSubfolder := testFolder + "/subfolder"
	testFile := testFolder + "/test.txt"
	testContent := []byte("Hello from onedrive-go E2E test!\n")

	// Cleanup at the end — delete the test folder.
	t.Cleanup(func() {
		// Best-effort cleanup — ignore errors.
		fullArgs := []string{"--profile", profile, "rm", "/" + testFolder}
		cmd := exec.Command(binaryPath, fullArgs...)
		_ = cmd.Run()
	})

	t.Run("whoami", func(t *testing.T) {
		stdout, _ := runCLI(t, "whoami", "--json")

		var out map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(stdout), &out))
		assert.Contains(t, out, "user")
		assert.Contains(t, out, "drives")

		drives, ok := out["drives"].([]interface{})
		require.True(t, ok)
		assert.NotEmpty(t, drives)
	})

	t.Run("ls_root", func(t *testing.T) {
		stdout, _ := runCLI(t, "ls", "/")
		assert.Contains(t, stdout, "NAME")
	})

	t.Run("mkdir", func(t *testing.T) {
		_, stderr := runCLI(t, "mkdir", "/"+testSubfolder)
		assert.Contains(t, stderr, "Created")
	})

	t.Run("put", func(t *testing.T) {
		// Write test content to a local temp file.
		tmpFile, err := os.CreateTemp("", "e2e-upload-*")
		require.NoError(t, err)
		defer os.Remove(tmpFile.Name())

		_, err = tmpFile.Write(testContent)
		require.NoError(t, err)
		require.NoError(t, tmpFile.Close())

		_, stderr := runCLI(t, "put", tmpFile.Name(), "/"+testFile)
		assert.Contains(t, stderr, "Uploaded")
	})

	t.Run("ls_folder", func(t *testing.T) {
		stdout, _ := runCLI(t, "ls", "/"+testFolder)
		assert.Contains(t, stdout, "test.txt")
		assert.Contains(t, stdout, "subfolder")
	})

	t.Run("stat", func(t *testing.T) {
		stdout, _ := runCLI(t, "stat", "/"+testFile)
		assert.Contains(t, stdout, "test.txt")
		assert.Contains(t, stdout, fmt.Sprintf("%d bytes", len(testContent)))
	})

	t.Run("get", func(t *testing.T) {
		tmpDir := t.TempDir()
		localPath := filepath.Join(tmpDir, "downloaded.txt")

		_, stderr := runCLI(t, "get", "/"+testFile, localPath)
		assert.Contains(t, stderr, "Downloaded")

		downloaded, err := os.ReadFile(localPath)
		require.NoError(t, err)
		assert.Equal(t, testContent, downloaded)
	})

	t.Run("rm_file", func(t *testing.T) {
		_, stderr := runCLI(t, "rm", "/"+testFile)
		assert.Contains(t, stderr, "Deleted")
	})

	t.Run("rm_subfolder", func(t *testing.T) {
		_, stderr := runCLI(t, "rm", "/"+testSubfolder)
		assert.Contains(t, stderr, "Deleted")
	})
}
