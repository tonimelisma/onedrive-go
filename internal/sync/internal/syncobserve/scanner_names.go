// OneDrive name and path validation applied to every observed entry.

package syncobserve

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// validateOneDriveNameFor applies the name rules of the target drive type.
// personal relaxes the Business/SharePoint-only reservations (lockfiles,
// desktop.ini, "~$" owner files) that OneDrive Personal accepts.
func validateOneDriveNameFor(name string, personal bool) (reason syncdomain.IssueType, detail string) {
	if name == "" {
		return syncdomain.IssueInvalidFilename, "the name is empty"
	}

	// Graph would reinterpret invalid bytes (0xFF arrives as "ÿ"), and the
	// renamed remote item would then rename the local file to match.
	if !utf8.ValidString(name) {
		return syncdomain.IssueInvalidFilename, "the name is not valid UTF-8"
	}

	if containsControlChar(name) {
		return syncdomain.IssueInvalidFilename, "the name contains a control character (such as a tab or newline)"
	}

	if name[len(name)-1] == '.' {
		return syncdomain.IssueInvalidFilename, "the name ends with a period"
	}

	if name[len(name)-1] == ' ' {
		return syncdomain.IssueInvalidFilename, "the name ends with a space"
	}

	if name[0] == ' ' {
		return syncdomain.IssueInvalidFilename, "the name starts with a space"
	}

	lower := strings.ToLower(name)

	if isReservedDeviceName(lower) {
		return syncdomain.IssueInvalidFilename, "the name is reserved by Windows"
	}

	if strings.Contains(lower, "_vti_") {
		return syncdomain.IssueInvalidFilename, `OneDrive reserves names containing "_vti_"`
	}

	if !personal && isReservedPattern(name, lower) {
		return syncdomain.IssueInvalidFilename, "OneDrive for work or school reserves this name"
	}

	if containsInvalidChars(name) {
		return syncdomain.IssueInvalidFilename, `the name contains a character OneDrive does not allow (" * : < > ? / \ |)`
	}

	return "", ""
}

func validateObservedName(name, path string, rules syncdomain.LocalObservationRules) (reason syncdomain.IssueType, detail string) {
	if reason, detail := validateOneDriveNameFor(name, rules.PersonalNameRules); reason != "" {
		return reason, detail
	}

	if rules.RejectSharePointRootForms && isSharePointRootForms(name, path) {
		return syncdomain.IssueInvalidFilename, "the name is reserved at the root of a SharePoint library"
	}

	return "", ""
}

func isSharePointRootForms(name, path string) bool {
	return path == name && strings.EqualFold(name, "forms")
}

// isReservedDeviceName returns true for Windows reserved device names
// (case-insensitive): CON, PRN, AUX, NUL, COM0-COM9, LPT0-LPT9, with or
// without an extension. OneDrive rejects "con.txt" and "nul.tar.gz" exactly
// like "con", so only the part before the first period matters.
func isReservedDeviceName(lower string) bool {
	if base, _, found := strings.Cut(lower, "."); found {
		lower = base
	}

	switch lower {
	case "con", "prn", "aux", "nul":
		return true
	}

	// COM0-COM9, LPT0-LPT9: exactly 4 characters, prefix + single digit.
	if len(lower) == deviceNameWithDigitLen &&
		(strings.HasPrefix(lower, "com") || strings.HasPrefix(lower, "lpt")) {
		digit := lower[3]
		return digit >= '0' && digit <= '9'
	}

	return false
}

// containsControlChar reports C0 control characters and DEL, which Graph
// rejects in names.
func containsControlChar(name string) bool {
	for _, c := range name {
		if c < 0x20 || c == 0x7f {
			return true
		}
	}

	return false
}

// isReservedPattern returns true for the names OneDrive for work or school
// reserves: .lock extension, desktop.ini, ~$ prefix (Office temp). "_vti_" is
// checked separately because every drive type rejects it.
func isReservedPattern(name, lower string) bool {
	if strings.HasSuffix(lower, ".lock") {
		return true
	}

	if lower == "desktop.ini" {
		return true
	}

	return strings.HasPrefix(name, "~$")
}

// containsInvalidChars returns true if the name contains characters
// forbidden by OneDrive: " * : < > ? / \ |
func containsInvalidChars(name string) bool {
	for _, c := range name {
		switch c {
		case '"', '*', ':', '<', '>', '?', '/', '\\', '|':
			return true
		}
	}

	return false
}

// oneDrivePathLength counts a path the way OneDrive's 400-character limit
// does: in UTF-16 code units, the unit its error messages and its 260-unit
// emoji names measure (L-05). Counting UTF-8 bytes skipped valid paths: a
// 150-character CJK path is 450 bytes, and 300 "ä" are 600.
func oneDrivePathLength(path string) int {
	return len(utf16.Encode([]rune(path)))
}
