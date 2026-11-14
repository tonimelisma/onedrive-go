package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Every byte of JSON a command prints is encoded in this file: one document
// through writeJSONDocument, or one line of `sync --json` through
// writeJSONLine. Both go through writeJSONOutput, so the escaping and the
// error texts are set once. The forbidigo rule in .golangci.yml refuses a
// direct call of json.NewEncoder, json.Marshal or json.MarshalIndent in the
// package's other non-test files. The one other file it exempts,
// control_request.go, encodes only the perf capture request the CLI sends
// over the control socket, which is not output.
//
// A list never prints as null. Each output type that can hold a list has one
// normalizing step that turns every nil list into an empty one, and its
// printer hands the value over through it: the type's withEmptyLists method,
// or for the status document newStatusOutput.
// TestJSONOutputCalls_PrintDeclaredTypesWithTheirListsFilled holds every call
// of the two functions to that, and TestJSONOutputTypes_NoListPrintsNull holds
// each step to its type's lists.

// jsonDocumentIndent indents a --json document.
const jsonDocumentIndent = "  "

// writeJSONDocument writes v as the command's single --json document,
// indented by two spaces and ending in one newline.
//
// Every command that honors --json ends with exactly one call to this, so
// stdout holds one parseable value and nothing else.
// TestCommandTree_EveryLeafHonorsJSON walks the whole command tree to keep it
// that way.
func writeJSONDocument(w io.Writer, v any) error {
	return writeJSONOutput(w, v, jsonDocumentIndent)
}

// writeJSONLine writes v as one compact line ending in a newline: one event
// of `sync --json`.
func writeJSONLine(w io.Writer, v any) error {
	return writeJSONOutput(w, v, "")
}

// writeJSONOutput encodes v into memory, then writes it in one call, so a
// value that cannot be encoded writes nothing and the two failures read
// differently: "encode JSON output" is a value the encoder refused, "write
// JSON output" a stdout that would not take the bytes (a closed pipe). HTML
// escaping is off: the readers are scripts, not browsers, so a file name
// holding an ampersand or an angle bracket prints it as itself rather than as
// a Unicode escape sequence. An empty indent writes compact JSON.
func writeJSONOutput(w io.Writer, v any, indent string) error {
	var encoded bytes.Buffer
	enc := json.NewEncoder(&encoded)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)

	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode JSON output: %w", err)
	}

	if _, err := w.Write(encoded.Bytes()); err != nil {
		return fmt.Errorf("write JSON output: %w", err)
	}

	return nil
}

// emptyIfNil returns list, or an empty list of its type when list is nil, so
// it prints [] rather than null. The withEmptyLists methods use it.
func emptyIfNil[S ~[]E, E any](list S) S {
	if list == nil {
		return S{}
	}

	return list
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
