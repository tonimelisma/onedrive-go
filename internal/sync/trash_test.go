package sync

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaultTrashFunc_NonDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("test only applicable on non-darwin")
	}

	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")

	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := defaultTrashFunc(path)
	if err == nil {
		t.Fatal("expected error on non-darwin platform")
	}
}

func TestMoveToMacOSTrash(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only test")
	}

	t.Parallel()

	// Create a temp file.
	dir := t.TempDir()
	path := filepath.Join(dir, "trash-test-file.txt")

	if err := os.WriteFile(path, []byte("trash me"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Move to trash.
	if err := moveToMacOSTrash(path); err != nil {
		t.Fatalf("moveToMacOSTrash: %v", err)
	}

	// Original should be gone.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file should have been moved to trash")
	}

	// Clean up from trash.
	home, _ := os.UserHomeDir()
	trashPath := filepath.Join(home, ".Trash", "trash-test-file.txt")
	os.Remove(trashPath) // best-effort cleanup
}

func TestMoveToMacOSTrash_NameCollision(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only test")
	}

	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	trashDir := filepath.Join(home, ".Trash")

	// Create a file in trash with the same name to force collision handling.
	collisionPath := filepath.Join(trashDir, "trash-collision-test.txt")
	if err := os.WriteFile(collisionPath, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	defer os.Remove(collisionPath)

	// Now try to trash a file with the same name.
	dir := t.TempDir()
	path := filepath.Join(dir, "trash-collision-test.txt")

	if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := moveToMacOSTrash(path); err != nil {
		t.Fatalf("moveToMacOSTrash with collision: %v", err)
	}

	// Original should be gone.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file should have been moved to trash")
	}

	// Clean up: the collision file should be "trash-collision-test 2.txt".
	suffix2 := filepath.Join(trashDir, "trash-collision-test 2.txt")
	os.Remove(suffix2) // best-effort cleanup
}
