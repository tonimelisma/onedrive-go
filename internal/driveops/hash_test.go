package driveops

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// hashContent computes the QuickXorHash of a string, returning the
// base64-encoded digest. Matches the output of ComputeQuickXorHash for
// the same content written to a file.
func hashContent(t *testing.T, content string) string {
	t.Helper()

	h := quickxorhash.New()
	if _, err := h.Write([]byte(content)); err != nil {
		t.Fatalf("hash.Write: %v", err)
	}

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func TestComputeQuickXorHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "hello world"
	path := filepath.Join(dir, "test.txt")

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	hash, err := ComputeQuickXorHash(path)
	if err != nil {
		t.Fatalf("ComputeQuickXorHash: %v", err)
	}

	want := hashContent(t, content)
	if hash != want {
		t.Errorf("hash = %q, want %q", hash, want)
	}
}

func TestComputeQuickXorHash_EmptyFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")

	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	hash, err := ComputeQuickXorHash(path)
	if err != nil {
		t.Fatalf("ComputeQuickXorHash: %v", err)
	}

	want := hashContent(t, "")
	if hash != want {
		t.Errorf("hash = %q, want %q", hash, want)
	}

	if hash == "" {
		t.Error("empty file hash should not be empty string")
	}
}

func TestComputeQuickXorHash_NonexistentFile(t *testing.T) {
	t.Parallel()

	_, err := ComputeQuickXorHash("/nonexistent/path/file.txt")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
