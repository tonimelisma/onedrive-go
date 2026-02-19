package quickxorhash

import (
	"bytes"
	"encoding/base64"
	"hash"
	"testing"
)

// decodeBase64 decodes a base64 string or panics.
func decodeBase64(t *testing.T, s string) []byte {
	t.Helper()

	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("failed to decode base64 %q: %v", s, err)
	}

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
			if err != nil {
				t.Fatalf("Write error: %v", err)
			}

			got := h.Sum(nil)
			want := decodeBase64(t, tc.expect)

			if !bytes.Equal(got, want) {
				t.Errorf("hash mismatch\n  got:  %s\n  want: %s",
					base64.StdEncoding.EncodeToString(got), tc.expect)
			}
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
	if !bytes.Equal(oneShot, want) {
		t.Fatalf("one-shot hash mismatch\n  got:  %s\n  want: h7xr2dbCayZCQYR9KKhlwDuT4UI=",
			base64.StdEncoding.EncodeToString(oneShot))
	}

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

	if !bytes.Equal(oneShot, incremental) {
		t.Errorf("incremental write mismatch\n  one-shot:    %x\n  incremental: %x",
			oneShot, incremental)
	}
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

	if !bytes.Equal(oneShot, byteAtATime) {
		t.Errorf("single-byte writes mismatch\n  one-shot:       %x\n  byte-at-a-time: %x",
			oneShot, byteAtATime)
	}
}

func TestReset(t *testing.T) {
	h := New()

	// Hash "hello".
	_, _ = h.Write([]byte("hello"))
	helloHash := h.Sum(nil)

	want := decodeBase64(t, "aCgDG9jwBgAAAAAABQAAAAAAAAA=")
	if !bytes.Equal(helloHash, want) {
		t.Fatalf("first hash wrong:\n  got:  %x\n  want: %x", helloHash, want)
	}

	// Reset and hash "world".
	h.Reset()
	_, _ = h.Write([]byte("world"))
	worldHash := h.Sum(nil)

	// Verify "world" hash is not the same as "hello" hash.
	if bytes.Equal(worldHash, helloHash) {
		t.Error("after Reset, hash of 'world' equals hash of 'hello'")
	}

	// Verify "world" hash matches a fresh computation.
	h2 := New()
	_, _ = h2.Write([]byte("world"))
	freshWorld := h2.Sum(nil)

	if !bytes.Equal(worldHash, freshWorld) {
		t.Errorf("reset hash mismatch\n  after-reset: %x\n  fresh:       %x",
			worldHash, freshWorld)
	}
}

func TestSumIsNonDestructive(t *testing.T) {
	h := New()
	_, _ = h.Write([]byte("hello"))

	sum1 := h.Sum(nil)
	sum2 := h.Sum(nil)

	if !bytes.Equal(sum1, sum2) {
		t.Errorf("calling Sum twice gave different results\n  first:  %x\n  second: %x",
			sum1, sum2)
	}
}

func TestSumAppendsToSlice(t *testing.T) {
	h := New()
	_, _ = h.Write([]byte("hello"))

	prefix := []byte("PREFIX")
	result := h.Sum(prefix)

	if !bytes.Equal(result[:len(prefix)], prefix) {
		t.Error("Sum did not preserve the prefix")
	}

	if len(result) != len(prefix)+Size {
		t.Errorf("expected result length %d, got %d", len(prefix)+Size, len(result))
	}
}

func TestWriteAfterSum(t *testing.T) {
	// Verify that calling Sum does not affect subsequent Write calls.
	h := New()
	_, _ = h.Write([]byte("hello"))
	_ = h.Sum(nil) // should not mutate state
	_, _ = h.Write([]byte(" world"))
	got := h.Sum(nil)

	want := decodeBase64(t, "aCgDG9jwBhDc4Q1yawMZAAAAAAA=")
	if !bytes.Equal(got, want) {
		t.Errorf("Write after Sum produced wrong hash\n  got:  %s\n  want: aCgDG9jwBhDc4Q1yawMZAAAAAAA=",
			base64.StdEncoding.EncodeToString(got))
	}
}

// Compile-time assertion that *digest satisfies hash.Hash.
var _ hash.Hash = (*digest)(nil)

func TestInterfaceCompliance(t *testing.T) {
	h := New()

	if h.Size() != Size {
		t.Errorf("Size() = %d, want %d", h.Size(), Size)
	}

	if h.BlockSize() != BlockSize {
		t.Errorf("BlockSize() = %d, want %d", h.BlockSize(), BlockSize)
	}
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
