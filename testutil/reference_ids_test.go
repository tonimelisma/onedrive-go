package testutil

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The reference docs quote Graph's answers, and the repository is public: a
// drive or item ID they quote is an invented one of the same shape, never one
// of a test account. These are the invented ones; a finding names its file
// and line, never the value.

// inventedReferenceDrives are the invented Personal drive IDs the reference
// docs quote, in either case.
func inventedReferenceDrives() []string {
	return []string{
		"0123456789abcd00", "0123456789abcd01", "0123456789abcd02", "0123456789abcd03", "0123456789abcd04",
	}
}

var (
	// referenceItemIDPattern is a Personal item ID: its drive part, 12 to 16
	// hex digits, ! or %21, and its item part.
	referenceItemIDPattern = regexp.MustCompile(`(?:^|[^A-Za-z0-9])([0-9A-Fa-f]{12,16})(?:!|%21)([A-Za-z0-9-]+)`)
	// referenceDriveIDPattern is a bare Personal drive ID: 16 hex digits as
	// a word of its own.
	referenceDriveIDPattern = regexp.MustCompile(`(?:^|[^A-Za-z0-9!])([0-9A-Fa-f]{16})(?:$|[^A-Za-z0-9!])`)
	// referenceBusinessIDPattern is a Business drive ID (b! and its base64url
	// part) or a Business item ID (01 and 32 letters or digits).
	referenceBusinessIDPattern = regexp.MustCompile(
		`(?:^|[^A-Za-z0-9])(b(?:!|%21)[A-Za-z0-9_-]{8,}|01[A-Z0-9]{32})(?:$|[^A-Za-z0-9])`)
	// inventedItemPartPattern is the item part of an invented item ID.
	inventedItemPartPattern = regexp.MustCompile(`^invented\d{3}x*$`)
)

func isInventedReferenceDrive(driveID string) bool {
	return slices.Contains(inventedReferenceDrives(), strings.ToLower(driveID))
}

// referenceIDFindings lists, as "<name>:<line>", every line of text that
// quotes a drive or item ID that is not one of the invented ones.
func referenceIDFindings(name, text string) []string {
	var findings []string
	for number, line := range strings.Split(text, "\n") {
		quotesLiveShape := false
		for _, match := range referenceItemIDPattern.FindAllStringSubmatch(line, -1) {
			if !isInventedReferenceDrive(match[1]) || !inventedItemPartPattern.MatchString(match[2]) {
				quotesLiveShape = true
			}
		}
		for _, match := range referenceDriveIDPattern.FindAllStringSubmatch(line, -1) {
			if !isInventedReferenceDrive(match[1]) {
				quotesLiveShape = true
			}
		}
		if referenceBusinessIDPattern.MatchString(line) {
			quotesLiveShape = true
		}
		if quotesLiveShape {
			findings = append(findings, name+":"+strconv.Itoa(number+1))
		}
	}

	return findings
}

// Validates: R-6.10.27
//
// No reference doc quotes a drive or item ID other than the invented ones,
// so a Graph answer pasted from a live run cannot bring a test account's IDs
// into the public tree. Planted rows prove the rule fails a Personal item
// ID, its percent-encoded spelling, a bare drive ID and a Business ID that
// are not invented, and passes the invented ones and hex words of other
// lengths.
func TestReferenceDocsQuoteOnlyInventedIDs(t *testing.T) {
	t.Parallel()

	paths, err := filepath.Glob(filepath.Join("..", "spec", "reference", "*.md"))
	require.NoError(t, err)
	require.NotEmpty(t, paths, "the reference docs moved: follow them")
	for _, path := range paths {
		data, readErr := os.ReadFile(filepath.Clean(path))
		require.NoError(t, readErr)
		for _, finding := range referenceIDFindings(filepath.Base(path), string(data)) {
			assert.Failf(t, "a reference doc quotes a drive or item ID that is not invented",
				"%s: replace it with an invented ID of the same shape and list it in inventedReferenceDrives", finding)
		}
	}

	planted := strings.Join([]string{
		"GET /drives/0123456789ABCD00/items/0123456789ABCD00!invented000xxxxxxxx",
		"item FEDCBA9876543210!s5e0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c",
		"route /items/FEDCBA9876543210%2112345",
		"drive fedcba9876543210 answered",
		"drive b!Zm9vYmFyYmF6cXV4 answered",
		"commit 0123456789abcdef0123 and hash 0123abcd",
		"item 01ABCDEFGHIJKLMNOPQRSTUVWXYZ012345",
	}, "\n")
	assert.Equal(t, []string{"planted.md:2", "planted.md:3", "planted.md:4", "planted.md:5", "planted.md:7"},
		referenceIDFindings("planted.md", planted))
}
