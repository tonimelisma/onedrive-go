package authstate

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func TestPresentationForReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		reason     Reason
		wantReason string
		wantAction string
	}{
		{
			name:       "missing login",
			reason:     ReasonMissingLogin,
			wantReason: "No saved login was found for this account.",
			wantAction: "Run 'onedrive-go login' to sign in.",
		},
		{
			name:       "invalid saved login",
			reason:     ReasonInvalidSavedLogin,
			wantReason: "The saved login for this account is invalid or unreadable.",
			wantAction: "Run 'onedrive-go login' to sign in.",
		},
		{
			name:       "sync auth rejected",
			reason:     ReasonSyncAuthRejected,
			wantReason: "The last sync attempt for this account was rejected by OneDrive.",
			wantAction: "Run 'onedrive-go whoami' to re-check access, or 'onedrive-go login' to sign in again.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			presentation := PresentationForReason(tc.reason)
			assert.Equal(t, "Authentication required", presentation.Title)
			assert.Equal(t, tc.wantReason, presentation.Reason)
			assert.Equal(t, tc.wantAction, presentation.Action)
		})
	}
}

func TestHealthHelpers(t *testing.T) {
	t.Parallel()

	assert.Equal(t, Health{State: StateReady}, ReadyHealth())
	assert.Equal(t, Health{
		State:  StateAuthenticationRequired,
		Reason: ReasonInvalidSavedLogin,
		Action: "Run 'onedrive-go login' to sign in.",
	}, RequiredHealth(ReasonInvalidSavedLogin))
	assert.Equal(t, Health{}, RequiredHealth(Reason("unknown")))
	assert.Equal(t, PresentationForReason(ReasonSyncAuthRejected), UnauthorizedIssuePresentation())
}

func TestErrorMessage(t *testing.T) {
	t.Parallel()

	assert.Equal(
		t,
		"Authentication required: no saved login was found for this account. Run 'onedrive-go login'.",
		ErrorMessage(graph.ErrNotLoggedIn),
	)
	assert.Equal(
		t,
		"Authentication required: OneDrive rejected the saved login for this account. Run 'onedrive-go login'.",
		ErrorMessage(graph.ErrUnauthorized),
	)
	assert.Empty(t, ErrorMessage(assert.AnError))
}
