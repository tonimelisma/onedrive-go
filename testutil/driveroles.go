package testutil

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/sharedref"
)

// DriveRoles names the live test accounts and fixtures by role: drive-1 for
// the primary drive, drive-2 for the secondary one, drive-1-account for its
// address, and the role each IdentityRole gives a value the suite holds about
// an account or a fixture. What the live suites record, the timing and quirk
// summaries, the per-test debug logs and the failure messages, goes into the
// logs and artifacts of a public repository's CI, where log masking does not
// reach, so every recorder writes through Redact and records roles, never an
// identifier, an address or a name of a test account. The zero value redacts
// nothing.
type DriveRoles struct {
	roles    map[string]string
	replacer *strings.Replacer
	// nameOrder are the values of nameRoles, longest first.
	nameOrder []string
	nameRoles map[string]string
	// roleWords are every role Redact writes, which the identifier mask
	// leaves in place, and longestRole the length of the longest of them and
	// of maskedIdentifier.
	roleWords   map[string]bool
	longestRole int
	// unlearnedNameRoles are the roles of the names too short to learn.
	unlearnedNameRoles []string
}

// IdentityRole is a value the suite holds about a test account or a live
// fixture, with the role Redact writes in its place. A name (a display name,
// a fixture's folder name) is replaced only as a whole word, so a short name
// cannot cut into a test's own names; any other value wherever it stands.
type IdentityRole struct {
	Value  string
	Role   string
	IsName bool
}

const (
	driveRolePrefix     = "drive-"
	driveAccountSuffix  = "-account"
	percentEncodedAt    = "%40"
	percentEncodedColon = "%3A"
	percentEncodedBang  = "%21"

	// graphDriveIDLength is the length driveid.New pads a Personal drive ID
	// to; Graph sends some without their leading zeros.
	graphDriveIDLength = 16
	// minUnpaddedDriveIDLength keeps an unpadded spelling long enough that it
	// cannot match unrelated text.
	minUnpaddedDriveIDLength = 12
	// minLearnedNameLength is the fewest characters a name must have to be
	// learned: a shorter one, replaced as a whole word, would replace
	// ordinary words of the tests' own output.
	minLearnedNameLength = 4
)

// NewDriveRoles names driveIDs, in order, drive-1, drive-2 and so on. An
// empty drive keeps its place and is skipped.
func NewDriveRoles(driveIDs ...string) DriveRoles {
	return NewSuiteRoles(driveIDs, nil)
}

// NewSuiteRoles names driveIDs as NewDriveRoles does and every value of
// identities by its role. A value keeps the first role it is given, so a
// fixture's value that is a configured drive or its address keeps the drive's
// role. A name shorter than minLearnedNameLength characters is not learned;
// UnlearnedNameRoles lists its role.
func NewSuiteRoles(driveIDs []string, identities []IdentityRole) DriveRoles {
	built := &roleBuilder{roles: map[string]string{}, replacements: map[string]string{}, nameRoles: map[string]string{}}
	for i, driveID := range driveIDs {
		if driveID == "" {
			continue
		}
		role := driveRolePrefix + strconv.Itoa(i+1)
		built.roles[driveID] = role
		built.addValue(driveSpellings(driveID), role)
		if address := driveEmailFromCanonicalID(driveID); address != "" {
			built.addValue(driveSpellings(address), role+driveAccountSuffix)
		}
	}
	for _, identity := range identities {
		switch {
		case identity.Value == "" || identity.Role == "":
		case identity.IsName && utf8.RuneCountInString(identity.Value) < minLearnedNameLength:
			built.unlearnedNameRoles = append(built.unlearnedNameRoles, identity.Role)
		case identity.IsName:
			if _, taken := built.nameRoles[identity.Value]; !taken {
				built.nameRoles[identity.Value] = identity.Role
			}
		default:
			built.addValue(identitySpellings(identity.Value), identity.Role)
		}
	}

	return built.driveRoles()
}

// roleBuilder collects what NewSuiteRoles names before it compiles them.
type roleBuilder struct {
	roles              map[string]string
	replacements       map[string]string
	nameRoles          map[string]string
	unlearnedNameRoles []string
}

func (b *roleBuilder) addValue(spellings []string, role string) {
	for _, spelling := range spellings {
		if _, taken := b.replacements[spelling]; !taken {
			b.replacements[spelling] = role
		}
	}
}

