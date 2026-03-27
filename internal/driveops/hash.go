package driveops

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// HasHash reports whether the item has at least one content hash.
// Items without hashes (e.g., some SharePoint files) cannot be hash-verified.
// Delegates to SelectHash to avoid duplicating the hash field enumeration.
func HasHash(item *graph.Item) bool {
	return SelectHash(item) != ""
}

// SelectHash returns the best available content hash from the item, preferring
// QuickXorHash (most common on all account types), falling back to SHA256Hash,
// then SHA1Hash. Returns empty string if no hash is available — the caller must
// handle hash-less items appropriately (typically skipping verification) (B-021).
func SelectHash(item *graph.Item) string {
	if item.QuickXorHash != "" {
		return item.QuickXorHash
	}

	if item.SHA256Hash != "" {
		return item.SHA256Hash
	}

	return item.SHA1Hash
}

// ComputeQuickXorHash computes the QuickXorHash of a file and returns the
// base64-encoded digest. Uses streaming I/O (constant memory).
func ComputeQuickXorHash(fsPath string) (string, error) {
	f, err := os.Open(fsPath) //nolint:gosec // Hashing operates on the caller-selected local filesystem path.
	if err != nil {
		return "", fmt.Errorf("opening %s for hashing: %w", fsPath, err)
	}
	defer f.Close()

	h := quickxorhash.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing %s: %w", fsPath, err)
	}

	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}
