package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- parseLine tests ---

func TestParseLine_Blank(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"spaces", "   "},
		{"tab", "\t"},
		{"mixed whitespace", "  \t  "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pl := parseLine(tt.raw)
			assert.Equal(t, lineBlank, pl.kind)
			assert.Equal(t, tt.raw, pl.raw)
		})
	}
}

func TestParseLine_Comment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{"bare hash", "# comment"},
		{"indented", "  # indented comment"},
		{"no space after hash", "#nospace"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pl := parseLine(tt.raw)
			assert.Equal(t, lineComment, pl.kind)
			assert.Equal(t, tt.raw, pl.raw)
		})
	}
}

func TestParseLine_Section(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		raw         string
		sectionName string
	}{
		{"simple", `[global]`, "global"},
		{"quoted", `["personal:toni@outlook.com"]`, "personal:toni@outlook.com"},
		{"whitespace around", `  ["business:alice@contoso.com"]  `, "business:alice@contoso.com"},
		{"inner spaces", `[ "personal:user@example.com" ]`, "personal:user@example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pl := parseLine(tt.raw)
			assert.Equal(t, lineSection, pl.kind)
			assert.Equal(t, tt.sectionName, pl.sectionName)
			assert.Equal(t, tt.raw, pl.raw)
		})
	}
}

func TestParseLine_ArrayOfTables_IsOther(t *testing.T) {
	t.Parallel()

	pl := parseLine("[[servers]]")
	assert.Equal(t, lineOther, pl.kind)
}

func TestParseLine_KeyValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		raw           string
		key           string
		value         string
		inlineComment string
	}{
		{"standard", `sync_dir = "~/OneDrive"`, "sync_dir", `"~/OneDrive"`, ""},
		{"no spaces", `paused=true`, "paused", "true", ""},
		{"extra spaces", `  sync_dir  =  "~/OneDrive"  `, "sync_dir", `"~/OneDrive"`, ""},
		{"boolean", `paused = true`, "paused", "true", ""},
		{"inline comment", `paused = true # temporarily`, "paused", "true", "# temporarily"},
		{"hash in quoted value", `sync_dir = "~/path#with#hash"`, "sync_dir", `"~/path#with#hash"`, ""},
		{"hash in quoted value with inline comment", `sync_dir = "~/path#hash" # real comment`, "sync_dir", `"~/path#hash"`, "# real comment"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pl := parseLine(tt.raw)
			assert.Equal(t, lineKeyValue, pl.kind, "kind")
			assert.Equal(t, tt.key, pl.key, "key")
			assert.Equal(t, tt.value, pl.value, "value")
			assert.Equal(t, tt.inlineComment, pl.inlineComment, "inlineComment")
			assert.Equal(t, tt.raw, pl.raw, "raw preserved")
		})
	}
}

func TestParseLine_CommentedOutKey_IsComment(t *testing.T) {
	t.Parallel()

	// Commented-out config lines start with # and should be classified as comments.
	pl := parseLine(`# log_level = "info"`)
	assert.Equal(t, lineComment, pl.kind)
}

// --- parseLines / renderLines round-trip ---

func TestParseAndRenderLines_RoundTrip(t *testing.T) {
	t.Parallel()

	content := `# onedrive-go configuration
log_level = "info"

# My drive
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
paused = true # temporarily

["business:alice@contoso.com"]
sync_dir = "~/Work"
`

	lines := parseLines(content)
	result := renderLines(lines)
	assert.Equal(t, content, result)
}

func TestParseAndRenderLines_EmptyContent(t *testing.T) {
	t.Parallel()

	lines := parseLines("")
	assert.Len(t, lines, 1) // strings.Split("", "\n") returns [""]
	assert.Empty(t, renderLines(lines))
}

// --- renderKeyValueLine ---

func TestRenderKeyValueLine_WithoutComment(t *testing.T) {
	t.Parallel()

	result := renderKeyValueLine("paused", "true", "")
	assert.Equal(t, "paused = true", result)
}

func TestRenderKeyValueLine_WithComment(t *testing.T) {
	t.Parallel()

	result := renderKeyValueLine("paused", "true", "# temporarily")
	assert.Equal(t, "paused = true # temporarily", result)
}

// --- findSectionByName ---

func TestFindSectionByName_Found(t *testing.T) {
	t.Parallel()

	lines := parseLines(`# header
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
`)

	idx, found := findSectionByName(lines, "personal:toni@outlook.com")
	assert.True(t, found)
	assert.Equal(t, 1, idx)
}

