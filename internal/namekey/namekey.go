// Package namekey derives the key under which OneDrive treats two item names
// in one folder as the same name.
//
// OneDrive refuses (or, with conflictBehavior=replace, overwrites) a second
// item whose name it considers equal to an existing sibling's. Every place
// that groups, collides or looks up names on the client's side must use the
// same equivalence, or two local files map onto one remote item and the
// second upload replaces the first.
//
// OneDrive's equivalence is not a Unicode standard form (it behaves like a
// collation). The rule below is reconstructed from live probes on OneDrive
// Personal (2026-10-25/26: probes 2 and 3, L-03, L-30, L-33 and the
// systematic probe M-1 in testdata/probe-m1-personal.tsv), and every probed
// pair is a test case. Business is unprobed.
//
// A name has two forms in this client besides its bytes. Its key (Key, from
// Of) is what OneDrive compares; its stored form (Stored) is how the client
// records it. A key is a comparison value only: it is never displayed,
// stored, sent or joined into a path. A path's key (PathKey, from OfPath) is
// the key of each segment, joined. Relate says how two names' keys and
// stored forms relate, and Find and FindStored pick one entry of a listing
// by name under one rule, so that no caller picks by listing order.
package namekey

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// maxBMPRune is the last code point of the Basic Multilingual Plane.
const maxBMPRune = 0xFFFF

// distinctMarker prefixes a code point that keeps its own identity in a key
// (see keepsCanonicalIdentity). U+FFFF is a noncharacter, so no composed
// output can produce the marked sequence.
const distinctMarker = '￿'

// Key is the namespace key of one item name (Of). Two names collide on
// OneDrive when their keys are equal, as far as the probes show.
type Key string

// PathKey is the key of a slash-separated path (OfPath): the Key of each
// segment, joined by "/".
type PathKey string

// Of returns the namespace key of one item name. Keys are comparison values
// only, never names to write anywhere. They are computed in four steps:
//
//  1. Ignored code points are dropped: everything above U+FFFF (a😀.txt
//     equals a.txt, any two emoji are equal, L-30), and the BMP format
//     characters OneDrive ignores (isIgnored). ZWNJ, ZWSP and the soft
//     hyphen are compared normally.
//  2. Canonically equivalent spellings are equal (NFD café is the NFC item,
//     probe 2; U+0958 equals U+0915 U+093C), except for the code points
//     OneDrive keeps distinct from their canonical decomposition
//     (keepsCanonicalIdentity): every singleton but the Angstrom sign
//     (Kelvin sign is not K, U+FA16 is not U+732A), the Hebrew presentation
//     forms (U+FB2A is not U+05E9 U+05C1), and a few precomposed Greek and
//     Latin letters.
//  3. Letters are case-folded by Unicode simple case folding (σ/Σ/ς, ǅ/ǆ,
//     Cherokee), except the compatibility variants OneDrive keeps apart
//     (keepsCaseIdentity: µ is not Μ, ſ is not s, ϐ is not β, ẞ is not ß).
//  4. Only a handful of full foldings apply (fullFolding): ß is ss, İ is
//     i̇, and the Latin ligatures ﬀ ﬁ ﬂ ﬃ ﬄ ﬆ are their letters. Other
//     expanding foldings (ŉ, ﬅ, Armenian ligatures, Greek with iota
//     subscript) are not applied.
func Of(name string) Key {
	if isASCII(name) {
		return Key(asciiLower(name))
	}

	var key strings.Builder
	key.Grow(len(name))

	var run []rune
	flush := func() {
		if len(run) == 0 {
			return
		}
		for _, r := range norm.NFC.String(string(run)) {
			writeFolded(&key, r)
		}
		run = run[:0]
	}

	for _, r := range name {
		switch {
		case isIgnored(r):
			continue
		case keepsCanonicalIdentity(r):
			flush()
			key.WriteRune(distinctMarker)
			key.WriteRune(r)
		default:
			run = append(run, r)
		}
	}
	flush()

	return Key(key.String())
}

// OfPath returns the key of a slash-separated path: Of of each segment,
// joined by "/". A key never holds "/", so two paths have one key exactly
// when they have as many segments and each pair has one key (EqualPath).
// Empty segments stay empty: the caller cleans the path first if a doubled
// or trailing slash is not part of it.
func OfPath(slashPath string) PathKey {
	segments := strings.Split(slashPath, "/")
	for i, segment := range segments {
		segments[i] = string(Of(segment))
	}

	return PathKey(strings.Join(segments, "/"))
}

// HasPrefix reports whether the path of k is the path of root or lies below
// it, on a segment boundary: a/b/c is below a/b, a/bc is not. The empty key
// is not a root: it is the key of a name whose every character OneDrive
// ignores (an emoji alone), so "x" does not lie below it. A caller whose
// root can be the drive's or the tree's own root tests for that path itself.
func (k PathKey) HasPrefix(root PathKey) bool {
	return k == root || strings.HasPrefix(string(k), string(root)+"/")
}

