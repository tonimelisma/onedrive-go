package cli

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jsonFormatSample is a value whose encoded bytes the format tests pin: a
// name holding the characters HTML escaping would rewrite, and a list.
type jsonFormatSample struct {
	Name  string   `json:"name"`
	Items []string `json:"items"`
}

const jsonFormatSampleName = "Q&A <draft> v2>v1.txt"

// A --json document is indented by two spaces and ends in one newline; &, <
// and > print as themselves; an empty list prints []; a value that cannot be
// encoded returns the error and writes nothing.
//
// Validates: R-6.6.33
func TestWriteJSONDocument_Format(t *testing.T) {
	t.Parallel()

	var object bytes.Buffer
	require.NoError(t, writeJSONDocument(&object, jsonFormatSample{Name: jsonFormatSampleName, Items: []string{}}))
	//nolint:testifylint // byte-exact on purpose: the indentation, escaping and newline are what is tested
	assert.Equal(t, "{\n  \"name\": \"Q&A <draft> v2>v1.txt\",\n  \"items\": []\n}\n", object.String())

	var list bytes.Buffer
	require.NoError(t, writeJSONDocument(&list, []jsonFormatSample{{Name: "a&b", Items: []string{"<x>"}}}))
	//nolint:testifylint // byte-exact on purpose: the indentation, escaping and newline are what is tested
	assert.Equal(t, "[\n  {\n    \"name\": \"a&b\",\n    \"items\": [\n      \"<x>\"\n    ]\n  }\n]\n", list.String())

	var failed bytes.Buffer
	err := writeJSONDocument(&failed, map[string]any{"events": make(chan int)})
	require.ErrorContains(t, err, "encode JSON output")
	assert.Empty(t, failed.String(), "a value that cannot be encoded leaves stdout untouched")
}

// A sync --json line is one compact object ending in one newline, with the
// same escaping as a document.
//
// Validates: R-6.6.28, R-6.6.33
func TestWriteJSONLine_Format(t *testing.T) {
	t.Parallel()

	var lines bytes.Buffer
	require.NoError(t, writeJSONLine(&lines, jsonFormatSample{Name: jsonFormatSampleName, Items: []string{}}))
	require.NoError(t, writeJSONLine(&lines, jsonFormatSample{Name: "b", Items: []string{"<x>"}}))
	//nolint:testifylint // byte-exact on purpose: compactness, escaping and the newlines are what is tested
	assert.Equal(t,
		"{\"name\":\"Q&A <draft> v2>v1.txt\",\"items\":[]}\n{\"name\":\"b\",\"items\":[\"<x>\"]}\n",
		lines.String())

	var failed bytes.Buffer
	err := writeJSONLine(&failed, map[string]any{"events": make(chan int)})
	require.ErrorContains(t, err, "encode JSON output")
	assert.Empty(t, failed.String(), "a value that cannot be encoded leaves stdout untouched")
}

// A document or line that encodes but cannot be written fails with an error
// that names the write, not the encoding, and wraps what the writer returned
// (a closed pipe is an I/O failure).
//
// Validates: R-6.6.33
func TestWriteJSONOutput_WriteFailureIsNamed(t *testing.T) {
	t.Parallel()

	closedPipe := errors.New("broken pipe")
	writers := map[string]func(io.Writer, any) error{
		"document": writeJSONDocument,
		"line":     writeJSONLine,
	}
	for form, write := range writers {
		err := write(&flakyWriter{failures: 1, err: closedPipe}, jsonFormatSample{Name: "a"})
		require.ErrorIsf(t, err, closedPipe, "%s", form)
		assert.Equalf(t, "write JSON output: broken pipe", err.Error(), "%s", form)
	}
}
