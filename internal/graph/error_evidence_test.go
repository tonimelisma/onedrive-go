package graph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	evidenceRequestID     = "example-request-id"
	evidenceAgsDiagnostic = "example-diagnostic"
	evidencePath          = "/drives/example-drive/items/example-item"
	evidenceMessage       = "example message text"
	evidenceJSON          = "application/json"
	evidenceMoveBody      = `{"name":"moved"}`
	evidenceCode          = "invalidRequest"
	evidenceInnerCode     = "exampleInner"
	evidenceInnermostCode = "exampleInnermost"
	evidenceChainBody     = `{"error":{"code":"` + evidenceCode + `","message":"` + evidenceMessage +
		`","innerError":{"code":"` + evidenceInnerCode + `","innerError":{"code":"` + evidenceInnermostCode + `"}}}}`
	// evidenceCutBody is all of an error body that arrives before its
	// connection drops: the start of a Graph error object.
	evidenceCutBody   = `{"error":{"code":"invalidRe`
	evidenceReadCause = "example connection reset"
	evidenceDriveID   = "d"
	evidenceItemID    = "example-item"
	evidenceFragment  = "fragment bytes"
	evidenceUploadURL = "http://localhost/upload"

	requestFailedMessage  = "request failed"
	bodyReadFailedMessage = "failed to read response body"
)

// debugRecord is one record of the client's debug log, as the JSON handler
// writes it: the "request failed" record of a failed answer, or a record
// written beside it.
//
//nolint:tagliatelle // The record's attribute names are the client's snake_case log keys.
type debugRecord struct {
	Msg           string   `json:"msg"`
	Method        string   `json:"method"`
	Path          string   `json:"path"`
	Status        int      `json:"status"`
	RequestID     string   `json:"request_id"`
	AgsDiagnostic string   `json:"ags_diagnostic"`
	Code          string   `json:"code"`
	InnerCodes    []string `json:"inner_codes"`
	ContentType   string   `json:"content_type"`
	BodyBytes     int      `json:"body_bytes"`
	BodyKind      string   `json:"body_kind"`
	Error         string   `json:"error"`
}

// failedAnswer is what one fake answer left behind: the request's error and
// the client's debug record of it, decoded and as the raw line.
type failedAnswer struct {
	err    *GraphError
	record debugRecord
	line   string
}

// badRequestServer answers every request 400 with the evidence request ID
// and gateway diagnostic, contentType (none when empty) and body.
func badRequestServer(t *testing.T, contentType, body string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", evidenceRequestID)
		w.Header().Set(headerAgsDiagnostic, evidenceAgsDiagnostic)
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(http.StatusBadRequest)
		if body != "" {
			writeClientTestBody(t, w, body)
		}
	}))
	t.Cleanup(srv.Close)

	return srv
}

// captureDebugLog points client's log, from Debug up, at a JSON handler and
// returns what the handler writes.
func captureDebugLog(client *Client) *bytes.Buffer {
	logBuf := &bytes.Buffer{}
	client.logger = slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return logBuf
}

// loggedRecords decodes every record of a captured debug log in order, each
// beside its raw line.
func loggedRecords(t *testing.T, logBuf *bytes.Buffer) ([]debugRecord, []string) {
	t.Helper()

	var (
		records []debugRecord
		lines   []string
	)
	for line := range strings.SplitSeq(strings.TrimSpace(logBuf.String()), "\n") {
		if line == "" {
			continue
		}

		var record debugRecord
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		records = append(records, record)
		lines = append(lines, line)
	}

	return records, lines
}

// recordIndex is the position of the one record whose message is msg; the
// test fails unless exactly one record has it, so an answer recorded twice
// fails like an answer not recorded at all.
func recordIndex(t *testing.T, records []debugRecord, msg string) int {
	t.Helper()

	at, count := 0, 0
	for i := range records {
		if records[i].Msg == msg {
			at, count = i, count+1
		}
	}

	require.Equalf(t, 1, count, "exactly one %q record among %+v", msg, records)

	return at
}

