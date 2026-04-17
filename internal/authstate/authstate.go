// Package authstate owns the shared user-facing auth-health vocabulary used by
// CLI auth surfaces and the unauthorized sync issue presentation.
package authstate

import (
	"errors"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

const (
	StateReady                  = "ready"
	StateAuthenticationRequired = "authentication_required"
)

type Reason string

const (
	ReasonMissingLogin      Reason = "missing_login"
	ReasonInvalidSavedLogin Reason = "invalid_saved_login"
	ReasonSyncAuthRejected  Reason = "sync_auth_rejected"
)

type Presentation struct {
	Title  string
	Reason string
	Action string
}

type Health struct {
	State  string
	Reason Reason
	Action string
}

func ReadyHealth() Health {
	return Health{State: StateReady}
}

func RequiredHealth(reason Reason) Health {
	presentation := PresentationForReason(reason)
	if presentation.Title == "" {
		return Health{}
	}

	return Health{
		State:  StateAuthenticationRequired,
		Reason: reason,
		Action: presentation.Action,
	}
}

func PresentationForReason(reason Reason) Presentation {
	switch reason {
	case ReasonMissingLogin:
		return Presentation{
			Title:  "Authentication required",
			Reason: "No saved login was found for this account.",
			Action: "Run 'onedrive-go login' to sign in.",
		}
	case ReasonInvalidSavedLogin:
		return Presentation{
			Title:  "Authentication required",
			Reason: "The saved login for this account is invalid or unreadable.",
			Action: "Run 'onedrive-go login' to sign in.",
		}
	case ReasonSyncAuthRejected:
		return Presentation{
			Title:  "Authentication required",
			Reason: "The last sync attempt for this account was rejected by OneDrive.",
			Action: "Run 'onedrive-go whoami' to re-check access, or 'onedrive-go login' to sign in again.",
		}
	default:
		return Presentation{}
	}
}

func UnauthorizedIssuePresentation() Presentation {
	return PresentationForReason(ReasonSyncAuthRejected)
}

func ErrorMessage(err error) string {
	switch {
	case errors.Is(err, graph.ErrNotLoggedIn):
		return "Authentication required: no saved login was found for this account. Run 'onedrive-go login'."
	case errors.Is(err, graph.ErrUnauthorized):
		return "Authentication required: OneDrive rejected the saved login for this account. Run 'onedrive-go login'."
	default:
		return ""
	}
}
