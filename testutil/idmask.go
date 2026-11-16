package testutil

import (
	"regexp"
	"slices"
	"strings"
)

// maskedIdentifier is the one word an identifier no role names becomes.
const maskedIdentifier = "masked"

// graphShareTokenPrefix starts the token Graph's /shares route takes for a
// sharing link (internal/graph's encodeSharingURL).
const graphShareTokenPrefix = "u!"

// An identifier is masked in two ways. By its position, wherever the
// product's output puts one, whatever its shape: a shared selector
// (internal/sharedref) in text and with its colons as underscores in a state
// database's file name (internal/config), the path segments of a Graph URL
// and of a route printed without a scheme, as a request's debug record and a
// transport error name one (internal/graph's routes), a JSON field and a log
// attribute whose key names an identifier, the drive of a throttle scope key
// (internal/sync's throttle:target:drive:<id>, and drive:<id> alone as a
// throttle target), stat's "ID:" line, drive list's "shared by" label, the
// ID column of recycle-bin list's table and the argument of recycle-bin
// restore in a command line (internal/cli). And by its shape, wherever it
// stands in free text, an error's included (maskIdentifierShapes): a
// Personal item ID, a Business item ID and a Business drive ID, which also
// masks a child mount ID through its item ID. A bare Personal drive ID
// outside those positions is left alone: its shape, 16 hex digits, is that of
// any hex or decimal word of that length, such as a file name a test makes.
// So is, outside them, a Personal item ID whose drive part has lost more than
// four leading zeros, which leaves it shorter than the shape's 12 hex digits.
// The patterns run only on text that holds their marker, and URLs, routes,
// JSON fields, log attributes and shapes, which a --debug log is full of, are
// found by scans linear in the length of the text.
var (
	selectorPattern         = regexp.MustCompile(`shared:([^:\s"'=,;()\[\]{}<>]+):([A-Za-z0-9!%_~+-]+):([A-Za-z0-9!%_~+-]+)`)
	fileNameSelectorPattern = regexp.MustCompile(
		`shared_([A-Za-z0-9]+(?:-[A-Za-z0-9]+)*-account|[^@\s/"']+@[^_\s/"']+)_([A-Za-z0-9!%~+_-]+)`)
	statIDLinePattern    = regexp.MustCompile(`(?m)^(ID:[ \t]+)(\S+)`)
	sharedByPattern      = regexp.MustCompile(`shared by ([^()\n]+)`)
	throttleDrivePattern = regexp.MustCompile(`(?:^|[\s="':])drive:([A-Za-z0-9!%_~+-]+)`)
	restoreArgPattern    = regexp.MustCompile(`recycle-bin restore ([^\s\]"']+)`)
	// recycleBinHeaderPattern is the header of recycle-bin list's table,
	// whose last column is the item ID (internal/cli's printRecycleBinTable).
	recycleBinHeaderPattern = regexp.MustCompile(`(?m)^NAME +SIZE +TYPE +DELETED +ID *$`)
)

// The shapes Graph gives the identifiers of both account kinds.
const (
	// businessDriveIDPrefix starts a Business drive ID, before its base64url
	// part; a route spells it encodedBusinessDriveIDPrefix (url.PathEscape).
	businessDriveIDPrefix        = "b!"
	encodedBusinessDriveIDPrefix = "b" + percentEncodedBang
	// personalItemIDSeparator joins a Personal item ID's drive part, the
	// drive ID in hex, to its item part; a route spells it percentEncodedBang.
	personalItemIDSeparator = "!"
	// percentEscapeLength is the length of a percent-encoded byte, %2F.
	percentEscapeLength = len("%00")
	// businessItemIDPrefix starts a Business item ID, before its
	// businessItemIDTailLength base32 characters.
	businessItemIDPrefix     = "01"
	businessItemIDTailLength = 32
)

// graphIdentifierSegments are the Graph routes whose next path segment is
// an identifier: a drive, an item, a share token, a site or a user.
func graphIdentifierSegments() []string {
	return []string{"drives", "items", "shares", "sites", "users"}
}

