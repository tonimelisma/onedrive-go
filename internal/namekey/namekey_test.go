package namekey

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.12.3
func TestKey_MatchesProbedOneDriveCollisions(t *testing.T) {
	t.Parallel()

	// Pairs from probes 2, 3, L-03, L-30 and L-33 on OneDrive Personal: both
	// names were created in one folder with conflictBehavior=fail; collide
	// means the second create answered 409 nameAlreadyExists.
	tests := []struct {
		name    string
		a, b    string
		collide bool
	}{
		{name: "ascii case", a: "Todo.md", b: "TODO.md", collide: true},
		{name: "NFC and NFD", a: "café.txt", b: "café.txt", collide: true},
		{name: "sharp s full folding", a: "straße.txt", b: "STRASSE.txt", collide: true},
		{name: "dotted capital I", a: "i̇stanbul", b: "İstanbul", collide: true},
		{name: "sigma", a: "σ.txt", b: "Σ.txt", collide: true},
		{name: "final sigma", a: "ς.txt", b: "σ.txt", collide: true},
		{name: "titlecase digraph", a: "ǅ.txt", b: "ǆ.txt", collide: true},
		{name: "fi ligature", a: "ﬁle", b: "FILE", collide: true},
		{name: "any two emoji", a: "\U0001F600.txt", b: "\U0001F601.txt", collide: true},
		{name: "repeated emoji", a: "\U0001F600.txt", b: "\U0001F600\U0001F600.txt", collide: true},
		{name: "emoji is ignored", a: "a\U0001F600.txt", b: "a.txt", collide: true},
		{name: "CJK extension B", a: "\U00020000.txt", b: "\U00020001.txt", collide: true},
		{name: "ZWJ sequence", a: "\U0001F468\u200D\U0001F469.txt", b: "\U0001F468.txt", collide: true},
		{name: "variation selector 16", a: "x️.txt", b: "x.txt", collide: true},
		{name: "Devanagari exclusion", a: "क़.txt", b: "क़.txt", collide: true},
		{name: "Kelvin sign", a: "K.txt", b: "K.txt", collide: false},
		{name: "compatibility ideograph", a: "猪.txt", b: "猪.txt", collide: false},
		{name: "micro sign", a: "µ.txt", b: "Μ.txt", collide: false},
		{name: "Hebrew presentation form", a: "שׁ.txt", b: "שׁ.txt", collide: false},
		{name: "Greek oxia singleton", a: "ά.txt", b: "ά.txt", collide: false},
		{name: "long s", a: "ſ.txt", b: "s.txt", collide: false},
		{name: "beta symbol", a: "ϐ.txt", b: "β.txt", collide: false},
		{name: "ZWNJ", a: "x\u200C.txt", b: "x.txt", collide: false},
		{name: "BMP symbols", a: "☃.txt", b: "☄.txt", collide: false},
		{name: "zero-width space", a: "a\u200Bb.txt", b: "ab.txt", collide: false},
		{name: "soft hyphen", a: "a\u00ADb.txt", b: "ab.txt", collide: false},
		{name: "different names", a: "a.txt", b: "b.txt", collide: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.collide, Equal(tt.a, tt.b), "Key(%q)=%q Key(%q)=%q", tt.a, Key(tt.a), tt.b, Key(tt.b))
		})
	}
}

// Validates: R-2.12.3
func TestKey_MatchesTheSystematicProbe(t *testing.T) {
	t.Parallel()

	// Every pair of probe M-1 (testdata/probe-m1-personal.tsv): code points
	// where Unicode case folding and simple lowercasing differ, the
	// composition exclusions, the canonical singletons and the default
	// ignorables, each created as "pNNNN-<a>.t" and "pNNNN-<b>.t".
	f, err := os.Open("testdata/probe-m1-personal.tsv")
	require.NoError(t, err)
	defer func() { assert.NoError(t, f.Close()) }()

	decode := func(field string) string {
		if field == "(empty)" {
			return ""
		}
		var b strings.Builder
		for _, cp := range strings.Fields(field) {
			v, parseErr := strconv.ParseUint(strings.TrimPrefix(cp, "U+"), 16, 32)
			require.NoError(t, parseErr)
			b.WriteRune(rune(v))
		}
		return b.String()
	}

	pairs := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		require.Len(t, fields, 4, line)
		a := "p-" + decode(fields[1]) + ".t"
		b := "p-" + decode(fields[2]) + ".t"
		assert.Equal(t, fields[3] == "SAME", Equal(a, b), "%s %s vs %s (OneDrive: %s)", fields[0], fields[1], fields[2], fields[3])
		pairs++
	}
	require.NoError(t, scanner.Err())
	assert.Greater(t, pairs, 500)
}

// Validates: R-2.12.3
func TestKey_ASCIIFastPathAgreesWithFolding(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "readme.md", Key("README.md"))
	assert.Equal(t, "already-lower.txt", Key("already-lower.txt"))
	assert.Equal(t, Key("x.txt"), Key("X️.txt"), "an ignored character must not change the folding path")
	assert.Equal(t, Key("README.md")+"é", Key("README.mdÉ"))
}

// Validates: R-2.12.3
func TestKey_InvalidUTF8DoesNotPanic(t *testing.T) {
	t.Parallel()

	assert.NotPanics(t, func() { Key("caf\xe9.txt") })
}

// Validates: R-2.12.3
func TestEqualPath_ComparesEverySegment(t *testing.T) {
	t.Parallel()

	assert.True(t, EqualPath("Docs/STRASSE.txt", "docs/straße.txt"))
	assert.True(t, EqualPath("a\U0001F600/x.txt", "a/X.txt"))
	assert.False(t, EqualPath("Docs/a.txt", "Docs/b.txt"))
	assert.False(t, EqualPath("Docs/a.txt", "Docs/sub/a.txt"))
	assert.False(t, EqualPath("a/b", "ab"))
}

// Validates: R-2.12.3
func TestDiffersInIgnoredCharacters(t *testing.T) {
	t.Parallel()

	assert.True(t, DiffersInIgnoredCharacters("a\U0001F600.txt", "a.txt"))
	assert.False(t, DiffersInIgnoredCharacters("STRASSE.txt", "straße.txt"))
}
