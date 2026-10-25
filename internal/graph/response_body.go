package graph

import (
	"errors"
	"io"
)

// ResponseReadError marks a failure while reading a response body: the
// connection dropped, timed out or was reset after OneDrive had started
// answering. net/http reports those without the *url.Error it uses for
// failures before the response, so without this mark a caller could not tell
// a dropped download from a local disk error that io.Copy returned for the
// same call. Unwrap keeps the cause (io.ErrUnexpectedEOF, a timeout, an HTTP/2
// stream reset, context cancellation) reachable through errors.Is/As.
type ResponseReadError struct {
	Err error
}

func (e *ResponseReadError) Error() string {
	return "graph: reading the response: " + e.Err.Error()
}

func (e *ResponseReadError) Unwrap() error { return e.Err }

// markedResponseBody wraps a response body so its read failures (anything
// but io.EOF, which is the normal end) carry *ResponseReadError.
type markedResponseBody struct {
	io.ReadCloser
}

func (b markedResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, &ResponseReadError{Err: err}
	}

	return n, err //nolint:wrapcheck // io.EOF must reach io.Copy and decoders unwrapped
}
