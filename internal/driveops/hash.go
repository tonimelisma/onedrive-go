package driveops

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// hashReadChunkSize is how much a hash reads between two cancellation checks.
// One check per MiB costs nothing next to the read itself, and a canceled hash
// of a multi-gigabyte file stops within one chunk instead of at the end of the
// file (R-6.3.6).
const hashReadChunkSize = 1 << 20

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

// ComputeQuickXorHashContext computes the QuickXorHash of a file and returns
// the base64-encoded digest. It streams in constant memory and returns
// ctx.Err() once ctx is canceled, checked between chunks, so a shutdown or a
// canceled upload does not wait for a whole-file read to finish. Every caller
// that hashes on a worker, on the watch event loop or in a scan uses this
// form.
func ComputeQuickXorHashContext(ctx context.Context, fsPath string) (string, error) {
	f, err := localpath.OpenRegular(fsPath)
	if err != nil {
		return "", fmt.Errorf("opening %s for hashing: %w", fsPath, err)
	}
	defer f.Close() //nolint:errcheck // the file is opened read-only for hashing; there is nothing buffered to flush

	hash, err := ComputeQuickXorHashFromReaderContext(ctx, f)
	if err != nil {
		return "", fmt.Errorf("hashing %s: %w", fsPath, err)
	}

	return hash, nil
}

// ComputeQuickXorHash is ComputeQuickXorHashContext without cancellation. It
// remains for the callers whose own signatures carry no context: the
// executor's live precondition checks (internal/sync/executor_preconditions.go)
// and syncverify's hasher seam. Code that has a context uses the Context form.
func ComputeQuickXorHash(fsPath string) (string, error) {
	return ComputeQuickXorHashContext(context.Background(), fsPath)
}

// ComputeQuickXorHashFromReaderContext hashes everything r yields, checking
// ctx before each chunk of hashReadChunkSize. Callers that must act on
// exactly the bytes they verified hash an open descriptor with it instead of
// re-opening a path.
func ComputeQuickXorHashFromReaderContext(ctx context.Context, r io.Reader) (string, error) {
	h := quickxorhash.New()
	for {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("reading content to hash: %w", err)
		}
		if _, err := io.CopyN(h, r, hashReadChunkSize); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return "", fmt.Errorf("reading content to hash: %w", err)
		}
	}

	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// ComputeQuickXorHashFromReader is ComputeQuickXorHashFromReaderContext
// without cancellation, for the executor's descriptor-hashing seam, whose
// destructive-action checks carry no context.
func ComputeQuickXorHashFromReader(r io.Reader) (string, error) {
	return ComputeQuickXorHashFromReaderContext(context.Background(), r)
}