// maskIdentifierPositions masks every identifier at those positions to
// maskedIdentifier unless keep reports that it is already a role; no role
// keep reports is longer than longestRole. Names and paths are not at
// them: a JSON "name", "path" or "display_name" is left as it is (a test's
// shared root is named by its own folder) unless it starts as one of the
// routes of graphIdentifierSegments does, and so is the path part of a
// path-addressed Graph URL.
func maskIdentifierPositions(text string, keep func(string) bool, longestRole int) string {
	mask := func(value string) string {
		if value == "" || keep(value) {
			return value
		}
		return maskedIdentifier
	}

	if strings.Contains(text, "shared:") {
		text = selectorPattern.ReplaceAllStringFunc(text, func(match string) string {
			parts := selectorPattern.FindStringSubmatch(match)
			return "shared:" + mask(parts[1]) + ":" + mask(parts[2]) + ":" + mask(parts[3])
		})
	}
	if strings.Contains(text, "shared_") {
		text = fileNameSelectorPattern.ReplaceAllStringFunc(text, func(match string) string {
			parts := fileNameSelectorPattern.FindStringSubmatch(match)
			return "shared_" + mask(parts[1]) + "_" + maskedIdentifier
		})
	}
	text = maskURLs(text, mask)
	text = maskJSONFields(text, mask)
	text = maskLogAttributes(text, mask)
	if strings.Contains(text, "ID:") {
		text = statIDLinePattern.ReplaceAllStringFunc(text, func(match string) string {
			parts := statIDLinePattern.FindStringSubmatch(match)
			return parts[1] + mask(parts[2])
		})
	}
	if strings.Contains(text, "shared by ") {
		text = sharedByPattern.ReplaceAllStringFunc(text, func(match string) string {
			owner := strings.TrimSpace(sharedByPattern.FindStringSubmatch(match)[1])
			return "shared by " + mask(owner)
		})
	}
	if strings.Contains(text, "drive:") {
		text = maskGroups(text, throttleDrivePattern, mask)
	}
	if strings.Contains(text, "recycle-bin restore ") {
		text = maskGroups(text, restoreArgPattern, mask)
	}
	if strings.Contains(text, "DELETED") {
		text = maskRecycleBinIDs(text, mask)
	}

	return maskIdentifierShapes(text, mask, longestRole)
}

// maskIdentifierShapes masks every word of an ID's shape wherever it stands.
// A word neither starts nor ends next to another letter or digit
// (isASCIIAlnum), and a percent-encoded byte before it ends the word before
// it (startsWord), so a shape inside a longer word is left alone. Each ! may
// be percent-encoded, as a route spells it.
//   - A Business drive ID: b! and its base64url part.
//   - A Personal item ID: its drive part, 12 to 16 hex digits (Graph drops a
//     drive ID's leading zeros at times), or a role Redact wrote for a
//     known drive there; then !; then its item part, which may hold a role
//     Redact wrote inside it.
//   - A Business item ID: 01 and 32 letters or digits (base32 as Graph sends
//     it).
//
// A child mount ID, <parent>|binding:<item ID>, is masked through its item
// ID. A word of such a shape that is no identifier is masked too, which
// costs a failure message nothing it needs.
func maskIdentifierShapes(text string, mask func(string) string, longestRole int) string {
	for _, prefix := range []string{businessDriveIDPrefix, encodedBusinessDriveIDPrefix} {
		if strings.Contains(text, prefix) {
			text = maskBusinessDriveIDs(text, prefix, mask)
		}
	}
	for _, separator := range []string{personalItemIDSeparator, percentEncodedBang} {
		if strings.Contains(text, separator) {
			text = maskPersonalItemIDs(text, separator, mask, longestRole)
		}
	}
	if strings.Contains(text, businessItemIDPrefix) {
		text = maskBusinessItemIDs(text, mask)
	}

	return text
}

// startsWord reports whether a word starts at start: at the start of text,
// after a byte that is no letter or digit, or right after a percent-encoded
// byte (%2F, %3A), which ends the word before it.
func startsWord(text string, start int) bool {
	if start == 0 || !isASCIIAlnum(text[start-1]) {
		return true
	}

	return start >= percentEscapeLength && text[start-percentEscapeLength] == '%' && isHex(text[start-2:start])
}

// maskBusinessDriveIDs masks every prefix, b! or its encoded spelling, that
// starts a word, with the base64url run after it.
func maskBusinessDriveIDs(text, prefix string, mask func(string) string) string {
	var edits []spanEdit
	for searchFrom := 0; ; {
		found := strings.Index(text[searchFrom:], prefix)
		if found < 0 {
			break
		}
		start := searchFrom + found
		end := start + len(prefix)
		for end < len(text) && isBase64URLByte(text[end]) {
			end++
		}
		if end == start+len(prefix) || !startsWord(text, start) {
			// The run may end in the b of the next prefix.
			searchFrom = start + len(prefix)
			continue
		}
		searchFrom = end
		if value := text[start:end]; mask(value) != value {
			edits = append(edits, spanEdit{start: start, end: end, text: maskedIdentifier})
		}
	}

	return applySpanEdits(text, edits)
}

