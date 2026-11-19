package namekey

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-2.12.3
func TestOfPath_HasPrefix_SegmentBoundary(t *testing.T) {
	t.Parallel()

	assert.False(t, OfPath("a/bc").HasPrefix(OfPath("a/b")), "a/bc is not below a/b")
	assert.True(t, OfPath("A/B/c").HasPrefix(OfPath("a/b")), "the name rule applies per segment")
	assert.True(t, OfPath("a/b").HasPrefix(OfPath("a/b")), "a path is at its own prefix")
	assert.False(t, OfPath("a").HasPrefix(OfPath("a/b")), "a parent is not below its child")
	assert.False(t, OfPath("b/a").HasPrefix(OfPath("a")))
	// U+00DF LATIN SMALL LETTER SHARP S folds to "ss".
	assert.True(t, OfPath("STRASSE/x").HasPrefix(OfPath("stra\u00dfe")))
	assert.False(t, OfPath("stra\u00dfex/y").HasPrefix(OfPath("strasse")))

	// The empty key is the key of a name OneDrive ignores whole (an emoji
	// alone), not a root: only what lies below that name is below it.
	assert.Equal(t, PathKey(""), OfPath("\U0001F600"))
	assert.False(t, OfPath("a/b").HasPrefix(OfPath("\U0001F600")))
	assert.True(t, OfPath("\U0001F601/b").HasPrefix(OfPath("\U0001F600")))
	assert.True(t, OfPath("").HasPrefix(OfPath("")))

	// Keys of uncleaned paths compare as written: callers clean first.
	assert.NotEqual(t, OfPath("a"), OfPath("a/"))
	assert.NotEqual(t, OfPath("a/b"), OfPath("a//b"))
	assert.False(t, OfPath("a/b").HasPrefix(OfPath("a/")))
}

// Validates: R-2.12.3
func TestOfPath_IsOfPerSegment(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"", "a", "Docs/STRASSE.txt", "a\U0001F600/X.txt", "a/b/c", "/lead", "trail/"} {
		segments := strings.Split(path, "/")
		keys := make([]string, len(segments))
		for i, segment := range segments {
			keys[i] = string(Of(segment))
		}
		assert.Equal(t, PathKey(strings.Join(keys, "/")), OfPath(path), path)
	}
	assert.Equal(t, OfPath("docs/stra\u00dfe.txt"), OfPath("Docs/STRASSE.txt"))
	assert.NotEqual(t, OfPath("a/b/c"), OfPath("ab/c"))
}

// Validates: R-2.12.1, R-2.12.3
func TestOfPath_EqualPathsShareAKey(t *testing.T) {
	t.Parallel()

	variants := []string{"a/edit.txt", "A/EDIT.txt", "a/\u00e9.txt", "a/e\u0301.txt", "a/\u00c9.txt", "a/x\U0001F600.txt", "a/x.txt", "a/b/c", "ab/c", "a/b"}
	for _, x := range variants {
		for _, y := range variants {
			if EqualPath(x, y) {
				assert.Equal(t, OfPath(x), OfPath(y), "%q and %q are one OneDrive path", x, y)
			}
		}
	}
	assert.NotEqual(t, OfPath("a/b/c"), OfPath("ab/c"))
	assert.NotEqual(t, OfPath("a/b"), OfPath("a/b/c"))
}

// relationOfPair is the relation the probe implies for a pair: a collision
// is one key, the stored forms tell the tiers apart.
func relationOfPair(pair probedPair) Relation {
	sameStored := Stored(pair.a) == Stored(pair.b)
	switch {
	case pair.a == pair.b:
		return Identical
	case pair.collide && sameStored:
		return SameStored
	case pair.collide:
		return SameName
	case sameStored:
		return Twin
	default:
		return Unrelated
	}
}

