// Package quickxorhash implements the QuickXorHash algorithm used by
// Microsoft OneDrive for file content hashing.
//
// The algorithm XORs each input byte into a circular bit-shift buffer of
// 160 bits, advancing the insertion point by 11 bits per byte.  The final
// digest also mixes in the total byte count.
//
// Based on the rclone implementation (BSD-0 license).
// Original source: github.com/rclone/rclone/backend/onedrive/quickxorhash
// Copyright (c) rclone contributors.
//
// Reference C# implementation by Microsoft:
// https://learn.microsoft.com/en-us/onedrive/developer/code-snippets/quickxorhash
package quickxorhash

import (
	"encoding/binary"
	"hash"
)

const (
	// Size is the length, in bytes, of a QuickXorHash digest.
	Size = 20

	// BlockSize is the preferred input block size for the hash, in bytes.
	BlockSize = 64

	// shift is the number of bits the insertion point advances per byte.
	shift = 11

	// widthInBits is the total width of the circular XOR buffer, in bits.
	widthInBits = 160

	// bitsInLastCell is the number of valid bits in the last uint64 of the data array.
	// widthInBits - (dataLen-1)*64 = 160 - 2*64 = 32.
	bitsInLastCell = 32

	// bitsPerByte is the number of bits in one byte.
	bitsPerByte = 8

	// bitsPerUint64 is the number of bits in a single uint64 element.
	bitsPerUint64 = 64

	// dataLen is the number of uint64 elements needed to hold widthInBits bits.
	dataLen = 3 // (widthInBits-1)/bitsPerUint64 + 1
)

// digest is the internal state of a QuickXorHash computation.
type digest struct {
	data        [dataLen]uint64
	shiftSoFar  int
	lengthSoFar uint64
}

// New returns a new hash.Hash computing the QuickXorHash checksum.
func New() hash.Hash {
	return &digest{}
}

// bitsInCell returns the number of valid bits in the cell at the given index.
func bitsInCell(index int) int {
	if index == dataLen-1 {
		return bitsInLastCell
	}

	return bitsPerUint64
}

// Write absorbs more data into the running hash.
// It always returns len(p), nil.
func (d *digest) Write(p []byte) (int, error) {
	currentShift := d.shiftSoFar
	vectorArrayIndex := currentShift / bitsPerUint64
	vectorOffset := currentShift % bitsPerUint64
	iterations := min(len(p), widthInBits)

	for i := range iterations {
		cellBits := bitsInCell(vectorArrayIndex)

		if vectorOffset <= cellBits-bitsPerByte {
			// The byte fits entirely within this cell.
			for j := i; j < len(p); j += widthInBits {
				d.data[vectorArrayIndex] ^= uint64(p[j]) << vectorOffset
			}
		} else {
			// The byte straddles two cells; pre-XOR all bytes at this
			// shift position, then split across cells.
			isLastCell := vectorArrayIndex == dataLen-1
			nextIndex := vectorArrayIndex + 1
			if isLastCell {
				nextIndex = 0
			}

			low := byte(cellBits - vectorOffset)

			var xoredByte byte
			for j := i; j < len(p); j += widthInBits {
				xoredByte ^= p[j]
			}

			d.data[vectorArrayIndex] ^= uint64(xoredByte) << vectorOffset
			d.data[nextIndex] ^= uint64(xoredByte) >> low
		}

		vectorOffset += shift
		for vectorOffset >= bitsInCell(vectorArrayIndex) {
			vectorOffset -= bitsInCell(vectorArrayIndex)
			if vectorArrayIndex == dataLen-1 {
				vectorArrayIndex = 0
			} else {
				vectorArrayIndex++
			}
		}
	}

	d.shiftSoFar = (d.shiftSoFar + shift*(len(p)%widthInBits)) % widthInBits
	d.lengthSoFar += uint64(len(p))

	return len(p), nil
}

// Sum appends the current hash to b and returns the resulting slice.
// It does not change the underlying hash state.
func (d *digest) Sum(b []byte) []byte {
	// Copy state so that Sum is non-destructive.
	dup := *d

	// Serialize the data array into 20 bytes (little-endian).
	var rgb [Size]byte
	binary.LittleEndian.PutUint64(rgb[0:8], dup.data[0])
	binary.LittleEndian.PutUint64(rgb[8:16], dup.data[1])
	// data[2] only uses bitsInLastCell (32) bits, so truncation to uint32 is safe.
	lastCell := uint32(dup.data[2]) //nolint:gosec // truncation is intentional; see bitsInLastCell
	binary.LittleEndian.PutUint32(rgb[16:Size], lastCell)

	// XOR the file length (little-endian int64) into the last 8 bytes of rgb.
	var lengthBytes [8]byte
	binary.LittleEndian.PutUint64(lengthBytes[:], dup.lengthSoFar)

	lengthStart := Size - len(lengthBytes)
	for i, lb := range lengthBytes {
		rgb[lengthStart+i] ^= lb
	}

	return append(b, rgb[:]...)
}

// Reset resets the hash to its initial state.
func (d *digest) Reset() {
	*d = digest{}
}

// Size returns the number of bytes Sum will return.
func (d *digest) Size() int {
	return Size
}

// BlockSize returns the hash's underlying block size.
func (d *digest) BlockSize() int {
	return BlockSize
}
