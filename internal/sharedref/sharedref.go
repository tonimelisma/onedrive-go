// Package sharedref parses and formats user-facing shared item selectors.
package sharedref

import (
	"fmt"
	"strings"
)

// Prefix is the selector prefix for shared item references.
const Prefix = "shared:"

const selectorPartCount = 4

// Ref identifies one shared item for one recipient account. It intentionally
// stays separate from driveid.CanonicalID because shared files are direct item
// targets, not configured drives.
type Ref struct {
	AccountEmail  string
	RemoteDriveID string
	RemoteItemID  string
}

// Parse parses a shared item selector in the form:
// shared:<recipientEmail>:<remoteDriveID>:<remoteItemID>
func Parse(raw string) (Ref, error) {
	if !strings.HasPrefix(raw, Prefix) {
		return Ref{}, fmt.Errorf("sharedref: selector %q must start with %q", raw, Prefix)
	}

	parts := strings.SplitN(raw, ":", selectorPartCount)
	if len(parts) != selectorPartCount {
		return Ref{}, fmt.Errorf(
			"sharedref: selector %q must be %q<recipientEmail>:<remoteDriveID>:<remoteItemID>",
			raw, Prefix,
		)
	}

	if parts[1] == "" {
		return Ref{}, fmt.Errorf("sharedref: selector %q requires non-empty recipient email", raw)
	}

	if parts[2] == "" {
		return Ref{}, fmt.Errorf("sharedref: selector %q requires non-empty remote drive ID", raw)
	}

	if parts[3] == "" {
		return Ref{}, fmt.Errorf("sharedref: selector %q requires non-empty remote item ID", raw)
	}

	return Ref{
		AccountEmail:  parts[1],
		RemoteDriveID: parts[2],
		RemoteItemID:  parts[3],
	}, nil
}

// MustParse panics on invalid shared item selectors. Use only in tests and
// static initialization.
func MustParse(raw string) Ref {
	ref, err := Parse(raw)
	if err != nil {
		panic(err)
	}

	return ref
}

// String formats the canonical shared item selector.
func (r Ref) String() string {
	if r.AccountEmail == "" || r.RemoteDriveID == "" || r.RemoteItemID == "" {
		return ""
	}

	return Prefix + r.AccountEmail + ":" + r.RemoteDriveID + ":" + r.RemoteItemID
}

// IsZero reports whether the reference is unset.
func (r Ref) IsZero() bool {
	return r.AccountEmail == "" || r.RemoteDriveID == "" || r.RemoteItemID == ""
}