func (b *roleBuilder) driveRoles() DriveRoles {
	if len(b.replacements) == 0 && len(b.nameRoles) == 0 {
		return DriveRoles{unlearnedNameRoles: b.unlearnedNameRoles}
	}

	roleWords := map[string]bool{}
	longestRole := len(maskedIdentifier)
	for _, roles := range []map[string]string{b.replacements, b.nameRoles} {
		for _, role := range roles {
			roleWords[role] = true
			longestRole = max(longestRole, len(role))
		}
	}

	// Longest first: an identifier is replaced whole before the account
	// address or the drive inside it.
	olds := longestFirst(b.replacements)
	pairs := make([]string, 0, 2*len(olds))
	for _, old := range olds {
		pairs = append(pairs, old, b.replacements[old])
	}

	return DriveRoles{
		roles:              b.roles,
		replacer:           strings.NewReplacer(pairs...),
		nameOrder:          longestFirst(b.nameRoles),
		nameRoles:          b.nameRoles,
		roleWords:          roleWords,
		longestRole:        longestRole,
		unlearnedNameRoles: b.unlearnedNameRoles,
	}
}

// UnlearnedNameRoles lists, in the order given, the roles of the names
// NewSuiteRoles did not learn because they are too short; Redact leaves such
// a name as it is.
func (r DriveRoles) UnlearnedNameRoles() []string {
	return r.unlearnedNameRoles
}

func longestFirst(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})

	return keys
}

// replaceWholeWords replaces each of names, longest first, with its role in
// roles where it is not part of a longer word: an edge of the name that is an
// ASCII word character must not meet another one.
func replaceWholeWords(text string, names []string, roles map[string]string) string {
	for _, name := range names {
		if !strings.Contains(text, name) {
			continue
		}

		var replaced strings.Builder
		last := 0
		for from := 0; from < len(text); {
			found := strings.Index(text[from:], name)
			if found < 0 {
				break
			}
			start := from + found
			end := start + len(name)
			from = start + 1
			if !isWholeWordAt(text, start, end) {
				continue
			}
			replaced.WriteString(text[last:start])
			replaced.WriteString(roles[name])
			last, from = end, end
		}
		if last > 0 {
			replaced.WriteString(text[last:])
			text = replaced.String()
		}
	}

	return text
}

func isWholeWordAt(text string, start, end int) bool {
	if start > 0 && isASCIIWordByte(text[start]) && isASCIIWordByte(text[start-1]) {
		return false
	}

	return end >= len(text) || !isASCIIWordByte(text[end-1]) || !isASCIIWordByte(text[end])
}

func isASCIIWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// driveSpellings lists the ways a recorded line can spell a drive identifier
// or an account address: as written, with its colons as underscores (test
// and token file names), lower-cased, and percent-encoded as Graph URLs spell
// it.
func driveSpellings(value string) []string {
	var spellings []string
	for _, form := range []string{value, strings.ReplaceAll(value, ":", "_")} {
		for _, cased := range []string{form, strings.ToLower(form)} {
			encoded := strings.NewReplacer("@", percentEncodedAt, ":", percentEncodedColon).Replace(cased)
			spellings = append(spellings, cased, encoded)
		}
	}

	return spellings
}

// identitySpellings adds to driveSpellings the forms Graph gives an
// identifier: upper-cased (a Personal drive ID and the item IDs that start
// with it), with a Personal item ID's "!" percent-encoded, and a padded
// Personal drive ID without the leading zeros Graph sometimes drops.
func identitySpellings(value string) []string {
	spellings := append(driveSpellings(value), strings.ToUpper(value))
	for _, cased := range []string{value, strings.ToLower(value), strings.ToUpper(value)} {
		spellings = append(spellings, strings.ReplaceAll(cased, "!", percentEncodedBang))
	}
	if len(value) == graphDriveIDLength && isHex(value) {
		for unpadded := value; strings.HasPrefix(unpadded, "0"); {
			unpadded = unpadded[1:]
			if len(unpadded) < minUnpaddedDriveIDLength {
				break
			}
			spellings = append(spellings, unpadded, strings.ToUpper(unpadded))
		}
	}

	return spellings
}

func isHex(value string) bool {
	return strings.Trim(value, "0123456789abcdefABCDEF") == ""
}

