package quickxorhash

import (
	"bytes"
	"encoding/base64"
	"hash"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodeBase64 decodes a base64 string or panics.
func decodeBase64(t *testing.T, s string) []byte {
	t.Helper()

	b, err := base64.StdEncoding.DecodeString(s)
	require.NoError(t, err, "failed to decode base64 %q", s)

	return b
}

// Reference hashes verified against rclone v1.73.1's quickxorhash implementation.
func TestKnownVectors(t *testing.T) {
	tests := []struct {
		name   string
		input  []byte
		expect string // base64-encoded expected hash
	}{
		{
			name:   "empty string",
			input:  []byte(""),
			expect: "AAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		},
		{
			name:   "hello",
			input:  []byte("hello"),
			expect: "aCgDG9jwBgAAAAAABQAAAAAAAAA=",
		},
		{
			name:   "hello world",
			input:  []byte("hello world"),
			expect: "aCgDG9jwBhDc4Q1yawMZAAAAAAA=",
		},
		{
			name:   "1000 zero bytes",
			input:  make([]byte, 1000),
			expect: "AAAAAAAAAAAAAAAA6AMAAAAAAAA=",
		},
		{
			name:   "1000 0xFF bytes",
			input:  bytes.Repeat([]byte{0xFF}, 1000),
			expect: "Yxvb2MY2trGNbWxj89jYOc5xjnM=",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := New()
			_, err := h.Write(tc.input)
			require.NoError(t, err, "Write error")

			got := h.Sum(nil)
			want := decodeBase64(t, tc.expect)
			assert.Equal(t, want, got, "hash mismatch")
		})
	}
}

func TestIncrementalWrite(t *testing.T) {
	// Build a non-trivial 1024-byte input with a repeating pattern.
	input := make([]byte, 1024)
	for i := range input {
		input[i] = byte(i)
	}

	// Reference: hash in one shot.
	h1 := New()
	_, _ = h1.Write(input)
	oneShot := h1.Sum(nil)

	// Verify against rclone reference.
	want := decodeBase64(t, "h7xr2dbCayZCQYR9KKhlwDuT4UI=")
	require.Equal(t, want, oneShot, "one-shot hash mismatch")

	// Hash in varying chunk sizes: 1, 7, 64, 13, 128, then the rest.
	chunkSizes := []int{1, 7, 64, 13, 128}
	h2 := New()
	offset := 0

	for _, sz := range chunkSizes {
		end := offset + sz
		if end > len(input) {
			end = len(input)
		}
		_, _ = h2.Write(input[offset:end])
		offset = end
	}

	if offset < len(input) {
		_, _ = h2.Write(input[offset:])
	}

	incremental := h2.Sum(nil)

	assert.Equal(t, oneShot, incremental, "incremental write mismatch")
}

func TestSingleByteWrites(t *testing.T) {
	input := []byte("hello world")

	// One-shot write.
	h1 := New()
	_, _ = h1.Write(input)
	oneShot := h1.Sum(nil)

	// One byte at a time.
	h2 := New()
	for _, b := range input {
		_, _ = h2.Write([]byte{b})
	}
	byteAtATime := h2.Sum(nil)

	assert.Equal(t, oneShot, byteAtATime, "single-byte writes mismatch")
}

func TestReset(t *testing.T) {
	h := New()

	// Hash "hello".
	_, _ = h.Write([]byte("hello"))
	helloHash := h.Sum(nil)

	want := decodeBase64(t, "aCgDG9jwBgAAAAAABQAAAAAAAAA=")
	require.Equal(t, want, helloHash, "first hash wrong")

	// Reset and hash "world".
	h.Reset()
	_, _ = h.Write([]byte("world"))
	worldHash := h.Sum(nil)

	// Verify "world" hash is not the same as "hello" hash.
	assert.NotEqual(t, helloHash, worldHash, "after Reset, hash of 'world' equals hash of 'hello'")

	// Verify "world" hash matches a fresh computation.
	h2 := New()
	_, _ = h2.Write([]byte("world"))
	freshWorld := h2.Sum(nil)
	assert.Equal(t, freshWorld, worldHash, "reset hash mismatch")
}

func TestSumIsNonDestructive(t *testing.T) {
	h := New()
	_, _ = h.Write([]byte("hello"))

	sum1 := h.Sum(nil)
	sum2 := h.Sum(nil)

	assert.Equal(t, sum1, sum2, "calling Sum twice gave different results")
}

func TestSumAppendsToSlice(t *testing.T) {
	h := New()
	_, _ = h.Write([]byte("hello"))

	prefix := []byte("PREFIX")
	result := h.Sum(prefix)

	assert.Equal(t, prefix, result[:len(prefix)], "Sum did not preserve the prefix")
	assert.Len(t, result, len(prefix)+Size)
}

func TestWriteAfterSum(t *testing.T) {
	// Verify that calling Sum does not affect subsequent Write calls.
	h := New()
	_, _ = h.Write([]byte("hello"))
	_ = h.Sum(nil) // should not mutate state
	_, _ = h.Write([]byte(" world"))
	got := h.Sum(nil)

	want := decodeBase64(t, "aCgDG9jwBhDc4Q1yawMZAAAAAAA=")
	assert.Equal(t, want, got, "Write after Sum produced wrong hash")
}

// Compile-time assertion that *digest satisfies hash.Hash.
var _ hash.Hash = (*digest)(nil)

func TestInterfaceCompliance(t *testing.T) {
	h := New()

	assert.Equal(t, Size, h.Size())
	assert.Equal(t, BlockSize, h.BlockSize())
}

func BenchmarkQuickXorHash(b *testing.B) {
	const oneMB = 1024 * 1024
	data := make([]byte, oneMB)

	for i := range data {
		data[i] = byte(i)
	}

	b.SetBytes(oneMB)
	b.ResetTimer()

	for b.Loop() {
		h := New()
		_, _ = h.Write(data)
		h.Sum(nil)
	}
}