// Equal reports whether OneDrive treats two names in one folder as the same.
func Equal(a, b string) bool {
	return a == b || Of(a) == Of(b)
}

// EqualPath reports whether two slash-separated paths name the same OneDrive
// item: the same number of segments, each pair of one key (Of).
func EqualPath(a, b string) bool {
	if a == b {
		return true
	}

	aSegments := strings.Split(a, "/")
	bSegments := strings.Split(b, "/")
	if len(aSegments) != len(bSegments) {
		return false
	}

	for i := range aSegments {
		if !Equal(aSegments[i], bSegments[i]) {
			return false
		}
	}

	return true
}

// Stored returns the form of a name this client stores and compares local
// spellings by: its NFC form (R-2.13.1). It is not a comparison of OneDrive
// names by itself: two names of one stored form can be two names on
// OneDrive (Twin), and two names of one key can be stored apart (SameName).
func Stored(name string) string {
	return norm.NFC.String(name)
}

// StoredPath returns the stored form of a slash-separated path: NFC over the
// whole string. It equals the join of the stored forms of its segments
// (TestStoredPath_EqualsPerSegment): "/" is a starter that composes with
// nothing, so normalization never reaches across it, and a path stored as a
// whole matches a path assembled from stored names.
func StoredPath(slashPath string) string {
	return norm.NFC.String(slashPath)
}

// Relation is how two names relate under OneDrive's key (Of) and this
// client's stored form (Stored). Every pair of strings has exactly one.
type Relation uint8

const (
	// Unrelated: different keys and different stored forms ("a" and "b").
	Unrelated Relation = iota
	// Twin: one stored form, different keys. The Kelvin sign "\u212A" is
	// stored as "K", and OneDrive keeps the two apart.
	Twin
	// SameName: one key, different stored forms ("Todo.md" and "TODO.md").
	SameName
	// SameStored: one key and one stored form, different bytes: the NFD
	// "cafe\u0301" and the NFC "caf\u00e9".
	SameStored
	// Identical: the same bytes.
	Identical
)

// Relate returns the relation of a and b. It is symmetric.
func Relate(a, b string) Relation {
	if a == b {
		return Identical
	}
	sameKey := Of(a) == Of(b)
	sameStored := Stored(a) == Stored(b)
	switch {
	case sameKey && sameStored:
		return SameStored
	case sameKey:
		return SameName
	case sameStored:
		return Twin
	default:
		return Unrelated
	}
}

// Match is the answer of Find and FindStored.
type Match struct {
	// Index is the matched entry's position in the listing; -1 when nothing
	// matched and when the answer is Ambiguous.
	Index int
	// Relation is the matched entry's relation to the wanted name; when
	// Ambiguous, the tier that held more than one entry; Unrelated when
	// nothing matched.
	Relation Relation
	// Ambiguous reports that the best tier held more than one entry.
	Ambiguous bool
}

// Found reports whether the match names one entry.
func (m Match) Found() bool {
	return m.Index >= 0
}

// Find picks the entry of a listing as OneDrive gives it that holds want:
// the entry spelled exactly so, else the only one of want's key and stored
// form, else the only one of want's key. Two entries in the best tier that
// holds any are Ambiguous, and none is picked. A Twin is never picked: it is
// another name on OneDrive. name returns an entry's name and whether the
// entry is a candidate at all (of the wanted kind, with an ID); a refused
// entry is never picked. The listing order decides nothing, so a caller
// whose listing can repeat an item removes the repeats first.
func Find[T any](items []T, want string, name func(*T) (string, bool)) Match {
	best := Match{Index: -1, Relation: Unrelated}
	for i := range items {
		candidate, ok := name(&items[i])
		if !ok {
			continue
		}
		relation := Relate(candidate, want)
		if relation < SameName || relation < best.Relation {
			continue
		}
		if relation == best.Relation {
			best.Ambiguous = true
			continue
		}
		best = Match{Index: i, Relation: relation}
	}
	if best.Ambiguous {
		best.Index = -1
	}

	return best
}

// FindStored picks the entry of a local directory listing whose stored form
// is stored: one entry is the match, two or more are a local name collision
// and the answer is Ambiguous. Relation is the matched entry's relation to
// stored (Identical, SameStored or Twin).
func FindStored[T any](items []T, stored string, name func(*T) string) Match {
	found := Match{Index: -1, Relation: Unrelated}
	for i := range items {
		candidate := name(&items[i])
		if Stored(candidate) != stored {
			continue
		}
		if found.Index >= 0 || found.Ambiguous {
			found = Match{Index: -1, Relation: found.Relation, Ambiguous: true}
			continue
		}
		found = Match{Index: i, Relation: Relate(candidate, stored)}
	}

	return found
}