// Role returns the role of a configured drive, a redacted spelling of any
// other text, and the empty string for an empty drive.
func (r DriveRoles) Role(driveID string) string {
	if role, ok := r.roles[driveID]; ok {
		return role
	}

	return r.Redact(driveID)
}

// Redact replaces every spelling of a configured drive in text with its role,
// every spelling of its account address with the role followed by -account,
// and every value of an IdentityRole with its role. Then it masks, to the
// word "masked", every identifier left where the product's output puts one
// and every one of an item or Business drive ID's shape or an address's
// wherever it stands (maskIdentifierPositions): the IDs of items and drives
// no fixture holds, and the sharers no configured drive is.
func (r DriveRoles) Redact(text string) string {
	if r.replacer == nil && len(r.nameOrder) == 0 {
		return text
	}

	if r.replacer != nil {
		text = r.replacer.Replace(text)
	}
	text = replaceWholeWords(text, r.nameOrder, r.nameRoles)

	return maskIdentifierPositions(text, r.isRoleValue, r.longestRole)
}

// isRoleValue reports a value that names nothing but roles: a role, the mask
// word, or a canonical drive ID built of them ("shared:drive-1-account:masked:masked").
func (r DriveRoles) isRoleValue(value string) bool {
	if r.roleWords[value] || value == maskedIdentifier {
		return true
	}
	if !strings.Contains(value, ":") {
		return false
	}

	for _, part := range strings.Split(value, ":") {
		switch {
		case part == maskedIdentifier, r.roleWords[part]:
		case part == "personal", part == "business", part == "sharepoint", part == "shared":
		default:
			return false
		}
	}

	return true
}

// accountFirstNameSuffix ends the role of an account's first name, after
// the account's role (drive-1-account-first-name).
const accountFirstNameSuffix = "-first-name"

// AccountNameIdentities names an account's display name by accountRole
// followed by -name, and the first name shared discovery labels a folder the
// account shares with ("<first name>'s <folder>", internal/cli's
// deriveSharedDisplayName) by accountRole followed by -first-name. Both are
// names: one shorter than minLearnedNameLength is reported, not learned.
func AccountNameIdentities(displayName, accountRole string) []IdentityRole {
	identities := []IdentityRole{{Value: displayName, Role: accountRole + "-name", IsName: true}}
	if first := sharerFirstName(displayName); first != displayName {
		identities = append(identities, IdentityRole{Value: first, Role: accountRole + accountFirstNameSuffix, IsName: true})
	}

	return identities
}

// sharerFirstName is the first name shared discovery takes from a sharer's
// display name: the text before its first space, or the whole name
// (internal/cli's extractFirstName, which a source test holds this to).
func sharerFirstName(displayName string) string {
	if i := strings.Index(displayName, " "); i > 0 {
		return displayName[:i]
	}

	return displayName
}

// FixtureIdentities lists the values of fixtures by role: each shared-folder
// selector, its drive, item and account; each shared link and the share
// token Graph URLs spell it as; each shortcut's parent drive, sharer, name
// and sentinel, a sentinel both shortcuts share as shortcut-sentinel; the
// standalone shared folder's name.
func FixtureIdentities(fixtures *LiveFixtures) []IdentityRole {
	identities := selectorIdentities(fixtures.WritableSharedFolderSelector, "writable-shared-folder")
	identities = append(identities, selectorIdentities(fixtures.ReadOnlySharedFolderSelector, "read-only-shared-folder")...)
	identities = append(identities, linkIdentities(fixtures.SharedFileLink, "shared-file-link")...)
	identities = append(identities, linkIdentities(fixtures.SharedFolderLink, "shared-folder-link")...)

	for _, shortcut := range []struct {
		role, parentDrive, sharerEmail, name, sentinelPath string
	}{
		{
			role: "writable-shortcut", parentDrive: fixtures.WritableShortcutParentDrive,
			sharerEmail: fixtures.WritableShortcutSharerEmail, name: fixtures.WritableShortcutName,
			sentinelPath: fixtures.WritableShortcutSentinelPath,
		},
		{
			role: "read-only-shortcut", parentDrive: fixtures.ReadOnlyShortcutParentDrive,
			sharerEmail: fixtures.ReadOnlyShortcutSharerEmail, name: fixtures.ReadOnlyShortcutName,
			sentinelPath: fixtures.ReadOnlyShortcutSentinelPath,
		},
	} {
		sentinelRole := shortcut.role + "-sentinel"
		if fixtures.WritableShortcutSentinelPath == fixtures.ReadOnlyShortcutSentinelPath {
			sentinelRole = "shortcut-sentinel"
		}
		identities = append(identities,
			IdentityRole{Value: shortcut.parentDrive, Role: shortcut.role + "-parent"},
			IdentityRole{Value: driveEmailFromCanonicalID(shortcut.parentDrive), Role: shortcut.role + "-parent" + driveAccountSuffix},
			IdentityRole{Value: shortcut.sharerEmail, Role: shortcut.role + "-sharer"},
			IdentityRole{Value: shortcut.name, Role: shortcut.role, IsName: true},
			IdentityRole{Value: shortcut.sentinelPath, Role: sentinelRole, IsName: true},
		)
	}

	return append(identities,
		IdentityRole{Value: fixtures.StandaloneSharedFolderName, Role: "standalone-shared-folder", IsName: true})
}