// Validates: R-2.12.3
func TestRelate_ProbeCorpus(t *testing.T) {
	t.Parallel()

	corpus := append(probedPairs(), systematicProbePairs(t)...)
	names := make([]string, 0, 2*len(corpus))
	for _, pair := range corpus {
		assert.Equal(t, relationOfPair(pair), Relate(pair.a, pair.b), "%s: %+q and %+q", pair.name, pair.a, pair.b)
		names = append(names, pair.a, pair.b)
	}

	// The tiers named in each constant's comment.
	assert.Equal(t, Twin, Relate("\u212a", "K"), "U+212A KELVIN SIGN")
	assert.Equal(t, SameName, Relate("Todo.md", "TODO.md"))
	assert.Equal(t, SameStored, Relate(nfdCafe, nfcCafe), "NFD and NFC of e with acute")
	assert.Equal(t, Identical, Relate("a", "a"))
	assert.Equal(t, Unrelated, Relate("a", "b"))

	// Every pair of corpus names, each name against its neighbors: the
	// relation is symmetric and says what holds for the keys and the bytes.
	const window = 8
	for i, a := range names {
		for _, b := range names[i:min(i+window, len(names))] {
			relation := Relate(a, b)
			assert.Equal(t, relation, Relate(b, a), "%+q %+q", a, b)
			sameKey, sameStored := Of(a) == Of(b), Stored(a) == Stored(b)
			switch relation {
			case Identical:
				assert.Equal(t, a, b)
			case SameStored:
				assert.True(t, sameKey && sameStored, "%+q %+q", a, b)
			case SameName:
				assert.True(t, sameKey && !sameStored, "%+q %+q", a, b)
			case Twin:
				assert.True(t, !sameKey && sameStored, "%+q %+q", a, b)
			case Unrelated:
				assert.True(t, !sameKey && !sameStored, "%+q %+q", a, b)
			}
		}
	}
}

// kelvinClass holds characters OneDrive keeps apart from their NFC form:
// each is its own name although this client stores it as another.
func kelvinClass() []string {
	return []string{
		"\u212a", // KELVIN SIGN, stored as K
		"\u2126", // OHM SIGN, stored as GREEK CAPITAL LETTER OMEGA
		"\ufa16", // CJK COMPATIBILITY IDEOGRAPH-FA16, stored as U+732A
		"\u1f71", // GREEK SMALL LETTER ALPHA WITH OXIA, stored with tonos
		"\ufb2a", // HEBREW LETTER SHIN WITH SHIN DOT, stored decomposed
	}
}

// Validates: R-2.12.3, R-2.13.1
func TestOf_StoredComposition_KelvinClass(t *testing.T) {
	t.Parallel()

	for _, x := range kelvinClass() {
		name := "a" + x + ".txt"
		assert.NotEqual(t, name, Stored(name), "%+q", name)
		assert.NotEqual(t, Of(name), Of(Stored(name)), "%+q", name)
		assert.Equal(t, Twin, Relate(name, Stored(name)), "%+q", name)
	}
}

// Validates: R-2.13.1
func TestStoredPath_EqualsPerSegment(t *testing.T) {
	t.Parallel()

	perSegment := func(p string) string {
		segments := strings.Split(p, "/")
		for i, segment := range segments {
			segments[i] = Stored(segment)
		}
		return strings.Join(segments, "/")
	}

	paths := []string{
		"",
		"/",
		"one",
		"/leading",
		"trailing/",
		"a//b",
		"\u0301starts-with-a-combining-mark/x", // COMBINING ACUTE ACCENT first
		"ends-with-e/\u0301",                   // a mark alone after the slash
		"e/\u0301e",                            // the mark follows the slash, not the e
		"cafe\u0301/cafe\u0301.txt",            // NFD names
		"\u1100/\u1161",                        // HANGUL CHOSEONG KIYEOK / JUNGSEONG A
		"\u212a/\ufb2a/\u2126",                 // the Kelvin class
		"a\U0001F600/\u0958/\u00df",            // emoji, DEVANAGARI QA, SHARP S
		strings.Repeat("o\u0308/", 3) + "\u0308tail", // DIAERESIS after each slash
	}
	pairs := append(probedPairs(), systematicProbePairs(t)...)
	for _, pair := range pairs {
		paths = append(paths, pair.a+"/"+pair.b, pair.b+"/"+pair.a)
	}
	for _, p := range paths {
		assert.Equal(t, perSegment(p), StoredPath(p), "%+q", p)
	}
	assert.Greater(t, len(paths), 1000)
}