// answerBadRequest sends one PATCH through a client whose debug log is
// captured and whose every request is answered 400 with contentType (none
// when empty) and body, the body read failing with readErr after its last
// byte (nil: it ends), and returns the error and the "request failed" record.
func answerBadRequest(t *testing.T, contentType, body string, readErr error) failedAnswer {
	t.Helper()

	client, logBuf := newAnswerClient(t, answerTransport{
		status: http.StatusBadRequest, contentType: contentType, body: body, readErr: readErr,
	})

	resp, err := client.do(t.Context(), http.MethodPatch, evidencePath, []byte(evidenceMoveBody))
	closeClientTestResponse(t, resp)

	var graphErr *GraphError
	require.ErrorAs(t, err, &graphErr)

	records, lines := loggedRecords(t, logBuf)
	at := recordIndex(t, records, requestFailedMessage)

	return failedAnswer{err: graphErr, record: records[at], line: lines[at]}
}

// cutBody is an answer's body: it yields rest, then fails with err, which is
// io.EOF for a body that ends and any other error for a connection that
// drops.
type cutBody struct {
	rest []byte
	err  error
}

func (b *cutBody) Read(p []byte) (int, error) {
	if len(b.rest) == 0 {
		return 0, b.err
	}

	n := copy(p, b.rest)
	b.rest = b.rest[n:]

	return n, nil
}

func (*cutBody) Close() error { return nil }

// answerTransport answers every request with status, the evidence request ID
// and gateway diagnostic, contentType (none when empty) and body, whose read
// fails with readErr after its last byte (nil: the body ends). The client
// reads the body as the transport hands it over, so what it reads does not
// depend on how much a server had written when the client stopped.
type answerTransport struct {
	status      int
	contentType string
	body        string
	readErr     error
}

func (a answerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A RoundTripper closes the request body, like a transport that sent it.
	if req.Body != nil {
		if err := req.Body.Close(); err != nil {
			return nil, fmt.Errorf("closing request body: %w", err)
		}
	}

	header := http.Header{}
	header.Set("request-id", evidenceRequestID)
	header.Set(headerAgsDiagnostic, evidenceAgsDiagnostic)
	if a.contentType != "" {
		header.Set("Content-Type", a.contentType)
	}

	readErr := a.readErr
	if readErr == nil {
		readErr = io.EOF
	}

	return &http.Response{
		StatusCode: a.status,
		Header:     header,
		Body:       &cutBody{rest: []byte(a.body), err: readErr},
		Request:    req,
	}, nil
}

// cutAnswer is a 400 whose JSON error body drops after evidenceCutBody.
func cutAnswer() answerTransport {
	return answerTransport{
		status: http.StatusBadRequest, contentType: evidenceJSON, body: evidenceCutBody,
		readErr: errors.New(evidenceReadCause),
	}
}

// newAnswerClient is a client whose every request gets the answer of
// transport, and the buffer its debug log goes to.
func newAnswerClient(t *testing.T, transport answerTransport) (*Client, *bytes.Buffer) {
	t.Helper()

	client := MustNewClient("http://localhost", &http.Client{Transport: transport},
		staticToken("test-token"), slog.Default(), "test-agent")

	return client, captureDebugLog(client)
}

// assertBodyReadFailureRecord checks the record of an answer's body that
// could not be read: it names the request, the answer and the cause.
func assertBodyReadFailureRecord(t *testing.T, record *debugRecord, method, path string, status int) {
	t.Helper()

	assert.Equal(t, method, record.Method)
	assert.Equal(t, path, record.Path)
	assert.Equal(t, status, record.Status, "the record names the answer it belongs to")
	assert.Equal(t, evidenceRequestID, record.RequestID, "the record names the answer it belongs to")
	assert.Contains(t, record.Error, evidenceReadCause)
}

