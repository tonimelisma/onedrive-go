package perf

import "context"

type collectorContextKey struct{}

func WithCollector(ctx context.Context, collector *Collector) context.Context {
	if collector == nil {
		return ctx
	}

	return context.WithValue(ctx, collectorContextKey{}, collector)
}

func FromContext(ctx context.Context) *Collector {
	value := ctx.Value(collectorContextKey{})
	collector, ok := value.(*Collector)
	if !ok {
		return nil
	}

	return collector
}