func TestFindSectionByName_NotFound(t *testing.T) {
	t.Parallel()

	lines := parseLines(`["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
`)

	_, found := findSectionByName(lines, "business:alice@contoso.com")
	assert.False(t, found)
}

// --- sectionContentRange ---

func TestSectionContentRange_WithNextSection(t *testing.T) {
	t.Parallel()

	lines := parseLines(`["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"

# Next section
["business:alice@contoso.com"]
sync_dir = "~/Work"`)

	start, end := sectionContentRange(lines, 0)
	assert.Equal(t, 1, start)
	assert.Equal(t, 2, end) // excludes blank line and comment before next section
}

func TestSectionContentRange_AtEOF(t *testing.T) {
	t.Parallel()

	lines := parseLines(`["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
paused = true`)

	start, end := sectionContentRange(lines, 0)
	assert.Equal(t, 1, start)
	assert.Equal(t, 3, end)
}

func TestSectionContentRange_EmptySection(t *testing.T) {
	t.Parallel()

	lines := parseLines(`["personal:toni@outlook.com"]

["business:alice@contoso.com"]`)

	start, end := sectionContentRange(lines, 0)
	assert.Equal(t, 1, start)
	assert.Equal(t, 1, end) // blank line belongs to next section preamble
}

// --- findKeyInRange ---

func TestFindKeyInRange_Found(t *testing.T) {
	t.Parallel()

	lines := parseLines(`["section"]
sync_dir = "~/OneDrive"
paused = true
display_name = "home"`)

	idx, found := findKeyInRange(lines, 1, 4, "paused")
	assert.True(t, found)
	assert.Equal(t, 2, idx)
}

func TestFindKeyInRange_NotFound(t *testing.T) {
	t.Parallel()

	lines := parseLines(`["section"]
sync_dir = "~/OneDrive"`)

	_, found := findKeyInRange(lines, 1, 2, "paused")
	assert.False(t, found)
}

func TestFindKeyInRange_ExactMatchOnly(t *testing.T) {
	t.Parallel()

	// "paused" should NOT match "paused_until" — this is the key bug B-284 fixes.
	lines := parseLines(`["section"]
paused_until = "2026-03-01T00:00:00Z"
paused = true`)

	idx, found := findKeyInRange(lines, 1, 3, "paused")
	assert.True(t, found)
	assert.Equal(t, 2, idx, "should match 'paused' exactly, not 'paused_until'")
}

// --- splitInlineComment ---

func TestSplitInlineComment_NoComment(t *testing.T) {
	t.Parallel()

	value, comment := splitInlineComment(`"~/OneDrive"`)
	assert.Equal(t, `"~/OneDrive"`, value)
	assert.Empty(t, comment)
}

func TestSplitInlineComment_WithComment(t *testing.T) {
	t.Parallel()

	value, comment := splitInlineComment(`true # temporarily`)
	assert.Equal(t, "true", value)
	assert.Equal(t, "# temporarily", comment)
}

func TestSplitInlineComment_HashInsideQuotes(t *testing.T) {
	t.Parallel()

	value, comment := splitInlineComment(`"path#with#hash"`)
	assert.Equal(t, `"path#with#hash"`, value)
	assert.Empty(t, comment)
}

func TestSplitInlineComment_HashInsideQuotesWithRealComment(t *testing.T) {
	t.Parallel()

	value, comment := splitInlineComment(`"path#hash" # real comment`)
	assert.Equal(t, `"path#hash"`, value)
	assert.Equal(t, "# real comment", comment)
}

func TestSplitInlineComment_EscapedQuoteInValue(t *testing.T) {
	t.Parallel()

	value, comment := splitInlineComment(`"escaped\"quote" # comment`)
	assert.Equal(t, `"escaped\"quote"`, value)
	assert.Equal(t, "# comment", comment)
}

// --- parseSectionHeader ---

func TestParseSectionHeader_Simple(t *testing.T) {
	t.Parallel()

	name, ok := parseSectionHeader("[global]")
	require.True(t, ok)
	assert.Equal(t, "global", name)
}

func TestParseSectionHeader_Quoted(t *testing.T) {
	t.Parallel()

	name, ok := parseSectionHeader(`["personal:toni@outlook.com"]`)
	require.True(t, ok)
	assert.Equal(t, "personal:toni@outlook.com", name)
}

func TestParseSectionHeader_NotSection(t *testing.T) {
	t.Parallel()

	_, ok := parseSectionHeader("key = value")
	assert.False(t, ok)
}