// selectorIdentities names a shared selector role, and its remote drive,
// remote item and recipient role-drive, role-item and role-account.
func selectorIdentities(selector, role string) []IdentityRole {
	identities := []IdentityRole{{Value: selector, Role: role}}
	ref, err := sharedref.Parse(selector)
	if err != nil {
		return identities
	}

	return append(identities,
		IdentityRole{Value: ref.RemoteDriveID, Role: role + "-drive"},
		IdentityRole{Value: ref.RemoteItemID, Role: role + "-item"},
		IdentityRole{Value: ref.AccountEmail, Role: role + driveAccountSuffix},
	)
}

// linkIdentities names a sharing link and the share token Graph's /shares
// route spells it as ("u!" and the link in unpadded base64url, as
// internal/graph encodes it).
func linkIdentities(link, role string) []IdentityRole {
	if link == "" {
		return nil
	}

	normalized := link
	if parsed, err := url.Parse(link); err == nil {
		normalized = parsed.String()
	}

	return []IdentityRole{
		{Value: link, Role: role},
		{Value: graphShareTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(normalized)), Role: role},
	}
}

// AccountFinding is one line of a tracked file that spells a test account.
type AccountFinding struct {
	File string
	Line int
	// Account is the account's role, such as drive-1-account: a finding
	// never carries the address.
	Account string
}

// String names the file, the line and the account's role.
func (f AccountFinding) String() string {
	return f.File + ":" + strconv.Itoa(f.Line) + " spells " + f.Account
}

// FindTestAccounts lists every line of the files, relative to root, that
// holds the account address of one of driveIDs (the configured drives and
// the allowlist entries) as written, in any case, or percent-encoded. Each
// account is named by its role in driveIDs' order. A binary file is read like
// any other, its line being the count of newlines before the address; a
// tracked file the checkout lacks is skipped.
func FindTestAccounts(root string, files []string, driveIDs []string) ([]AccountFinding, error) {
	unique := []string{}
	seen := map[string]bool{}
	for _, driveID := range driveIDs {
		driveID = strings.TrimSpace(driveID)
		if driveID == "" || seen[driveID] {
			continue
		}
		seen[driveID] = true
		unique = append(unique, driveID)
	}
	roles := NewDriveRoles(unique...)

	type account struct {
		role      string
		spellings []string
	}
	var accounts []account
	for _, driveID := range unique {
		// A shared selector's address is its second field only.
		address, _, _ := strings.Cut(strings.ToLower(driveEmailFromCanonicalID(driveID)), ":")
		if address == "" {
			continue
		}
		encoded := strings.NewReplacer("@", strings.ToLower(percentEncodedAt), ":", strings.ToLower(percentEncodedColon)).Replace(address)
		accounts = append(accounts, account{role: roles.Redact(address), spellings: []string{address, encoded}})
	}

	var findings []AccountFinding
	for _, file := range files {
		data, err := localpath.ReadFile(filepath.Join(root, file))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		for number, line := range strings.Split(strings.ToLower(string(data)), "\n") {
			for _, candidate := range accounts {
				for _, spelling := range candidate.spellings {
					if strings.Contains(line, spelling) {
						findings = append(findings, AccountFinding{File: file, Line: number + 1, Account: candidate.role})
						break
					}
				}
			}
		}
	}

	return findings, nil
}