// maskPersonalItemIDs masks every Personal item ID whose parts separator, !
// or its encoded spelling, joins: an item part after it and a drive part
// before it (personalDrivePartStart).
func maskPersonalItemIDs(text, separator string, mask func(string) string, longestRole int) string {
	var edits []spanEdit
	editedTo := 0
	for searchFrom := 0; ; {
		found := strings.Index(text[searchFrom:], separator)
		if found < 0 {
			break
		}
		separatorAt := searchFrom + found
		itemStart := separatorAt + len(separator)
		end := itemStart
		for end < len(text) && (isASCIIAlnum(text[end]) || text[end] == '-') {
			end++
		}
		searchFrom = end
		if end == itemStart {
			continue
		}
		start, isItemID := personalDrivePartStart(text, editedTo, separatorAt, longestRole, mask)
		if !isItemID {
			continue
		}
		if value := text[start:end]; mask(value) != value {
			edits = append(edits, spanEdit{start: start, end: end, text: maskedIdentifier})
			editedTo = end
		}
	}

	return applySpanEdits(text, edits)
}

// personalDrivePartStart is where the drive part of a Personal item ID whose
// separator is at separator starts, looking no further back than floor: the
// word before the separator (startsWord) when it is 12 to 16 hex digits,
// else the longest hyphen-joined tail of the run before it that is a role (a
// value mask keeps), which is no longer than longestRole. It reports false
// when there is neither.
func personalDrivePartStart(text string, floor, separator, longestRole int, mask func(string) string) (int, bool) {
	runStart := separator
	for runStart > floor && !startsWord(text, runStart) {
		runStart--
	}
	hexLength := separator - runStart
	if hexLength >= minUnpaddedDriveIDLength && hexLength <= graphDriveIDLength && isHex(text[runStart:separator]) {
		return runStart, true
	}

	for runStart > floor && (text[runStart-1] == '-' || !startsWord(text, runStart)) {
		runStart--
	}
	for start := max(runStart, separator-longestRole); start < separator; start++ {
		if start > runStart && text[start-1] != '-' {
			continue
		}
		if part := text[start:separator]; mask(part) == part {
			return start, true
		}
	}

	return 0, false
}

// maskBusinessItemIDs masks every word that is 01 and
// businessItemIDTailLength letters or digits.
func maskBusinessItemIDs(text string, mask func(string) string) string {
	var edits []spanEdit
	idLength := len(businessItemIDPrefix) + businessItemIDTailLength
	for searchFrom := 0; ; {
		found := strings.Index(text[searchFrom:], businessItemIDPrefix)
		if found < 0 {
			break
		}
		start := searchFrom + found
		end := start
		for end < len(text) && isASCIIAlnum(text[end]) {
			end++
		}
		searchFrom = end
		if end-start != idLength || !startsWord(text, start) {
			continue
		}
		if value := text[start:end]; mask(value) != value {
			edits = append(edits, spanEdit{start: start, end: end, text: maskedIdentifier})
		}
	}

	return applySpanEdits(text, edits)
}

