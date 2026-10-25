package cli

import (
	"encoding/json"
	"fmt"
	"io"
)

// writeJSONDocument writes v as the command's single --json document.
//
// Every command that honors --json ends with exactly one call to this (or a
// command-specific printer that does the same), so stdout holds one parseable
// value and nothing else. TestCommandTree_EveryLeafHonorsJSON walks the whole
// command tree to keep it that way.
func writeJSONDocument(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode JSON output: %w", err)
	}

	return nil
}

// resultWriter is where a command writes its human-readable confirmation
// ("Removed drive ...", "Token removed for ..."). Under --json the JSON
// document is the confirmation, so the prose is dropped rather than mixed
// into stdout. Warnings and prompts never go through here: they use the
// status stream, which stays on stderr in both modes.
func (cc *CLIContext) resultWriter() io.Writer {
	if cc.Flags.JSON {
		return io.Discard
	}

	return cc.Output()
}
