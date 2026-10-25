package cli

import (
	"context"
	"log/slog"
	"sync"
)

// bootstrapLogBuffer holds the records logged while the CLI is still finding
// its config file -- before it knows the log file, its level, or log_format.
// replayInto hands them to the command logger once that exists, so they follow
// the same console rules as every later record and reach the log file as
// well. Afterwards it forwards records straight through.
//
// It is meant for that handful of startup records only: it buffers without a
// level check, and WithGroup flattens (the config bootstrap does not group).
type bootstrapLogBuffer struct {
	state *bootstrapLogState
	attrs []slog.Attr
}

type bootstrapLogState struct {
	mu      sync.Mutex
	records []slog.Record
	target  slog.Handler
}

func newBootstrapLogBuffer() *bootstrapLogBuffer {
	return &bootstrapLogBuffer{state: &bootstrapLogState{}}
}

func (b *bootstrapLogBuffer) Enabled(ctx context.Context, level slog.Level) bool {
	b.state.mu.Lock()
	target := b.state.target
	b.state.mu.Unlock()

	if target != nil {
		return target.Enabled(ctx, level)
	}

	return true
}

//nolint:gocritic // slog.Handler interface requires value receiver for Record
func (b *bootstrapLogBuffer) Handle(ctx context.Context, r slog.Record) error {
	r = r.Clone()
	r.AddAttrs(b.attrs...)

	b.state.mu.Lock()
	target := b.state.target
	if target == nil {
		b.state.records = append(b.state.records, r)
	}
	b.state.mu.Unlock()

	if target == nil {
		return nil
	}

	return handleIfEnabled(ctx, target, r)
}

func (b *bootstrapLogBuffer) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *b
	next.attrs = append(append([]slog.Attr(nil), b.attrs...), attrs...)

	return &next
}

func (b *bootstrapLogBuffer) WithGroup(string) slog.Handler { return b }

// replayInto sends the buffered records to target, each only if target takes
// its level, and forwards every later record there directly.
func (b *bootstrapLogBuffer) replayInto(ctx context.Context, target slog.Handler) {
	b.state.mu.Lock()
	records := b.state.records
	b.state.records = nil
	b.state.target = target
	b.state.mu.Unlock()

	for i := range records {
		// A console or file write failure has nowhere better to go than the
		// handler that just failed; later records will meet the same writer.
		_ = handleIfEnabled(ctx, target, records[i]) //nolint:errcheck // see above
	}
}

//nolint:gocritic // slog.Record is passed by value throughout log/slog
func handleIfEnabled(ctx context.Context, target slog.Handler, r slog.Record) error {
	if !target.Enabled(ctx, r.Level) {
		return nil
	}

	return target.Handle(ctx, r) //nolint:wrapcheck // the handler's own error is the log write failure
}
