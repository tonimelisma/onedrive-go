package driveops

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// hashContent computes the QuickXorHash of a string, returning the
// base64-encoded digest. Matches the output of ComputeQuickXorHash for
// the same content written to a file.
func hashContent(t *testing.T, content string) string {
	t.Helper()

	h := quickxorhash.New()
	_, err := h.Write([]byte(content))
	require.NoError(t, err, "hash.Write")

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func TestComputeQuickXorHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "hello world"
	path := filepath.Join(dir, "test.txt")

	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	hash, err := ComputeQuickXorHash(path)
	require.NoError(t, err, "ComputeQuickXorHash")

	want := hashContent(t, content)
	assert.Equal(t, want, hash)
}

func TestComputeQuickXorHash_EmptyFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")

	require.NoError(t, os.WriteFile(path, []byte(""), 0o600))

	hash, err := ComputeQuickXorHash(path)
	require.NoError(t, err, "ComputeQuickXorHash")

	want := hashContent(t, "")
	assert.Equal(t, want, hash)
	assert.NotEmpty(t, hash, "empty file hash should not be empty string")
}

func TestComputeQuickXorHash_NonexistentFile(t *testing.T) {
	t.Parallel()

	_, err := ComputeQuickXorHash("/nonexistent/path/file.txt")
	require.Error(t, err)
}

// Validates: R-5.5.1
func TestHasHash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		item *graph.Item
		want bool
	}{
		{"quickxor only", &graph.Item{QuickXorHash: "abc123"}, true},
		// SHA digests are never computed locally, so they can prove nothing.
		{"sha256 only", &graph.Item{SHA256Hash: "def456"}, false},
		{"sha1 only", &graph.Item{SHA1Hash: "ghi789"}, false},
		{"no hashes", &graph.Item{}, false},
		{"all empty strings", &graph.Item{QuickXorHash: "", SHA256Hash: "", SHA1Hash: ""}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := HasHash(tt.item)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Validates: R-5.5.1
func TestSelectHash_ReturnsOnlyQuickXor(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "qx", SelectHash(&graph.Item{QuickXorHash: "qx", SHA256Hash: "s256", SHA1Hash: "s1"}))
	assert.Empty(t, SelectHash(&graph.Item{SHA256Hash: "s256"}), "SHA-256 is not comparable with local QuickXor")
	assert.Empty(t, SelectHash(&graph.Item{SHA1Hash: "s1"}), "SHA-1 is not comparable with local QuickXor")
	assert.Empty(t, SelectHash(nil))
}

// endlessReader yields data forever and reports its first Read, so a test can
// cancel a hash it knows is in progress.
type endlessReader struct {
	firstRead chan struct{}
	once      sync.Once
}

func (r *endlessReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.firstRead) })
	for i := range p {
		p[i] = 'x'
	}

	return len(p), nil
}

// Validates: R-6.3.6
//
// A hash reads between cancellation checks in fixed chunks, so a canceled
// context stops it within one chunk rather than at the end of the input --
// here an input that has no end.
func TestComputeQuickXorHashFromReaderContext_StopsAtCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	reader := &endlessReader{firstRead: make(chan struct{})}

	type hashResult struct {
		hash string
		err  error
	}
	done := make(chan hashResult, 1)
	go func() {
		hash, err := ComputeQuickXorHashFromReaderContext(ctx, reader)
		done <- hashResult{hash: hash, err: err}
	}()

	select {
	case <-reader.firstRead:
	case <-time.After(10 * time.Second):
		require.Fail(t, "the hash never started reading")
	}
	cancel()

	select {
	case result := <-done:
		require.ErrorIs(t, result.err, context.Canceled)
		assert.Empty(t, result.hash)
	case <-time.After(10 * time.Second):
		require.Fail(t, "a canceled hash must stop within one chunk")
	}
}

// Validates: R-6.3.6
func TestComputeQuickXorHashContext_CanceledContextDoesNotHashTheFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "big.bin")
	require.NoError(t, os.WriteFile(path, []byte("content"), 0o600))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := ComputeQuickXorHashContext(ctx, path)
	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), "hashing "+path)
}

// Validates: R-5.5.1
//
// The chunked read must produce the same digest as a single pass, across
// chunk boundaries and for a final partial chunk.
func TestComputeQuickXorHashContext_ChunkedReadMatchesSinglePass(t *testing.T) {
	t.Parallel()

	content := strings.Repeat("0123456789abcdef", (2*hashReadChunkSize+4096)/16)
	path := filepath.Join(t.TempDir(), "multi-chunk.bin")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	hash, err := ComputeQuickXorHashContext(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, hashContent(t, content), hash)

	legacy, err := ComputeQuickXorHash(path)
	require.NoError(t, err)
	assert.Equal(t, hash, legacy)
}
