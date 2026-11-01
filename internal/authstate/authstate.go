// Package authstate owns the shared user-facing auth-health vocabulary used by
// CLI auth surfaces and the unauthorized sync issue presentation.
package authstate

const (
	StateReady                  = "ready"
	StateAuthenticationRequired = "authentication_required"
)

type Reason string

const (
	ReasonMissingLogin      Reason = "missing_login"
	ReasonInvalidSavedLogin Reason = "invalid_saved_login"
	ReasonSyncAuthRejected  Reason = "sync_auth_rejected"
	ReasonSavedLoginExpired Reason = "saved_login_expired"
	// ReasonSavedLoginUnreadable means the token file could not be checked
	// or read (permission denied, I/O error): distinct from a missing login,
	// because signing in again would write to the same unreadable place.
	ReasonSavedLoginUnreadable Reason = "saved_login_unreadable"
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
	case ReasonSavedLoginExpired:
		return Presentation{
			Title:  "Authentication required",
			Reason: "Microsoft no longer accepts the saved login (it expired after a long time unused, or was revoked).",
			Action: "Run 'onedrive-go login' to sign in again.",
		}
	case ReasonSavedLoginUnreadable:
		return Presentation{
			Title:  "Saved login cannot be read",
			Reason: "The saved login for this account could not be read from the data directory.",
			Action: "Check that the data directory is readable, then run 'onedrive-go status' again.",
		}
	case ReasonSyncAuthRejected:
		return Presentation{
			Title:  "Authentication required",
			Reason: "The last sync attempt for this account was rejected by OneDrive.",
			Action: "Run 'onedrive-go status' to re-check access, or 'onedrive-go login' to sign in again.",
		}
	default:
		return Presentation{}
	}
}

func UnauthorizedIssuePresentation() Presentation {
	return PresentationForReason(ReasonSyncAuthRejected)
}
