package cli

import (
	"context"
	"fmt"
)

// routeDurableIntent centralizes the CLI policy for mutating durable sync
// intent. Watch owners receive live RPCs; missing sockets, one-shot owners,
// and disappearing sockets fall back to direct DB intent writes. Typed daemon
// application errors are authoritative and must not fall back.
func routeDurableIntent[T any](
	ctx context.Context,
	direct func(context.Context) (T, error),
	watch func(context.Context, *controlSocketClient) (T, error),
) (T, error) {
	var zero T

	probe, err := probeControlOwner(ctx)
	if err != nil && probe.state == controlOwnerStateProbeFailed {
		return zero, fmt.Errorf("probe control owner: %w", err)
	}

	switch probe.state {
	case controlOwnerStateOneShotOwner, controlOwnerStateNoSocket, controlOwnerStatePathUnavailable:
		return direct(ctx)
	case controlOwnerStateWatchOwner:
		result, watchErr := watch(ctx, probe.client)
		if watchErr == nil {
			return result, nil
		}
		if isControlDaemonError(watchErr) {
			return zero, watchErr
		}
		if isControlSocketGone(watchErr) {
			return direct(ctx)
		}

		return zero, watchErr
	case controlOwnerStateProbeFailed:
		return zero, fmt.Errorf("probe control owner: %w", err)
	default:
		return zero, fmt.Errorf("probe control owner: unhandled probe state %q", probe.state)
	}
}
