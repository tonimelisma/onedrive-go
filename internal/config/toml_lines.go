package config

import (
	"fmt"
	"strings"
)

// lineKind classifies a line in a TOML file for structured editing.
type lineKind int

const (
	lineBlank    lineKind = iota // empty or whitespace-only
	lineComment                  // starts with #
	lineSection                  // [section] or ["quoted:section"]
	lineKeyValue                 // key = value
	lineOther                    // everything else (multi-line values, etc.)
)

// parsedLine is a lightweight representation of a single TOML line.
// Only lines being modified are reconstructed; all others round-trip
// via their original raw text.
type parsedLine struct {
	kind          lineKind
	raw           string // original text — round-trip preserved
	key           string // for lineKeyValue: parsed TOML key
	value         string // for lineKeyValue: text after "=" (trimmed)
	inlineComment string // for lineKeyValue: trailing "# ..." if present
	sectionName   string // for lineSection: unquoted header name
}

// parseLine classifies a single TOML line and extracts structured fields.
func parseLine(raw string) parsedLine {
	trimmed := strings.TrimSpace(raw)

	if trimmed == "" {
		return parsedLine{kind: lineBlank, raw: raw}
	}

	if strings.HasPrefix(trimmed, "#") {
		return parsedLine{kind: lineComment, raw: raw}
	}

	if name, ok := parseSectionHeader(trimmed); ok {
		return parsedLine{kind: lineSection, raw: raw, sectionName: name}
	}

	if key, value, comment, ok := parseKeyValue(trimmed); ok {
		return parsedLine{
			kind:          lineKeyValue,
			raw:           raw,
			key:           key,
			value:         value,
			inlineComment: comment,
		}
	}

	return parsedLine{kind: lineOther, raw: raw}
}

// parseSectionHeader extracts the section name from a line like [name] or ["quoted:name"].
// Returns the unquoted name and true if successful.
func parseSectionHeader(trimmed string) (string, bool) {
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return "", false
	}

	// Reject array-of-tables [[...]] — not used in our config but handled for safety.
	if strings.HasPrefix(trimmed, "[[") {
		return "", false
	}

	inner := trimmed[1 : len(trimmed)-1]
	inner = strings.TrimSpace(inner)

	// Handle quoted section names: ["personal:user@example.com"]
	if len(inner) >= 2 && inner[0] == '"' && inner[len(inner)-1] == '"' {
		return inner[1 : len(inner)-1], true
	}

	return inner, true
}

// parseKeyValue splits a line into key, value, and inline comment.
// Handles both "key = value" and "key=value" forms. Returns false if
// the line is not a valid key-value pair.
func parseKeyValue(trimmed string) (key, value, inlineComment string, ok bool) {
	eqIdx := strings.Index(trimmed, "=")
	if eqIdx < 1 {
		return "", "", "", false
	}

	key = strings.TrimSpace(trimmed[:eqIdx])

	// Keys must be bare or quoted identifiers — reject lines where the
	// "key" part contains spaces (likely a comment or something else).
	if strings.ContainsAny(key, " \t") {
		return "", "", "", false
	}

	rest := strings.TrimSpace(trimmed[eqIdx+1:])

	// Extract inline comment, but only if the '#' is outside a quoted string.
	value, inlineComment = splitInlineComment(rest)

	return key, value, inlineComment, true
}

// splitInlineComment separates a TOML value from any trailing inline comment.
// Handles quoted strings correctly — '#' inside quotes is not a comment.
func splitInlineComment(rest string) (value, comment string) {
	inQuote := false
	escaped := false

	for i, ch := range rest {
		if escaped {
			escaped = false

			continue
		}

		if ch == '\\' && inQuote {
			escaped = true

			continue
		}

		if ch == '"' {
			inQuote = !inQuote

			continue
		}

		if ch == '#' && !inQuote {
			return strings.TrimSpace(rest[:i]), strings.TrimSpace(rest[i:])
		}
	}

	return rest, ""
}

// parseLines splits content into lines and classifies each one.
func parseLines(content string) []parsedLine {
	raws := strings.Split(content, "\n")
	lines := make([]parsedLine, len(raws))

	for i, raw := range raws {
		lines[i] = parseLine(raw)
	}

	return lines
}

// renderLines joins parsed lines back into a string using their raw text.
func renderLines(lines []parsedLine) string {
	raws := make([]string, len(lines))
	for i, l := range lines {
		raws[i] = l.raw
	}

	return strings.Join(raws, "\n")
}

// renderKeyValueLine constructs a TOML key-value line, preserving any
// inline comment from the original line.
func renderKeyValueLine(key, formattedValue, inlineComment string) string {
	line := fmt.Sprintf("%s = %s", key, formattedValue)
	if inlineComment != "" {
		line += " " + inlineComment
	}

	return line
}

// findSectionByName locates a section by its parsed name. Returns the
// header line index and whether the section was found.
func findSectionByName(lines []parsedLine, name string) (headerIdx int, found bool) {
	for i, l := range lines {
		if l.kind == lineSection && l.sectionName == name {
			return i, true
		}
	}

	return -1, false
}

// sectionContentRange returns the start (inclusive) and end (exclusive) indices
// of a section's content lines. Content ends before the next section header.
// Trailing blank lines and comments before the next section are excluded
// (they belong to the next section's preamble).
func sectionContentRange(lines []parsedLine, headerIdx int) (contentStart, contentEnd int) {
	contentStart = headerIdx + 1

	// Find the next section header.
	nextHeader := len(lines)
	for i := contentStart; i < len(lines); i++ {
		if lines[i].kind == lineSection {
			nextHeader = i

			break
		}
	}

	// Walk backwards to exclude trailing blank lines and comments that
	// belong to the next section's preamble.
	end := nextHeader
	for end > contentStart {
		k := lines[end-1].kind
		if k == lineBlank || k == lineComment {
			end--

			continue
		}

		break
	}

	return contentStart, end
}

// findKeyInRange finds a key-value line with an exact key match within
// a line range [start, end). Returns the line index and whether found.
func findKeyInRange(lines []parsedLine, start, end int, key string) (idx int, found bool) {
	for i := start; i < end; i++ {
		if lines[i].kind == lineKeyValue && lines[i].key == key {
			return i, true
		}
	}

	return -1, false
}
