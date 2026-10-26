package driveops

import (
	"encoding/base64"
	"fmt"
	"io"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// HasHash reports whether the item has a content hash the client can compare.
// Items without one (some SharePoint files, SHA-only items) cannot be
// hash-verified.
func HasHash(item *graph.Item) bool {
	return SelectHash(item) != ""
}

// SelectHash returns the item's content hash in the only algorithm the local
// side computes: QuickXorHash. It used to fall back to SHA-256 and SHA-1,
// which can never equal a local QuickXor digest, so such an item failed every
// download verification and never compared equal (D-041); now it is hashless
// and its size is the evidence. SHA digests stay on the item for diagnostics.
func SelectHash(item *graph.Item) string {
	if item == nil {
		return ""
	}

	return item.QuickXorHash
}

// ComputeQuickXorHash computes the QuickXorHash of a file and returns the
// base64-encoded digest. Uses streaming I/O (constant memory).
func ComputeQuickXorHash(fsPath string) (string, error) {
	f, err := localpath.OpenRegular(fsPath)
	if err != nil {
		return "", fmt.Errorf("opening %s for hashing: %w", fsPath, err)
	}
	defer f.Close() //nolint:errcheck // the file is opened read-only for hashing; there is nothing buffered to flush

	h := quickxorhash.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing %s: %w", fsPath, err)
	}

	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}