// The two spellings of one name: NFD (e and COMBINING ACUTE ACCENT) and NFC
// (LATIN SMALL LETTER E WITH ACUTE).
const (
	nfdCafe = "cafe\u0301"
	nfcCafe = "caf\u00e9"
)

type listed struct {
	name      string
	candidate bool
}

func listedName(e *listed) (string, bool) { return e.name, e.candidate }

// Validates: R-2.12.3
func TestFind_TieBreak(t *testing.T) {
	t.Parallel()

	nfd := nfdCafe
	nfc := nfcCafe
	upper := "CAF\u00c9" // NFC, upper case

	tests := []struct {
		name  string
		items []listed
		want  string
		match Match
	}{
		{name: "empty listing", want: nfc, match: Match{Index: -1, Relation: Unrelated}},
		{
			name:  "identical beats same stored and same name",
			items: []listed{{upper, true}, {nfd, true}, {nfc, true}},
			want:  nfc, match: Match{Index: 2, Relation: Identical},
		},
		{
			name:  "same stored beats same name",
			items: []listed{{upper, true}, {nfd, true}},
			want:  nfc, match: Match{Index: 1, Relation: SameStored},
		},
		{
			name:  "one same name",
			items: []listed{{"other", true}, {upper, true}},
			want:  nfc, match: Match{Index: 1, Relation: SameName},
		},
		{
			name:  "two same names are ambiguous",
			items: []listed{{upper, true}, {"Caf\u00c9", true}},
			want:  nfc, match: Match{Index: -1, Relation: SameName, Ambiguous: true},
		},
		{
			name:  "a better tier settles an ambiguous lower one",
			items: []listed{{upper, true}, {"Caf\u00c9", true}, {nfd, true}},
			want:  nfc, match: Match{Index: 2, Relation: SameStored},
		},
		{
			name:  "a twin is never returned",
			items: []listed{{"K", true}},
			want:  "\u212a", match: Match{Index: -1, Relation: Unrelated}, // KELVIN SIGN
		},
		{
			name:  "a twin beside a same name",
			items: []listed{{"\u212a", true}, {"k", true}}, // KELVIN SIGN
			want:  "K", match: Match{Index: 1, Relation: SameName},
		},
		{
			name:  "a refused entry is never returned",
			items: []listed{{nfc, false}, {upper, true}},
			want:  nfc, match: Match{Index: 1, Relation: SameName},
		},
		{
			name:  "nothing related",
			items: []listed{{"a", true}, {"b", true}},
			want:  nfc, match: Match{Index: -1, Relation: Unrelated},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Find(tt.items, tt.want, listedName)
			assert.Equal(t, tt.match, got)
			assert.Equal(t, tt.match.Index >= 0, got.Found())
		})
	}
}

// Validates: R-2.12.3, R-2.13.2
func TestFindStored_TwoEntriesAreAmbiguous(t *testing.T) {
	t.Parallel()

	entryName := func(s *string) string { return *s }
	nfd := nfdCafe
	nfc := nfcCafe

	assert.Equal(t, Match{Index: -1, Relation: Unrelated}, FindStored([]string{}, nfc, entryName), "no entry")
	assert.Equal(t, Match{Index: -1, Relation: Unrelated}, FindStored([]string{"CAF\u00c9"}, nfc, entryName),
		"another stored form")
	assert.Equal(t, Match{Index: 1, Relation: SameStored}, FindStored([]string{"x", nfd}, nfc, entryName), "one entry")
	assert.Equal(t, Match{Index: 0, Relation: Identical}, FindStored([]string{nfc}, nfc, entryName))
	assert.Equal(t, Match{Index: 0, Relation: Twin}, FindStored([]string{"\u212a"}, "K", entryName),
		"KELVIN SIGN is stored as K")

	got := FindStored([]string{nfd, "x", nfc}, nfc, entryName)
	assert.True(t, got.Ambiguous, "two entries of one stored form are a local collision")
	assert.Equal(t, -1, got.Index)
	assert.False(t, got.Found())
}
