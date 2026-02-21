package sync

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseBandwidthRate_Valid(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"0", 0},
		{"", 0},
		{"5MB/s", 5_000_000},
		{"100KB/s", 100_000},
		{"1GB/s", 1_000_000_000},
		{"10MiB/s", 10_485_760},
		// Without /s suffix: treated as raw size (bytes/sec implied).
		{"1024", 1024},
		{"5MB", 5_000_000},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseBandwidthRate(tc.input)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseBandwidthRate_Invalid(t *testing.T) {
	tests := []string{
		"abc",
		"-1MB/s",
		"not-a-number/s",
	}

	for _, tc := range tests {
		t.Run(tc, func(t *testing.T) {
			_, err := parseBandwidthRate(tc)
			assert.Error(t, err)
		})
	}
}

func TestNewBandwidthLimiter_Unlimited(t *testing.T) {
	bl, err := NewBandwidthLimiter("0", testLogger(t))
	require.NoError(t, err)
	assert.Nil(t, bl, "zero limit should return nil (unlimited)")
}

func TestNewBandwidthLimiter_Empty(t *testing.T) {
	bl, err := NewBandwidthLimiter("", testLogger(t))
	require.NoError(t, err)
	assert.Nil(t, bl)
}

func TestNewBandwidthLimiter_Static(t *testing.T) {
	bl, err := NewBandwidthLimiter("1MB/s", testLogger(t))
	require.NoError(t, err)
	require.NotNil(t, bl)
	assert.NotNil(t, bl.limiter)
}

func TestNewBandwidthLimiter_Invalid(t *testing.T) {
	_, err := NewBandwidthLimiter("garbage", testLogger(t))
	assert.Error(t, err)
}

func TestRateLimitedReader_Throttles(t *testing.T) {
	// 1 KB/s with burst=2KB. Read 4KB total so we exceed the initial burst
	// and must wait ~2 seconds. We check for at least 500ms (conservative).
	bl, err := NewBandwidthLimiter("1KB/s", testLogger(t))
	require.NoError(t, err)
	require.NotNil(t, bl)

	data := make([]byte, 4000)
	reader := bl.WrapReader(context.Background(), bytes.NewReader(data))

	start := time.Now()
	buf := make([]byte, 1024)

	var total int

	for total < len(data) {
		n, readErr := reader.Read(buf)
		total += n

		if readErr == io.EOF {
			break
		}

		require.NoError(t, readErr)
	}

	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 500*time.Millisecond, "rate-limited read should be throttled")
}

func TestRateLimitedWriter_Throttles(t *testing.T) {
	// 1 KB/s with burst=2KB. Write 4KB total so we exceed the initial burst.
	bl, err := NewBandwidthLimiter("1KB/s", testLogger(t))
	require.NoError(t, err)
	require.NotNil(t, bl)

	var buf bytes.Buffer
	writer := bl.WrapWriter(context.Background(), &buf)

	chunk := make([]byte, 1024)
	start := time.Now()

	for i := range 4 {
		n, writeErr := writer.Write(chunk)
		require.NoError(t, writeErr, "chunk %d", i)
		assert.Equal(t, len(chunk), n)
	}

	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 500*time.Millisecond, "rate-limited write should be throttled")
}

func TestRateLimitedReader_ContextCancel(t *testing.T) {
	// Very low rate so limiter blocks quickly after the initial burst.
	bl, err := NewBandwidthLimiter("1KB/s", testLogger(t))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	// Large data source so reads don't EOF before context is canceled.
	data := strings.NewReader(strings.Repeat("x", 100000))
	reader := bl.WrapReader(ctx, data)

	// Cancel the context after a short delay.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	// Small buffer so each Read is within the burst size.
	buf := make([]byte, 512)

	var readErr error

	for {
		_, readErr = reader.Read(buf)
		if readErr != nil {
			break
		}
	}

	// Should get context canceled error (not just EOF).
	assert.ErrorIs(t, readErr, context.Canceled)
}

func TestWrapReader_NilLimiter(t *testing.T) {
	r := strings.NewReader("hello")
	got := wrapReader(nil, context.Background(), r)
	assert.Equal(t, r, got, "nil limiter should return original reader")
}

func TestWrapWriter_NilLimiter(t *testing.T) {
	var buf bytes.Buffer
	got := wrapWriter(nil, context.Background(), &buf)
	assert.Equal(t, &buf, got, "nil limiter should return original writer")
}

func TestBandwidthLimiter_WrapReader_NilReceiver(t *testing.T) {
	r := strings.NewReader("data")
	var bl *BandwidthLimiter

	got := bl.WrapReader(context.Background(), r)
	assert.Equal(t, r, got, "nil BandwidthLimiter should return original reader")
}

func TestBandwidthLimiter_WrapWriter_NilReceiver(t *testing.T) {
	var buf bytes.Buffer
	var bl *BandwidthLimiter

	got := bl.WrapWriter(context.Background(), &buf)
	assert.Equal(t, &buf, got, "nil BandwidthLimiter should return original writer")
}

func TestWrapReader_NonNilLimiter(t *testing.T) {
	// Verify that wrapReader with a non-nil limiter returns a wrapped reader
	// that produces the correct data (high rate so no throttling delay).
	bl, err := NewBandwidthLimiter("100MB/s", testLogger(t))
	require.NoError(t, err)
	require.NotNil(t, bl)

	input := "hello bandwidth"
	r := strings.NewReader(input)
	wrapped := wrapReader(bl, context.Background(), r)

	// The wrapped reader should not be the original reader.
	assert.NotEqual(t, r, wrapped, "non-nil limiter should wrap the reader")

	// Reading through the wrapped reader should return correct data.
	data, err := io.ReadAll(wrapped)
	require.NoError(t, err)
	assert.Equal(t, input, string(data))
}

func TestWrapWriter_NonNilLimiter(t *testing.T) {
	// Verify that wrapWriter with a non-nil limiter returns a wrapped writer
	// that writes the correct data (high rate so no throttling delay).
	bl, err := NewBandwidthLimiter("100MB/s", testLogger(t))
	require.NoError(t, err)
	require.NotNil(t, bl)

	var buf bytes.Buffer
	wrapped := wrapWriter(bl, context.Background(), &buf)

	// The wrapped writer should not be the original writer.
	assert.NotEqual(t, &buf, wrapped, "non-nil limiter should wrap the writer")

	// Writing through the wrapped writer should produce correct output.
	input := []byte("hello bandwidth writer")
	n, err := wrapped.Write(input)
	require.NoError(t, err)
	assert.Equal(t, len(input), n)
	assert.Equal(t, input, buf.Bytes())
}
