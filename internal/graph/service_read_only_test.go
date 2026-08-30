package graph

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forbiddenGraphError builds the shape Graph actually returns for a backend
// read-only window: an outer accessDenied whose inner code carries the real
// reason.
func forbiddenGraphError(innerCodes ...string) error {
	return &GraphError{
		StatusCode: http.StatusForbidden,
		Code:       "accessDenied",
		InnerCodes: innerCodes,
		Message:    "Database Is Read Only",
		RequestID:  "req-1",
		Err:        ErrForbidden,
	}
}

// Validates: R-6.6.17
//
// Graph reports a backend read-only window as an outer accessDenied with an
// inner serviceReadOnly. The token-propagation retry family keys on
// accessDenied, so without an explicit exclusion it burns its whole budget on
// a condition no amount of retrying can clear.
func TestIsTransientDrivesDiscoveryError_ServiceReadOnlyIsNotTokenPropagation(t *testing.T) {
	t.Parallel()

	_, retryable := isTransientDrivesDiscoveryError(forbiddenGraphError("serviceReadOnly")) //nolint:errcheck // the bool is the assertion; the error value is irrelevant here

	assert.False(t, retryable, "a provider read-only window must not be retried as token propagation")
}

// Validates: R-6.6.17
func TestIsTransientDrivesDiscoveryError_PlainAccessDeniedStillRetries(t *testing.T) {
	t.Parallel()

	graphErr, retryable := isTransientDrivesDiscoveryError(forbiddenGraphError())
	require.True(t, retryable, "ordinary token-propagation accessDenied must still retry")
	require.NotNil(t, graphErr)
}

// Validates: R-6.6.17
//
// Callers need to tell "the provider is unavailable" apart from "this account
// cannot do that", so the read-only window carries its own sentinel.
func TestServiceReadOnly_IsClassifiedAsProviderUnavailable(t *testing.T) {
	t.Parallel()

	err := forbiddenGraphError("serviceReadOnly")
	require.ErrorIs(t, err, ErrProviderReadOnly,
		"a serviceReadOnly response must be recognizable as a provider read-only window")
	assert.True(t, IsProviderUnavailable(err))
}

// Validates: R-6.6.17
func TestServiceReadOnly_OrdinaryForbiddenIsNotProviderUnavailable(t *testing.T) {
	t.Parallel()

	assert.False(t, IsProviderUnavailable(forbiddenGraphError()))
	assert.False(t, IsProviderUnavailable(errors.New("unrelated")))
}

// Validates: R-6.6.17
func TestMostSpecificErrorCode_PrefersInnerCode(t *testing.T) {
	t.Parallel()

	body := []byte(`{"error":{"code":"accessDenied","message":"Database Is Read Only",` +
		`"innererror":{"code":"serviceReadOnly"}}}`)
	code := MostSpecificErrorCode(body)
	assert.Equal(t, "serviceReadOnly", code)
	assert.True(t, IsProviderReadOnlyCode(code))
}

// Validates: R-6.6.17
func TestMostSpecificErrorCode_FallsBackToOuterCode(t *testing.T) {
	t.Parallel()

	code := MostSpecificErrorCode([]byte(`{"error":{"code":"accessDenied","message":"nope"}}`))
	assert.Equal(t, "accessDenied", code)
	assert.False(t, IsProviderReadOnlyCode(code))
}

// Validates: R-6.6.17
func TestMostSpecificErrorCode_HandlesUnparseableBody(t *testing.T) {
	t.Parallel()

	assert.Empty(t, MostSpecificErrorCode([]byte("not json")))
	assert.False(t, IsProviderReadOnlyCode(""))
}