// emptyOverwritePath is the request path of overwriteWithNothing.
func emptyOverwritePath() string {
	return fmt.Sprintf("/drives/%s/items/%s/content", driveid.New(evidenceDriveID), evidenceItemID)
}

// overwriteWithNothing overwrites the evidence item with zero bytes, which
// goes out as one simple upload.
func overwriteWithNothing(t *testing.T, client *Client) error {
	t.Helper()

	_, err := client.UploadToItem(t.Context(), driveid.New(evidenceDriveID), evidenceItemID, "",
		strings.NewReader(""), 0, time.Time{}, nil)

	return err
}

// Validates: R-2.10.68, R-2.10.72
func TestBuildError_DebugLineNamesTheRequest(t *testing.T) {
	t.Parallel()

	answer := answerBadRequest(t, evidenceJSON,
		`{"error":{"code":"`+evidenceCode+`","message":"`+evidenceMessage+`"}}`, nil)

	assert.Equal(t, http.MethodPatch, answer.record.Method)
	assert.Equal(t, evidencePath, answer.record.Path)
	assert.Equal(t, http.StatusBadRequest, answer.record.Status)
	assert.Equal(t, evidenceRequestID, answer.record.RequestID)
	assert.Equal(t, evidenceAgsDiagnostic, answer.record.AgsDiagnostic)
	assert.Equal(t, evidenceRequestID, answer.err.RequestID)
	assert.Equal(t, evidenceAgsDiagnostic, answer.err.AgsDiagnostic, "the error keeps the gateway diagnostic")
	assert.NotContains(t, answer.line, evidenceMessage, "the record never holds the message or the body")
}

// bodyEvidenceCase is one body of a failed answer and what the debug record
// and the error text say of it.
type bodyEvidenceCase struct {
	name        string
	contentType string
	body        string
	readErr     error
	wantCode    string
	wantInner   []string
	wantBytes   int
	wantKind    string
	wantText    string
}

func bodyEvidenceCases() []bodyEvidenceCase {
	const (
		textHead     = "graph: HTTP 400 (request-id: " + evidenceRequestID + "): "
		codeOnlyBody = `{"error":{"code":"exampleCode"}}`
		oauthBody    = `{"error":"invalid_request"}`
	)

	// A Graph error object longer than the read bound: only its first
	// maxErrBodySize bytes are read, and they no longer decode.
	overlongBody := `{"error":{"code":"` + evidenceCode + `","message":"` +
		strings.Repeat("x", maxErrBodySize) + `"}}`

	return []bodyEvidenceCase{
		{
			name: "a Graph error object with a code chain", contentType: evidenceJSON, body: evidenceChainBody,
			wantCode: evidenceCode, wantInner: []string{evidenceInnerCode, evidenceInnermostCode},
			wantBytes: len(evidenceChainBody), wantKind: "graph_error",
			wantText: "graph: HTTP 400 " + evidenceInnermostCode + " (request-id: " + evidenceRequestID + "): " + evidenceMessage,
		},
		{
			name: "a plain-text body", contentType: "text/plain", body: evidenceMessage,
			wantInner: []string{}, wantBytes: len(evidenceMessage), wantKind: "other",
			wantText: textHead + evidenceMessage,
		},
		{
			name: "an empty body", wantInner: []string{}, wantKind: "empty",
			wantText: textHead,
		},
		{
			name:        "an error object with a code and no message gives the whole body as the message",
			contentType: evidenceJSON, body: codeOnlyBody,
			wantCode: "exampleCode", wantInner: []string{}, wantBytes: len(codeOnlyBody), wantKind: "graph_error",
			wantText: "graph: HTTP 400 exampleCode (request-id: " + evidenceRequestID + "): " + codeOnlyBody,
		},
		{
			name: "JSON that is not a Graph error object names no code", contentType: evidenceJSON, body: oauthBody,
			wantInner: []string{}, wantBytes: len(oauthBody), wantKind: "other",
			wantText: textHead + oauthBody,
		},
		{
			name: "a body over the read bound is cut at the bound and reads as other", contentType: evidenceJSON,
			body: overlongBody, wantInner: []string{}, wantBytes: maxErrBodySize, wantKind: "other",
			wantText: textHead + overlongBody[:maxErrBodySize],
		},
		{
			name:        "a whole Graph error object followed by a read error is recorded by its codes",
			contentType: evidenceJSON, body: evidenceChainBody, readErr: errors.New(evidenceReadCause),
			wantCode: evidenceCode, wantInner: []string{evidenceInnerCode, evidenceInnermostCode},
			wantBytes: len(evidenceChainBody), wantKind: "graph_error",
			wantText: textHead + unreadableErrorBody,
		},
	}
}

