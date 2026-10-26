package graph

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.10.65, R-2.10.66
func TestIsQuotaExhaustedAndIsNotSupported(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		err          error
		quota        bool
		notSupported bool
	}{
		{name: "507", err: &GraphError{StatusCode: 507}, quota: true},
		{name: "403 quotaLimitReached inner", err: &GraphError{StatusCode: 403, Code: "accessDenied", InnerCodes: []string{"quotaLimitReached"}}, quota: true},
		{name: "500 insufficient space message", err: &GraphError{StatusCode: 500, Message: "Insufficient Space Available"}, quota: true},
		{name: "insufficientStorage code", err: &GraphError{StatusCode: 409, Code: "insufficientStorage"}, quota: true},
		{name: "501", err: &GraphError{StatusCode: 501}, notSupported: true},
		{name: "505", err: &GraphError{StatusCode: 505}, notSupported: true},
		{name: "notSupported under 400", err: fmt.Errorf("wrapped: %w", &GraphError{StatusCode: 400, Code: "notSupported"}), notSupported: true},
		{name: "plain 500", err: &GraphError{StatusCode: 500, Message: "Internal error"}},
		{name: "not a graph error", err: errors.New("boom")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.quota, IsQuotaExhausted(tt.err))
			assert.Equal(t, tt.notSupported, IsNotSupported(tt.err))
		})
	}
}

// Validates: R-2.10.68
func TestGraphError_KeepsAgsDiagnosticHeader(t *testing.T) {
	const diagnostic = `{"ServerInfo":{"DataCenter":"West US","Slice":"E","Ring":"5","ScaleUnit":"001","RoleInstance":"SJ1PEPF0000"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-ags")
		w.Header().Set("X-Ms-Ags-Diagnostic", diagnostic)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		writeTestResponse(t, w, `{"error":{"code":"generalException","message":"gateway"}}`)
	}))
	t.Cleanup(srv.Close)
	client := newTestClient(t, srv.URL)

	_, err := client.UpdateFileSystemInfo(t.Context(), driveid.New("d"), "item", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))

	var graphErr *GraphError
	require.ErrorAs(t, err, &graphErr)
	assert.Equal(t, "req-ags", graphErr.RequestID)
	assert.JSONEq(t, diagnostic, graphErr.AgsDiagnostic)
}

// Validates: R-2.10.68
func TestBuildQuirkRetryAttempt_CarriesAgsDiagnostic(t *testing.T) {
	t.Parallel()

	record := buildQuirkRetryAttempt(2, &GraphError{StatusCode: 404, Code: "itemNotFound", RequestID: "r", AgsDiagnostic: "diag"})
	assert.Equal(t, QuirkRetryAttempt{Attempt: 2, StatusCode: 404, GraphCode: "itemNotFound", RequestID: "r", AgsDiagnostic: "diag"}, record)
}
