package cli

import (
	"encoding/json"
	"fmt"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

// encodeControlRequest encodes request as the JSON body a control-socket
// request sends to the running sync. It is the package's one encode that is
// not command output, so the forbidigo rule in .golangci.yml exempts this file
// by name. It takes only the request bodies the CLI sends (a perf capture
// and a confirmation of held deletes), so no other value can pass through
// it. What the rule enforces is that a direct call of json.NewEncoder,
// json.Marshal or json.MarshalIndent in any other non-test file of
// internal/cli fails lint.
func encodeControlRequest[Request synccontrol.PerfCaptureRequest | synccontrol.ConfirmDeletesRequest](
	request Request,
) ([]byte, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode control request: %w", err)
	}

	return encoded, nil
}
