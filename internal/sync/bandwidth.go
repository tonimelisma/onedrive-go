package sync

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"golang.org/x/time/rate"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

// burstMultiplier controls the token bucket burst size relative to the per-second rate.
// A 2x burst allows short savings to be spent on the next read/write without
// reducing sustained throughput below the configured limit.
const burstMultiplier = 2

// BandwidthLimiter provides shared rate limiting across all transfer workers.
// A single limiter is shared by all concurrent downloads and uploads, ensuring
// aggregate throughput stays within the configured bandwidth_limit.
type BandwidthLimiter struct {
	limiter *rate.Limiter
	logger  *slog.Logger
}

// NewBandwidthLimiter creates a limiter from the bandwidth_limit config string.
// Returns nil if limit is "0" or empty (unlimited).
func NewBandwidthLimiter(bandwidthLimit string, logger *slog.Logger) (*BandwidthLimiter, error) {
	bytesPerSec, err := parseBandwidthRate(bandwidthLimit)
	if err != nil {
		return nil, fmt.Errorf("bandwidth: parse limit %q: %w", bandwidthLimit, err)
	}

	if bytesPerSec == 0 {
		return nil, nil //nolint:nilnil // nil limiter = unlimited; callers check with nil-safe wrappers
	}

	burst := int(bytesPerSec) * burstMultiplier
	limiter := rate.NewLimiter(rate.Limit(bytesPerSec), burst)

	logger.Info("bandwidth: limiter created",
		"bytes_per_sec", bytesPerSec,
		"burst", burst,
	)

	return &BandwidthLimiter{limiter: limiter, logger: logger}, nil
}

// parseBandwidthRate parses "5MB/s", "100KB/s", "0" → bytes/sec.
// Strips the "/s" suffix and delegates to config.ParseSize for the numeric+unit part.
func parseBandwidthRate(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}

	// Strip "/s" suffix (case-insensitive) to isolate the size component.
	normalized := s
	if strings.HasSuffix(strings.ToLower(normalized), "/s") {
		normalized = normalized[:len(normalized)-len("/s")]
	}

	bytes, err := config.ParseSize(normalized)
	if err != nil {
		return 0, fmt.Errorf("invalid bandwidth rate %q: %w", s, err)
	}

	if bytes < 0 {
		return 0, fmt.Errorf("invalid bandwidth rate %q: must be non-negative", s)
	}

	return bytes, nil
}

// WrapReader returns a rate-limited io.Reader. If bl is nil, returns r unchanged.
func (bl *BandwidthLimiter) WrapReader(ctx context.Context, r io.Reader) io.Reader {
	if bl == nil {
		return r
	}

	return &rateLimitedReader{r: r, limiter: bl.limiter, ctx: ctx}
}

// WrapWriter returns a rate-limited io.Writer. If bl is nil, returns w unchanged.
func (bl *BandwidthLimiter) WrapWriter(ctx context.Context, w io.Writer) io.Writer {
	if bl == nil {
		return w
	}

	return &rateLimitedWriter{w: w, limiter: bl.limiter, ctx: ctx}
}

// wrapReader is a package-level nil-safe helper to avoid nil-checks at every call site.
func wrapReader(bl *BandwidthLimiter, ctx context.Context, r io.Reader) io.Reader {
	if bl == nil {
		return r
	}

	return bl.WrapReader(ctx, r)
}

// wrapWriter is a package-level nil-safe helper to avoid nil-checks at every call site.
func wrapWriter(bl *BandwidthLimiter, ctx context.Context, w io.Writer) io.Writer {
	if bl == nil {
		return w
	}

	return bl.WrapWriter(ctx, w)
}

// rateLimitedReader wraps an io.Reader with token bucket rate limiting.
// After each successful read, it blocks until the limiter allows the bytes consumed.
type rateLimitedReader struct {
	r       io.Reader
	limiter *rate.Limiter
	ctx     context.Context
}

func (r *rateLimitedReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		if waitErr := waitN(r.limiter, r.ctx, n); waitErr != nil {
			return n, waitErr
		}
	}

	return n, err
}

// rateLimitedWriter wraps an io.Writer with token bucket rate limiting.
// After each successful write, it blocks until the limiter allows the bytes produced.
type rateLimitedWriter struct {
	w       io.Writer
	limiter *rate.Limiter
	ctx     context.Context
}

func (w *rateLimitedWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 {
		if waitErr := waitN(w.limiter, w.ctx, n); waitErr != nil {
			return n, waitErr
		}
	}

	return n, err
}

// waitN splits a large token request into burst-sized chunks.
// rate.Limiter.WaitN rejects requests exceeding the burst size, so we loop.
func waitN(limiter *rate.Limiter, ctx context.Context, n int) error {
	burst := limiter.Burst()

	for n > 0 {
		take := n
		if take > burst {
			take = burst
		}

		if err := limiter.WaitN(ctx, take); err != nil {
			return err
		}

		n -= take
	}

	return nil
}