func isASCIIAlnum(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isBase64URLByte(b byte) bool {
	return isASCIIAlnum(b) || b == '-' || b == '_'
}

// maskGroups masks, in each match of pattern in text, the first of its
// groups that matched.
func maskGroups(text string, pattern *regexp.Regexp, mask func(string) string) string {
	var edits []spanEdit
	for _, match := range pattern.FindAllStringSubmatchIndex(text, -1) {
		for group := 2; group+1 < len(match); group += 2 {
			start, end := match[group], match[group+1]
			if start < 0 {
				continue
			}
			if value := text[start:end]; mask(value) != value {
				edits = append(edits, spanEdit{start: start, end: end, text: maskedIdentifier})
			}
			break
		}
	}

	return applySpanEdits(text, edits)
}

// maskRecycleBinIDs masks the last field of every row of each recycle-bin
// list table in text: the item ID. A table's rows run from its header to the
// first empty line.
func maskRecycleBinIDs(text string, mask func(string) string) string {
	var edits []spanEdit
	for _, header := range recycleBinHeaderPattern.FindAllStringIndex(text, -1) {
		for rowStart := header[1] + 1; rowStart < len(text); {
			rowEnd := len(text)
			if newline := strings.IndexByte(text[rowStart:], '\n'); newline >= 0 {
				rowEnd = rowStart + newline
			}
			row := strings.TrimRight(text[rowStart:rowEnd], " \t\r")
			if row == "" {
				break
			}
			idStart := rowStart + strings.LastIndexAny(row, " \t") + 1
			if id := text[idStart : rowStart+len(row)]; mask(id) != id {
				edits = append(edits, spanEdit{start: idStart, end: rowStart + len(row), text: maskedIdentifier})
			}
			rowStart = rowEnd + 1
		}
	}

	return applySpanEdits(text, edits)
}

// spanEdit is a span of text a scan replaces.
type spanEdit struct {
	start, end int
	text       string
}

// applySpanEdits replaces each span of edits, in order and not overlapping.
func applySpanEdits(text string, edits []spanEdit) string {
	if len(edits) == 0 {
		return text
	}

	var edited strings.Builder
	last := 0
	for _, edit := range edits {
		edited.WriteString(text[last:edit.start])
		edited.WriteString(edit.text)
		last = edit.end
	}
	edited.WriteString(text[last:])

	return edited.String()
}

// maskURLs masks the identifier segments of every http or https URL in text,
// and of every Graph route printed without a scheme: a / that opens a word
// (at the start of the text, or after a space, =, : or a quote) followed by
// one of graphIdentifierSegments.
func maskURLs(text string, mask func(string) string) string {
	var edits []spanEdit
	for searchFrom := 0; ; {
		found := strings.IndexByte(text[searchFrom:], '/')
		if found < 0 {
			break
		}
		slash := searchFrom + found
		start, pathStart, isURL := urlAt(text, slash)
		if !isURL {
			searchFrom = slash + 1
			continue
		}
		end := pathStart
		for end < len(text) && !strings.ContainsRune(" \t\r\n\"'<>", rune(text[end])) {
			end++
		}
		searchFrom = end
		if masked := maskURL(text[start:end], pathStart-start, mask); masked != text[start:end] {
			edits = append(edits, spanEdit{start: start, end: end, text: masked})
		}
	}

	return applySpanEdits(text, edits)
}

// urlAt is where the URL or route whose first / is at slash starts, and where
// its path, which maskURL reads, starts: an http or https URL at its scheme,
// its path after ://, and a route at the /. It reports false for any other /.
func urlAt(text string, slash int) (start, pathStart int, isURL bool) {
	if strings.HasPrefix(text[slash:], "//") && strings.HasSuffix(text[:slash], ":") {
		for _, scheme := range []string{"https:", "http:"} {
			if strings.HasSuffix(text[:slash], scheme) {
				return slash - len(scheme), slash + len("//"), true
			}
		}

		return 0, 0, false
	}

	if slash > 0 && strings.IndexByte(" \t\r\n=:\"'", text[slash-1]) < 0 {
		return 0, 0, false
	}
	isRoute := slices.ContainsFunc(graphIdentifierSegments(), func(segment string) bool {
		return strings.HasPrefix(text[slash+1:], segment+"/")
	})

	return slash, slash, isRoute
}

// maskURL masks the segment after each Graph identifier route of a URL whose
// path starts at pathStart, and leaves the path a path-addressed URL names
// (/root:/folder/file: or /items/<id>:/name:) as it is.
func maskURL(rawURL string, pathStart int, mask func(string) string) string {
	var edits []spanEdit
	for i := pathStart; i < len(rawURL); i++ {
		switch {
		case rawURL[i] == ':' && i+1 < len(rawURL) && rawURL[i+1] == '/':
			// A path address runs to its closing colon.
			if closing := strings.IndexByte(rawURL[i+1:], ':'); closing >= 0 {
				i += closing + 1
			} else {
				i = len(rawURL)
			}
		case rawURL[i] == '/':
			routeEnd := strings.IndexByte(rawURL[i+1:], '/')
			if routeEnd < 0 || !slices.Contains(graphIdentifierSegments(), rawURL[i+1:i+1+routeEnd]) {
				continue
			}
			segmentStart := i + 1 + routeEnd + 1
			segmentEnd := segmentStart
			for segmentEnd < len(rawURL) && !strings.ContainsRune("/?#:", rune(rawURL[segmentEnd])) {
				segmentEnd++
			}
			if segment := rawURL[segmentStart:segmentEnd]; segment != "root" && mask(segment) != segment {
				edits = append(edits, spanEdit{start: segmentStart, end: segmentEnd, text: maskedIdentifier})
			}
			i = segmentEnd - 1
		}
	}

	return applySpanEdits(rawURL, edits)
}

// maskJSONFields masks the string value of every JSON field whose key names
// an identifier (isIdentifierKey).
func maskJSONFields(text string, mask func(string) string) string {
	var edits []spanEdit
	for searchFrom := 0; ; {
		found := strings.IndexByte(text[searchFrom:], ':')
		if found < 0 {
			break
		}
		colon := searchFrom + found
		searchFrom = colon + 1

		keyEnd := colon
		for keyEnd > 0 && text[keyEnd-1] == ' ' {
			keyEnd--
		}
		if keyEnd == 0 || text[keyEnd-1] != '"' {
			continue
		}
		keyStart := strings.LastIndexByte(text[:keyEnd-1], '"')
		valueStart := colon + 1
		for valueStart < len(text) && text[valueStart] == ' ' {
			valueStart++
		}
		if keyStart < 0 || valueStart >= len(text) || text[valueStart] != '"' {
			continue
		}
		valueEnd, closed := quotedEnd(text, valueStart)
		if !closed {
			continue
		}
		searchFrom = valueEnd
		if !isIdentifierKey(text[keyStart+1 : keyEnd-1]) {
			continue
		}
		if value := text[valueStart+1 : valueEnd-1]; mask(value) != value {
			edits = append(edits, spanEdit{start: valueStart + 1, end: valueEnd - 1, text: maskedIdentifier})
		}
	}

	return applySpanEdits(text, edits)
}

// quotedEnd is the index after the closing quote of the string quoted at
// start, and whether the string is closed: neither a JSON string nor a
// quoted log value holds a raw newline, so one that reaches the end of its
// line is not, and the scans go on after it. A backslash escapes the byte
// after it.
func quotedEnd(text string, start int) (int, bool) {
	for i := start + 1; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case '"':
			return i + 1, true
		case '\n':
			return i, false
		}
	}

	return len(text), false
}