// Validates: R-2.10.72
func TestBuildError_DebugLineRecordsWhatTheBodyHeld(t *testing.T) {
	t.Parallel()

	for _, tt := range bodyEvidenceCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			answer := answerBadRequest(t, tt.contentType, tt.body, tt.readErr)

			assert.Equal(t, tt.wantCode, answer.record.Code)
			assert.Equal(t, tt.wantInner, answer.record.InnerCodes, "inner codes in order, an empty list when none")
			assert.Equal(t, tt.contentType, answer.record.ContentType)
			assert.Equal(t, tt.wantBytes, answer.record.BodyBytes)
			assert.Equal(t, tt.wantKind, answer.record.BodyKind)
			assert.Equal(t, tt.wantText, answer.err.Error())
			assert.NotContains(t, answer.line, evidenceMessage, "the record never holds the message or the body")
		})
	}
}

// Validates: R-2.10.72
func TestBuildError_RecordsAnUnreadableBody(t *testing.T) {
	t.Parallel()

	client, logBuf := newAnswerClient(t, cutAnswer())
	resp, err := client.do(t.Context(), http.MethodPatch, evidencePath, []byte(evidenceMoveBody))
	closeClientTestResponse(t, resp)

	var graphErr *GraphError
	require.ErrorAs(t, err, &graphErr)
	assert.Equal(t, "(failed to read response body)", graphErr.Message, "an unreadable body leaves a placeholder")

	records, _ := loggedRecords(t, logBuf)
	readFailureAt := recordIndex(t, records, bodyReadFailedMessage)
	assertBodyReadFailureRecord(t, &records[readFailureAt], http.MethodPatch, evidencePath, http.StatusBadRequest)

	const arrivedBytes = len(evidenceCutBody)
	answerAt := recordIndex(t, records, requestFailedMessage)
	assert.Greater(t, answerAt, readFailureAt, "the record of the answer follows the read failure")
	assert.Equal(t, arrivedBytes, records[answerAt].BodyBytes, "the bytes that arrived")
	assert.Equal(t, "other", records[answerAt].BodyKind, "a cut Graph error object no longer decodes")
}

// Validates: R-2.10.72
func TestUploadToItem_RefusedEmptyOverwriteRecordsTheAnswer(t *testing.T) {
	t.Parallel()

	client := newNoRetryTestClient(t, badRequestServer(t, evidenceJSON, evidenceChainBody).URL)
	logBuf := captureDebugLog(client)

	var graphErr *GraphError
	require.ErrorAs(t, overwriteWithNothing(t, client), &graphErr)

	records, lines := loggedRecords(t, logBuf)
	at := recordIndex(t, records, requestFailedMessage)
	assert.Equal(t, debugRecord{
		Msg: requestFailedMessage, Method: http.MethodPut, Path: emptyOverwritePath(),
		Status: http.StatusBadRequest, RequestID: evidenceRequestID, AgsDiagnostic: evidenceAgsDiagnostic,
		Code: evidenceCode, InnerCodes: []string{evidenceInnerCode, evidenceInnermostCode},
		ContentType: evidenceJSON, BodyBytes: len(evidenceChainBody), BodyKind: "graph_error",
	}, records[at], "a simple upload's failed answer is recorded like every other")
	assert.NotContains(t, lines[at], evidenceMessage, "the record never holds the message or the body")
}

