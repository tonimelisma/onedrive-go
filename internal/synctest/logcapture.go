package synctest

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// LogCapture is a slog.Handler that keeps every record it handles, so a test
// can assert on what was logged. WithAttrs and WithGroup return the capture
// itself: attributes added to a logger are not kept.
type LogCapture struct {
	// mu guards records.
	mu      sync.Mutex
	records []CapturedRecord
}

// CapturedRecord is a record a LogCapture kept: its level, its message, and
// its attributes, each value as its string.
type CapturedRecord struct {
	Level   slog.Level
	Message string
	Attrs   map[string]string
}

// NewLogCapture returns a capture and a logger that writes to it.
func NewLogCapture() (*LogCapture, *slog.Logger) {
	capture := &LogCapture{}
	return capture, slog.New(capture)
}

// Enabled takes every level.
func (c *LogCapture) Enabled(context.Context, slog.Level) bool { return true }

// Handle keeps record's level, message and attributes.
//
//nolint:gocritic // slog.Handler fixes the by-value Record parameter.
func (c *LogCapture) Handle(_ context.Context, record slog.Record) error {
	attrs := make(map[string]string, record.NumAttrs())
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.String()
		return true
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, CapturedRecord{Level: record.Level, Message: record.Message, Attrs: attrs})

	return nil
}

// WithAttrs returns the capture.
func (c *LogCapture) WithAttrs([]slog.Attr) slog.Handler { return c }

// WithGroup returns the capture.
func (c *LogCapture) WithGroup(string) slog.Handler { return c }

// Records are the records handled so far, in order.
func (c *LogCapture) Records() []CapturedRecord {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]CapturedRecord(nil), c.records...)
}

// AtLeast are the records at level or above, in order: what a console at that
// level would show.
func (c *LogCapture) AtLeast(level slog.Level) []CapturedRecord {
	var out []CapturedRecord
	for _, record := range c.Records() {
		if record.Level >= level {
			out = append(out, record)
		}
	}

	return out
}

// Contains reports whether a record's message contains text.
func (c *LogCapture) Contains(text string) bool {
	for _, record := range c.Records() {
		if strings.Contains(record.Message, text) {
			return true
		}
	}

	return false
}

// PanicOnMessage is a slog.Handler that panics on the record whose message is
// Message, as a bug in a log call would, and hands every other record to
// Next.
type PanicOnMessage struct {
	Message string
	Next    slog.Handler
}

// Enabled takes every level, so the panicking record always reaches Handle.
func (h PanicOnMessage) Enabled(context.Context, slog.Level) bool { return true }

// Handle panics on Message and passes every other record Next takes to Next.
//
//nolint:gocritic // slog.Handler fixes the by-value Record parameter.
func (h PanicOnMessage) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == h.Message {
		panic("log call exploded")
	}
	if !h.Next.Enabled(ctx, record.Level) {
		return nil
	}
	if err := h.Next.Handle(ctx, record); err != nil {
		return fmt.Errorf("handle test record: %w", err)
	}

	return nil
}

// WithAttrs keeps panicking on Message.
func (h PanicOnMessage) WithAttrs(attrs []slog.Attr) slog.Handler {
	return PanicOnMessage{Message: h.Message, Next: h.Next.WithAttrs(attrs)}
}

// WithGroup keeps panicking on Message.
func (h PanicOnMessage) WithGroup(name string) slog.Handler {
	return PanicOnMessage{Message: h.Message, Next: h.Next.WithGroup(name)}
}
