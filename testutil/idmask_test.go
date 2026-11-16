package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Invented identifiers of each shape maskIdentifierShapes masks.
const (
	inventedPersonalItemID         = "DEADBEEF00C0FFEE!s1nvented"
	inventedUnpaddedPersonalItemID = "EADBEEF00C0FFEE!42"
	inventedBusinessItemID         = "01INVENTEDBUSINESSITEMAAAAAAAAAAAA"
	inventedBusinessDriveID        = "b!Invented-Business_Drive0000"
	// inventedRouteDriveID is a Personal drive ID no fixture holds.
	inventedRouteDriveID = "0a1b2c3d4e5f6a7b"
)

// shapedIdentifiers are identifiers of every shape, each in the spellings
// Graph and Redact give it, and with its ! percent-encoded as a route spells
// it (url.PathEscape).
func shapedIdentifiers() []string {
	return []string{
		inventedPersonalItemID,
		strings.ToLower(inventedPersonalItemID),
		strings.ReplaceAll(inventedPersonalItemID, "!", percentEncodedBang),
		inventedUnpaddedPersonalItemID,
		// On a configured drive, which Redact names before the mask runs.
		strings.ToUpper(plantedFirstGraphDrive) + "!77",
		strings.ToUpper(plantedFirstGraphDrive) + percentEncodedBang + "77",
		// An item part that holds a fixture's drive ID, which Redact names
		// inside it.
		"DEADBEEF00C0FFEE!s0f1e2d3c4b5a6978aa",
		inventedBusinessItemID,
		outsideItemID,
		inventedBusinessDriveID,
		strings.ReplaceAll(inventedBusinessDriveID, "!", percentEncodedBang),
	}
}

// shapeMaskText is a text that quotes an identifier at its %s, and the text
// Redact makes of it, its identifier at %s, when the two differ.
type shapeMaskText struct {
	name string
	text string
	want string
}

func shapeMaskTexts() []shapeMaskText {
	return []shapeMaskText{
		{
			name: "stale precondition",
			text: "file changed while it was being synced: remote delete item %s changed before mutation",
		},
		{name: "item operation", text: `delete item "%s": graph: HTTP 404: itemNotFound`},
		{
			name: "transfer verification",
			text: "uploading e2e-x/file.txt: reading item %s: graph: HTTP 404: itemNotFound",
		},
		{name: "store error", text: "sync: recording pending create of e2e-x/file.txt (item %s): database is locked"},
		{
			name: "child mount ID",
			text: "removed shortcut " + plantedFirstDrive + "|binding:%s has no reference to its drive's removal record",
			want: "removed shortcut drive-1|binding:%s has no reference to its drive's removal record",
		},
		{name: "no format of the product", text: "retrying %s after a transient failure"},
		// A percent-encoded byte ends the word before the shape after it.
		{name: "after an encoded colon", text: "retrying x%%3A%s"},
		{name: "after an encoded slash", text: "uploading e2e-x%%2F%s"},
	}
}

// Validates: R-6.10.27
//
// An identifier of the shape of a Personal item ID, a Business item ID or a
// Business drive ID, its "!" also spelled %21, is masked wherever it stands
// in free text, with no format to place it: in a stale-precondition error,
// an item operation's error, a transfer verification error, a store error,
// a child mount ID, a text no format of the product makes, and right after
// a percent-encoded colon or slash. A Personal item ID on a known drive is
// masked whole, its drive's role with it.
func TestRedactMasksAnIdentifierByItsShapeInFreeText(t *testing.T) {
	t.Parallel()

	roles := plantedSuiteRoles()
	for _, row := range shapeMaskTexts() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			want := row.want
			if want == "" {
				want = row.text
			}
			for _, id := range shapedIdentifiers() {
				assert.Equal(t, fmt.Sprintf(want, maskedIdentifier), roles.Redact(fmt.Sprintf(row.text, id)), id)
			}
		})
	}
}

// Validates: R-6.10.27
//
// A shape is masked only as a whole word, so a word that holds one inside it
// stays, and so does a bare Personal drive ID: 16 hex digits, the shape of any
// hex or decimal word that long, such as a test's file name.
func TestRedactLeavesAWordThatOnlyHoldsAnIdentifiersShape(t *testing.T) {
	t.Parallel()

	roles := plantedSuiteRoles()
	for _, text := range []string{
		"retrying x" + inventedBusinessItemID,
		"retrying " + inventedBusinessItemID + "Z",
		"retrying 1" + inventedPersonalItemID,
		"retrying Bob!Invented",
		"drive DEADBEEF00C0FFEE is not running",
		"uploaded e2e-x/0123456789abcdef.txt",
	} {
		assert.Equal(t, text, roles.Redact(text))
	}
}

// Validates: R-6.10.27
//
// A Graph route printed without a scheme, as a request's debug record names
// it in text and JSON and a transport error quotes it, has its identifier
// segments masked as a URL's are, on a drive the suite does not know.
func TestRedactMasksTheIdentifiersOfARouteWithoutAScheme(t *testing.T) {
	t.Parallel()

	roles := plantedSuiteRoles()
	route := "/drives/" + inventedRouteDriveID + "/items/" +
		strings.ReplaceAll(inventedPersonalItemID, "!", percentEncodedBang) + "/content"
	for _, row := range []struct{ text, want string }{
		{
			text: `level=DEBUG msg="request succeeded" method=GET path=` + route + " status=200",
			want: `level=DEBUG msg="request succeeded" method=GET path=/drives/masked/items/masked/content status=200`,
		},
		{
			text: `{"level":"DEBUG","msg":"request succeeded","path":"` + route + `","status":200}`,
			want: `{"level":"DEBUG","msg":"request succeeded","path":"/drives/masked/items/masked/content","status":200}`,
		},
		{
			text: "graph: GET /drives/" + inventedRouteDriveID + "/root/delta: context deadline exceeded",
			want: "graph: GET /drives/masked/root/delta: context deadline exceeded",
		},
	} {
		assert.Equal(t, row.want, roles.Redact(row.text))
	}
}

// Validates: R-6.10.27
//
// A role before a Personal item ID's ! is found at the end of a long
// hyphen-joined run as it is in a short one, and a run that ends in none is
// left as it is.
func TestRedactFindsADrivesRoleAtTheEndOfALongHyphenRun(t *testing.T) {
	t.Parallel()

	const hyphenRunLength = 20000
	roles := plantedSuiteRoles()
	run := strings.Repeat("x-", hyphenRunLength)

	assert.Equal(t, run+maskedIdentifier, roles.Redact(run+strings.ToUpper(plantedFirstGraphDrive)+"!91"))
	roleless := run + "x!91"
	assert.Equal(t, roleless, roles.Redact(roleless))
}

// Validates: R-6.10.27
//
// The recycle-bin table mask follows the product's table: its headers, the
// item ID last (recycleBinHeaderPattern).
func TestRecycleBinTableMaskFollowsTheProductsHeaders(t *testing.T) {
	t.Parallel()

	source, err := os.ReadFile(filepath.Join("..", "internal", "cli", "recycle_bin.go"))
	require.NoError(t, err)
	assert.Contains(t, string(source), `headers := []string{"NAME", "SIZE", "TYPE", "DELETED", "ID"}`,
		"recycle-bin list's table changed: give recycleBinHeaderPattern and maskRecycleBinIDs its new columns")
	assert.Regexp(t, recycleBinHeaderPattern, "NAME  SIZE  TYPE  DELETED  ID")
}