// Validates: R-2.10.72
func TestUploadToItem_UnreadableErrorBodyIsRecorded(t *testing.T) {
	t.Parallel()

	client, logBuf := newAnswerClient(t, cutAnswer())
	err := overwriteWithNothing(t, client)

	require.EqualError(t, err, "graph: reading upload error body: graph: reading the response: "+evidenceReadCause)
	var graphErr *GraphError
	require.NotErrorAs(t, err, &graphErr, "an answer whose body could not be read is not classified as a Graph answer")

	records, _ := loggedRecords(t, logBuf)
	assertBodyReadFailureRecord(t, &records[recordIndex(t, records, bodyReadFailedMessage)],
		http.MethodPut, emptyOverwritePath(), http.StatusBadRequest)
}

// Validates: R-2.10.72
func TestUploadChunk_UnexpectedStatusRecordsNoBody(t *testing.T) {
	t.Parallel()

	const (
		fragmentBytes = int64(len(evidenceFragment))
		answerBytes   = len(evidenceMessage)
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("request-id", evidenceRequestID)
		w.WriteHeader(http.StatusPartialContent)
		writeClientTestBody(t, w, evidenceMessage)
	}))
	t.Cleanup(srv.Close)

	client := newNoRetryTestClient(t, "http://localhost")
	logBuf := captureDebugLog(client)
	session := &UploadSession{UploadURL: UploadURL(srv.URL + "/upload")}

	_, complete, err := client.UploadChunk(t.Context(), session, strings.NewReader(evidenceFragment),
		0, fragmentBytes, fragmentBytes)
	require.EqualError(t, err, "graph: chunk upload unexpected status 206")
	assert.False(t, complete)
	assert.NotContains(t, logBuf.String(), evidenceMessage, "the debug log never holds the answer's body")

	records, _ := loggedRecords(t, logBuf)
	record := &records[recordIndex(t, records, "chunk upload returned unexpected 2xx status")]
	assert.Equal(t, http.MethodPut, record.Method)
	assert.Equal(t, preAuthLogTarget(uploadChunkDesc), record.Path)
	assert.Equal(t, http.StatusPartialContent, record.Status)
	assert.Equal(t, evidenceRequestID, record.RequestID, "the record names the answer it belongs to")
	assert.Equal(t, answerBytes, record.BodyBytes)
	assert.Equal(t, "other", record.BodyKind)
}

// Validates: R-2.10.72
func TestUploadChunk_UnreadableUnexpectedStatusBodyIsRecorded(t *testing.T) {
	t.Parallel()

	const fragmentBytes = int64(len(evidenceFragment))

	client, logBuf := newAnswerClient(t, answerTransport{
		status: http.StatusPartialContent, contentType: "text/plain", body: evidenceMessage,
		readErr: errors.New(evidenceReadCause),
	})
	session := &UploadSession{UploadURL: UploadURL(evidenceUploadURL)}

	_, complete, err := client.UploadChunk(t.Context(), session, strings.NewReader(evidenceFragment),
		0, fragmentBytes, fragmentBytes)
	require.EqualError(t, err,
		"graph: reading unexpected chunk response body: graph: reading the response: "+evidenceReadCause)
	assert.False(t, complete)
	assert.NotContains(t, logBuf.String(), evidenceMessage, "the debug log never holds the answer's body")

	records, _ := loggedRecords(t, logBuf)
	assertBodyReadFailureRecord(t, &records[recordIndex(t, records, bodyReadFailedMessage)],
		http.MethodPut, preAuthLogTarget(uploadChunkDesc), http.StatusPartialContent)
}
