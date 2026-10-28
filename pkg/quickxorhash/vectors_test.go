package quickxorhash

import (
	"encoding/base64"
	"encoding/binary"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// naiveQuickXor is QuickXorHash written straight from Microsoft's
// description, one bit at a time: byte i is XORed into the 160-bit ring at
// bit (11*i) mod 160, and the input length, little-endian, is XORed into the
// last eight bytes of the digest. It shares no code with the optimized
// implementation, so agreement between the two is evidence for both.
func naiveQuickXor(data []byte) []byte {
	var ring [widthInBits]byte // one bit per element
	for i, b := range data {
		start := (i * shift) % widthInBits
		for bit := range bitsPerByte {
			ring[(start+bit)%widthInBits] ^= (b >> bit) & 1
		}
	}

	digest := make([]byte, Size)
	for pos, bit := range ring {
		digest[pos/bitsPerByte] |= bit << (pos % bitsPerByte)
	}
	var length [8]byte
	binary.LittleEndian.PutUint64(length[:], uint64(len(data)))
	for i := range length {
		digest[Size-len(length)+i] ^= length[i]
	}

	return digest
}

// vectorInput is the deterministic input of the given length: the seeded
// stream every vector below was computed from.
func vectorInput(length int) []byte {
	rng := rand.New(rand.NewPCG(uint64(length), 0x51ed27a3)) //nolint:gosec // deterministic test data
	data := make([]byte, length)
	for i := range data {
		data[i] = byte(rng.Uint32())
	}

	return data
}

// Validates: R-5.5
//
// The reference agrees with the vectors verified against rclone, so it can
// stand for them at the lengths below.
func TestNaiveReferenceMatchesKnownVectors(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]string{
		"":            "AAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"hello":       "aCgDG9jwBgAAAAAABQAAAAAAAAA=",
		"hello world": "aCgDG9jwBhDc4Q1yawMZAAAAAAA=",
	} {
		assert.Equal(t, want, base64.StdEncoding.EncodeToString(naiveQuickXor([]byte(input))), "%q", input)
	}
}

// Validates: R-5.5
//
// Lengths where the optimized implementation changes paths (ABR-035,
// MS-059): around the 20-byte digest, the 160-byte ring wrap, 64 KiB reads,
// and a megabyte plus an odd tail, written whole and in odd pieces. Each is
// pinned to a digest and checked against the bitwise reference.
func TestBoundaryLengthVectors(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		length int
		want   string
	}{
		{length: 0, want: "AAAAAAAAAAAAAAAAAAAAAAAAAAA="},
		{length: 20, want: vectorDigest20},
		{length: 21, want: vectorDigest21},
		{length: 159, want: vectorDigest159},
		{length: 160, want: vectorDigest160},
		{length: 161, want: vectorDigest161},
		{length: 64*1024 - 1, want: vectorDigest64KMinus1},
		{length: 64 * 1024, want: vectorDigest64K},
		{length: 64*1024 + 1, want: vectorDigest64KPlus1},
		{length: 1024*1024 + 7, want: vectorDigest1MPlus7},
	} {
		data := vectorInput(tc.length)
		reference := base64.StdEncoding.EncodeToString(naiveQuickXor(data))
		require.Equal(t, tc.want, reference, "length %d: pinned vector disagrees with the reference", tc.length)

		whole := New()
		_, err := whole.Write(data)
		require.NoError(t, err)
		assert.Equal(t, tc.want, base64.StdEncoding.EncodeToString(whole.Sum(nil)), "length %d written whole", tc.length)

		pieces := New()
		for start := 0; start < len(data); {
			end := min(start+1+start%997, len(data))
			_, err := pieces.Write(data[start:end])
			require.NoError(t, err)
			start = end
		}
		assert.Equal(t, tc.want, base64.StdEncoding.EncodeToString(pieces.Sum(nil)), "length %d written in pieces", tc.length)
	}
}

// The digests of vectorInput(n), computed by naiveQuickXor.
const (
	vectorDigest20        = "OPlg+uZ5Pzs8gwu4PAIz4JEIJJQ="
	vectorDigest21        = "2eyeUPuCqY/YAxvRBQYudpCBAOQ="
	vectorDigest159       = "l/mg1L9I56sZLt23aIZN9UWW2Gw="
	vectorDigest160       = "CaFqnsVBSeQMovDLjl2+F2nJ1qg="
	vectorDigest161       = "sPfx2GFZWYbEgAX8bST49YgIrao="
	vectorDigest64KMinus1 = "9KBz4jT2nVVX+vlwVE8eCcncZ+M="
	vectorDigest64K       = "qruQbLyT7gMpfkSInrW6k/Lju8o="
	vectorDigest64KPlus1  = "eAgsuVDs0oiX1euudfrTdZgvlcg="
	vectorDigest1MPlus7   = "N5jpTAA7PZJJebWN7ciB25vLl8M="
)