// maskLogAttributes masks the value of every key=value attribute of a log
// line, at the start of the text or after a space, whose key names an
// identifier (isIdentifierKey). A quoted value runs to its closing quote.
func maskLogAttributes(text string, mask func(string) string) string {
	var edits []spanEdit
	for searchFrom := 0; ; {
		found := strings.IndexByte(text[searchFrom:], '=')
		if found < 0 {
			break
		}
		equals := searchFrom + found
		keyStart := equals
		for keyStart > 0 && isLogKeyByte(text[keyStart-1]) {
			keyStart--
		}
		valueEnd := logValueEnd(text, equals+1)
		searchFrom = max(valueEnd, equals+1)

		atAttribute := keyStart == 0 || strings.ContainsRune(" \t\r\n", rune(text[keyStart-1]))
		if keyStart == equals || !atAttribute || !isIdentifierKey(text[keyStart:equals]) {
			continue
		}
		if value := strings.Trim(text[equals+1:valueEnd], `"`); mask(value) != value {
			edits = append(edits, spanEdit{start: equals + 1, end: valueEnd, text: maskedIdentifier})
		}
	}

	return applySpanEdits(text, edits)
}

func isLogKeyByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// logValueEnd is where the attribute value starting at start ends: after its
// closing quote when it is quoted, at the end of its line when that quote is
// not closed, else at the next space.
func logValueEnd(text string, start int) int {
	if start < len(text) && text[start] == '"' {
		end, _ := quotedEnd(text, start)
		return end
	}

	end := start
	for end < len(text) && !strings.ContainsRune(" \t\r\n", rune(text[end])) {
		end++
	}

	return end
}

// isIdentifierKey reports a JSON or log key whose value names a drive, an
// item, an account or its owner: the keys the CLI's --json output and the
// --debug log use for them. A Graph request ID names none of them, and a run
// ID is the suite's own, so both stay readable.
func isIdentifierKey(key string) bool {
	switch key {
	case "id", "email", "etag", "ctag", "remote_drive",
		"owner_name", "shared_by_name", "shared_owner_name", "org_name", "site_name", "library_name":
		return true
	case "run_id":
		return false
	}
	if strings.HasSuffix(key, "request_id") {
		return false
	}

	return strings.HasSuffix(key, "_id") || strings.HasSuffix(key, "_email") || strings.HasSuffix(key, "_etag")
}