// DiffersInIgnoredCharacters reports whether a or b contains characters the
// server ignores when comparing names (see Of), so that a message can say
// why two visibly different names are one name on OneDrive.
func DiffersInIgnoredCharacters(a, b string) bool {
	return strings.ContainsFunc(a, isIgnored) || strings.ContainsFunc(b, isIgnored)
}

func writeFolded(key *strings.Builder, r rune) {
	if expansion, ok := fullFolding(r); ok {
		key.WriteString(expansion)
		return
	}
	if keepsCaseIdentity(r) {
		key.WriteRune(r)
		return
	}

	key.WriteRune(simpleFoldKey(r))
}

// simpleFoldKey is a stable representative of r's simple case-folding orbit:
// the lowest lowercase mapping among its members, ignoring the variants
// OneDrive keeps apart. Lowercase, so ASCII keys agree with asciiLower.
func simpleFoldKey(r rune) rune {
	best := unicode.ToLower(r)
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if keepsCaseIdentity(f) {
			continue
		}
		if lower := unicode.ToLower(f); lower < best {
			best = lower
		}
	}

	return best
}

// isIgnored reports a code point OneDrive drops when comparing names (L-30,
// M-1): everything beyond the BMP, ZWJ, the Mongolian free variation
// selectors, U+2060-U+2064, and the variation selectors U+FE00-U+FE0F.
func isIgnored(r rune) bool {
	switch {
	case r > maxBMPRune,
		r == '‍',
		r >= '᠋' && r <= '᠍',
		r >= '⁠' && r <= '⁤',
		r >= '︀' && r <= '️':
		return true
	default:
		return false
	}
}

// keepsCanonicalIdentity reports a code point that OneDrive does not treat as
// equal to its canonical decomposition (M-1), so it is neither normalized nor
// folded: canonical singletons other than the Angstrom sign, the Hebrew
// presentation forms U+FB1D-U+FB4F, and the probed exceptions below.
func keepsCanonicalIdentity(r rune) bool {
	if r < utf8.RuneSelf || r == 'Å' {
		return false
	}
	if r >= 'יִ' && r <= 'ﭏ' {
		return isCanonicallyDecomposable(r)
	}
	if slices.Contains(distinctPrecomposed(), r) {
		return true
	}

	return isCanonicalSingleton(r)
}

// distinctPrecomposed lists precomposed letters M-1 found distinct from their
// canonical decompositions although they are not singletons.
func distinctPrecomposed() []rune {
	return []rune{
		'̈́', 'ΐ', 'ΰ', 'ਲ਼', 'ਸ਼', 'ẖ',
		'ὐ', 'ὒ', 'ὔ', 'ὖ', 'ᾶ', 'ῆ',
		'ῒ', 'ῖ', 'ῗ', 'ῢ', 'ῤ', 'ῦ',
		'ῧ', 'ῶ', '⫝̸',
	}
}

// isCanonicalSingleton reports a code point whose canonical decomposition is
// one other code point: NFC rewrites it alone and never restores it.
func isCanonicalSingleton(r rune) bool {
	var buf [utf8.UTFMax]byte
	n := utf8.EncodeRune(buf[:], r)
	if norm.NFC.IsNormal(buf[:n]) {
		return false
	}
	if utf8.RuneCountInString(norm.NFD.String(string(r))) == 1 {
		return true
	}

	// A singleton whose target itself decomposes (U+1FD3 -> U+0390).
	return utf8.RuneCountInString(norm.NFC.String(string(r))) == 1
}

func isCanonicallyDecomposable(r rune) bool {
	return norm.NFD.String(string(r)) != string(r)
}

// keepsCaseIdentity reports a compatibility variant that OneDrive does not
// fold onto its plain letter (M-1, L-03).
func keepsCaseIdentity(r rune) bool {
	switch r {
	case 'µ', // micro sign
		'ſ',                               // long s
		'ͅ',                               // combining ypogegrammeni
		'ϐ', 'ϑ', 'ϕ', 'ϖ', 'ϰ', 'ϱ', 'ϵ', // Greek symbol variants
		'ẛ', // long s with dot above
		'ẞ', // capital sharp s
		'ι': // Greek prosgegrammeni
		return true
	default:
		return r >= 'ᲀ' && r <= 'ᲇ' // Cyrillic rounded/tall variants
	}
}

// fullFolding returns the expanding case foldings OneDrive applies (M-1,
// probe 3).
func fullFolding(r rune) (string, bool) {
	switch r {
	case 'ß':
		return "ss", true
	case 'İ':
		return "i̇", true
	case 'ﬀ':
		return "ff", true
	case 'ﬁ':
		return "fi", true
	case 'ﬂ':
		return "fl", true
	case 'ﬃ':
		return "ffi", true
	case 'ﬄ':
		return "ffl", true
	case 'ﬆ':
		return "st", true
	default:
		return "", false
	}
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}

	return true
}

func asciiLower(s string) string {
	for i := range len(s) {
		if s[i] >= 'A' && s[i] <= 'Z' {
			return strings.ToLower(s)
		}
	}

	return s
}
