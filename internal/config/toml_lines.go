package config

import (
	"fmt"
	"strconv"
	"strings"
)

// tomlControlCharLimit is the first character that is not a control
// character; tomlDelete is the one control character above it.
const (
	tomlControlCharLimit = 0x20
	tomlDelete           = 0x7f
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

// renderSectionHeaderLine and parseSectionHeader are the one pair that writes
// and reads a table header. The name is written as a TOML basic string and
// read back with its escapes undone, so a quote or a backslash in a drive's
// name (a SharePoint library may hold either) finds its section again.
func renderSectionHeaderLine(name string) string {
	return "[" + tomlString(name) + "]"
}

// parseSectionHeader reads a table header line: [name], ["basic string"] or
// ['literal string'], with an optional trailing comment. It returns the name
// with escapes undone and false for anything else, including an array of
// tables ([[name]]), which the config does not use.
func parseSectionHeader(trimmed string) (string, bool) {
	if !strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "[[") {
		return "", false
	}

	name, rest, ok := cutHeaderName(strings.TrimSpace(trimmed[1:]))
	if !ok {
		return "", false
	}

	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "]") {
		return "", false
	}

	after := strings.TrimSpace(rest[1:])
	if after != "" && !strings.HasPrefix(after, "#") {
		return "", false
	}

	return name, true
}

// cutHeaderName splits a header's name from what follows it, undoing a
// quoted name's escapes.
func cutHeaderName(header string) (name, rest string, ok bool) {
	switch {
	case strings.HasPrefix(header, `"`):
		end := closingBasicQuote(header)
		if end < 0 {
			return "", "", false
		}

		unquoted, err := strconv.Unquote(header[:end+1])
		if err != nil {
			return "", "", false
		}

		return unquoted, header[end+1:], true
	case strings.HasPrefix(header, "'"):
		end := strings.IndexByte(header[1:], '\'')
		if end < 0 {
			return "", "", false
		}

		return header[1 : end+1], header[end+2:], true
	default:
		end := strings.IndexByte(header, ']')
		if end < 0 {
			return "", "", false
		}

		return strings.TrimSpace(header[:end]), header[end:], true
	}
}

// closingBasicQuote returns the index of the quote that closes the basic
// string s starts with, skipping escaped characters, or -1.
func closingBasicQuote(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}

	return -1
}

// tomlString renders s as a TOML basic string. Go's %q is not one: it writes
// \a, \v and \xNN escapes, which TOML rejects, so a value holding such a
// character would leave a config file that no longer loads. Every other
// control character becomes a \uXXXX escape. Every string tomlString writes
// is also a Go string literal, which is how parseSectionHeader reads one back.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < tomlControlCharLimit || r == tomlDelete:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')

	return b.String()
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
