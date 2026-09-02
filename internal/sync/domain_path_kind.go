package sync

import "strings"

// observedKind and the path helpers below are domain vocabulary rather than
// observation internals: the content filter decides visibility from a kind and
// a path, and it sits in the domain because every family applies it. Leaving
// them in the scanner made the domain appear to depend on observation.

type observedKind uint8

const (
	observedKindUnknown observedKind = iota
	observedKindFile
	observedKindDir
)

// asciiLower returns s with ASCII uppercase letters converted to lowercase.
// Unlike strings.ToLower, this avoids heap allocation when s is already
// lowercase (the common case for filenames). Non-ASCII bytes are passed through
// unchanged, which is correct for file extension matching.
func asciiLower(s string) string {
	for i := range len(s) {
		if s[i] >= 'A' && s[i] <= 'Z' {
			// Found an uppercase letter — allocate and convert.
			buf := make([]byte, len(s))
			copy(buf, s[:i])

			for j := i; j < len(s); j++ {
				if s[j] >= 'A' && s[j] <= 'Z' {
					buf[j] = s[j] + ('a' - 'A')
				} else {
					buf[j] = s[j]
				}
			}

			return string(buf)
		}
	}

	// No uppercase letters found — return the original string (zero alloc).
	return s
}

func hasDotfileComponent(parts []string) bool {
	for _, part := range parts {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}

	return false
}
