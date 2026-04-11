package perf

import "context"

type collectorContextKey struct{}

func WithCollector(ctx context.Context, collector *Collector) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if collector == nil {
		return ctx
	}

	return context.WithValue(ctx, collectorContextKey{}, collector)
}

func FromContext(ctx context.Context) *Collector {
	if ctx == nil {
		return nil
	}

	value := ctx.Value(collectorContextKey{})
	collector, ok := value.(*Collector)
	if !ok {
		return nil
	}

	return collector
}
