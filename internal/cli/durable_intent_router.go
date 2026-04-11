package cli

import (
	"context"

	"github.com/tonimelisma/onedrive-go/internal/synccontrol"
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

	client, ok, err := openControlSocketClient(ctx)
	if err != nil || !ok || client.ownerMode() != synccontrol.OwnerModeWatch {
		return direct(ctx)
	}

	result, err := watch(ctx, client)
	if err == nil {
		return result, nil
	}
	if isControlDaemonError(err) {
		return zero, err
	}

	return direct(ctx)
}
