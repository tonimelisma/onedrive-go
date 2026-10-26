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

// Key returns the namespace key of one item name. Two names collide on
// OneDrive when their keys are equal, as far as the probes show. Keys are
// comparison values only, never names to write anywhere. They are computed
// in four steps:
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
func Key(name string) string {
	if isASCII(name) {
		return asciiLower(name)
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

	return key.String()
}

// Equal reports whether OneDrive treats two names in one folder as the same.
func Equal(a, b string) bool {
	return a == b || Key(a) == Key(b)
}

// EqualPath reports whether two slash-separated paths name the same OneDrive
// item: the same number of segments, each pair equal under Key.
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

// DiffersInIgnoredCharacters reports whether a or b contains characters the
// server ignores when comparing names (see Key), so that a message can say
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
