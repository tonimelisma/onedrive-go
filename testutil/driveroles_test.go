package testutil

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Invented values in the shapes the live suites meet: a Personal drive ID
// with leading zeros (Graph also sends it upper-cased and without them), a
// Business drive ID, item IDs of both kinds, selectors, links and names.
const (
	plantedFirstDrive       = "personal:planted.one@example.test"
	plantedSecondDrive      = "business:planted_two@example.test"
	plantedFirstGraphDrive  = "00a1b2c3d4e5f607"
	plantedSecondGraphDrive = "b!PlantedBusinessDrive_2-x"
	plantedUserID           = "planted-user-0001"
	plantedDisplayName      = "Planted Person"
	plantedWritableFolder   = "shared:planted.one@example.test:b!PlantedSharerDrive_1-y:01PLANTEDWRITABLEITEM0000000000000"
	plantedReadOnlyFolder   = "shared:planted.one@example.test:0f1e2d3c4b5a6978:0F1E2D3C4B5A6978!123"
	plantedFolderLink       = "https://1drv.ms/f/s!PlantedFolderLink"
	plantedFileLink         = "https://1drv.ms/t/s!PlantedFileLink"
	plantedOutsideSharer    = "outside.sharer@example.test"
	plantedShortcutName     = "Planted Writable Shortcut"
	plantedTestFolder       = "e2e-sync-root-1731700000000000000"
	// outsideItemID is a Business item ID no fixture holds.
	outsideItemID = "01OUTSIDEITEMAAAAAAAAAAAAAAAAAAAAA"
)

func plantedFixtures() LiveFixtures {
	return LiveFixtures{
		SharedFileLink:               plantedFileLink,
		SharedFolderLink:             plantedFolderLink,
		WritableSharedFolderSelector: plantedWritableFolder,
		ReadOnlySharedFolderSelector: plantedReadOnlyFolder,
		WritableShortcutParentDrive:  plantedFirstDrive,
		WritableShortcutName:         plantedShortcutName,
		WritableShortcutSharerEmail:  "planted_two@example.test",
		WritableShortcutSentinelPath: "planted-sentinel.txt",
		ReadOnlyShortcutParentDrive:  plantedSecondDrive,
		ReadOnlyShortcutName:         "Planted Read-only Shortcut",
		ReadOnlyShortcutSharerEmail:  plantedOutsideSharer,
		ReadOnlyShortcutSentinelPath: "planted-sentinel.txt",
		StandaloneSharedFolderName:   "Planted Standalone Folder",
	}
}

func plantedSuiteRoles() DriveRoles {
	fixtures := plantedFixtures()
	identities := FixtureIdentities(&fixtures)
	identities = append(identities,
		IdentityRole{Value: plantedFirstGraphDrive, Role: "drive-1-id"},
		IdentityRole{Value: plantedSecondGraphDrive, Role: "drive-2-id"},
		IdentityRole{Value: plantedUserID, Role: "drive-1-account-id"},
		IdentityRole{Value: plantedDisplayName, Role: "drive-1-account-name", IsName: true},
	)

	return NewSuiteRoles([]string{plantedFirstDrive, plantedSecondDrive}, identities)
}

// plantedValues are every planted identifier, address and name: none of them
// may survive Redact in any spelling.
func plantedValues() []string {
	return []string{
		"planted.one@example.test", "planted_two@example.test", plantedOutsideSharer,
		plantedFirstGraphDrive, "a1b2c3d4e5f607", plantedSecondGraphDrive, plantedUserID, plantedDisplayName,
		"PlantedSharerDrive_1-y", "01PLANTEDWRITABLEITEM0000000000000", "0f1e2d3c4b5a6978", "!123",
		"PlantedFolderLink", "PlantedFileLink", plantedShortcutName, "Planted Read-only Shortcut",
		"Planted Standalone Folder", "planted-sentinel", "OutsideDrive99", "01OUTSIDEITEM", "Outside Person",
		"outside.person@example.test", "!555", "!987", "s0f1e2d3c4b5a6978", "Planted Org",
		"outside_person@example.test",
	}
}

// redactShapeCase is one shape of printed text, with what a failure still
// needs from it.
type redactShapeCase struct {
	name  string
	input string
	keeps []string
}

func redactShapeCases() []redactShapeCase {
	return append(redactListingShapeCases(), redactPositionShapeCases()...)
}

// redactListingShapeCases are the listings and logs of the commands the
// suite runs most.
func redactListingShapeCases() []redactShapeCase {
	shareToken := "u!" + base64.RawURLEncoding.EncodeToString([]byte(plantedFolderLink))

	return []redactShapeCase{
		{
			name: "stat text",
			input: "Name:     fragile.txt\nID:       00A1B2C3D4E5F607!s0f1e2d3c4b5a69788796a5b4c3d2e1f0\n" +
				"Shared:   " + plantedReadOnlyFolder + "\n",
			keeps: []string{"Name:     fragile.txt", "ID:       masked", "Shared:   read-only-shared-folder"},
		},
		{
			name:  "ls --json",
			input: `[{"name":"` + plantedTestFolder + `","id":"00A1B2C3D4E5F607!987","size":0}]`,
			keeps: []string{`"name":"` + plantedTestFolder + `"`, `"id":"masked"`},
		},
		{
			name: "--debug stderr",
			input: `level=DEBUG msg="request failed" method=GET url=https://graph.microsoft.com/v1.0/drives/` +
				plantedFirstGraphDrive + `/items/00A1B2C3D4E5F607!987/children status=404 ` +
				"request_id=11111111-2222-3333-4444-555555555555\n" +
				"level=DEBUG msg=resolved drive_id=0f1e2d3c4b5a6978 item_id=0F1E2D3C4B5A6978!123 " +
				"remote_drive=b!OutsideDrive99 path=/e2e-folder/file.txt\n" +
				"level=DEBUG url=https://graph.microsoft.com/v1.0/shares/" + shareToken + "/driveItem\n" +
				"level=DEBUG url=https://graph.microsoft.com/v1.0/users/planted.one%40example.test/drive\n",
			keeps: []string{
				"/drives/drive-1-id/items/masked/children", "request_id=11111111-2222-3333-4444-555555555555",
				"drive_id=read-only-shared-folder-drive", "item_id=read-only-shared-folder-item",
				"remote_drive=masked", "path=/e2e-folder/file.txt", "/shares/shared-folder-link/driveItem",
				"/users/drive-1-account/drive",
			},
		},
		{
			name: "status --json",
			input: `{"accounts":[{"email":"planted.one@example.test","display_name":"` + plantedDisplayName +
				`","org_name":"Planted Org","drives":[{"name":"` + plantedTestFolder + `","display_name":"` +
				plantedTestFolder + `"}]}]}`,
			keeps: []string{
				`"email":"drive-1-account"`, `"display_name":"drive-1-account-name"`, `"org_name":"masked"`,
				`"display_name":"` + plantedTestFolder + `"`,
			},
		},
		{
			name: "debug log command line",
			input: "CMD: --config /tmp/onedrive-e2e-1/config.toml --drive shared:planted.one@example.test:" +
				plantedFirstGraphDrive + ":00A1B2C3D4E5F607!555 --debug stat /" + plantedTestFolder + "/file.txt\n" +
				"CMD: --drive " + plantedWritableFolder + " ls /\n",
			keeps: []string{
				"--drive shared:drive-1-account:drive-1-id:masked --debug stat /" + plantedTestFolder + "/file.txt",
				"--drive writable-shared-folder ls /",
			},
		},
		{
			name: "shared --json",
			input: `{"items":[{"selector":"shared:planted.one@example.test:b!OutsideDrive99:01OUTSIDEITEM0000000000000000000",` +
				`"type":"file","name":"holiday.txt","account_email":"planted.one@example.test",` +
				`"shared_by_name":"Outside Person","shared_by_email":"outside.person@example.test",` +
				`"remote_drive_id":"b!OutsideDrive99","remote_item_id":"01OUTSIDEITEM0000000000000000000"},` +
				`{"selector":"` + plantedWritableFolder + `","name":"` + plantedShortcutName + `",` +
				`"shared_by_email":"planted_two@example.test","remote_drive_id":"b!PlantedSharerDrive_1-y"}]}`,
			keeps: []string{
				`"selector":"shared:drive-1-account:masked:masked"`, `"name":"holiday.txt"`,
				`"shared_by_name":"masked"`, `"shared_by_email":"masked"`, `"remote_drive_id":"masked"`,
				`"remote_item_id":"masked"`, `"selector":"writable-shared-folder"`, `"name":"writable-shortcut"`,
				`"shared_by_email":"drive-2-account"`, `"remote_drive_id":"writable-shared-folder-drive"`,
			},
		},
		{
			name: "drive list --json and text",
			input: `{"configured":[{"canonical_id":"shared:planted.one@example.test:` + plantedFirstGraphDrive +
				`:00A1B2C3D4E5F607!555","display_name":"` + plantedTestFolder + `","sync_dir":"/tmp/e2e-sync-1",` +
				`"owner_name":"` + plantedDisplayName + `","owner_email":"planted.one@example.test"}]}` + "\n" +
				"  holiday folder (shared by Outside Person)\n  Docs (shared by planted_two@example.test)\n",
			keeps: []string{
				`"canonical_id":"shared:drive-1-account:drive-1-id:masked"`,
				`"display_name":"` + plantedTestFolder + `"`, `"sync_dir":"/tmp/e2e-sync-1"`,
				`"owner_name":"drive-1-account-name"`, `"owner_email":"drive-1-account"`,
				"holiday folder (shared by masked)", "Docs (shared by drive-2-account)",
			},
		},
		{
			name: "state database file name",
			input: "open /tmp/x/onedrive-go/state_shared_planted.one@example.test_" + plantedFirstGraphDrive +
				"_00A1B2C3D4E5F607!555.db: database is locked",
			keeps: []string{"/state_shared_drive-1-account_masked.db: database is locked"},
		},
	}
}

// redactPositionShapeCases are the positions of an identifier in a table, a
// command line, a scope key or a file name, and texts a scan must get past.
func redactPositionShapeCases() []redactShapeCase {
	return []redactShapeCase{
		{
			name: "state database file name of an address with an underscore",
			input: "open /tmp/x/onedrive-go/state_shared_outside_person@example.test_b!OutsideDrive99_" +
				outsideItemID + ".db: database is locked",
			keeps: []string{"/state_shared_masked_masked.db: database is locked"},
		},
		{
			name:  "stat with an empty ID line",
			input: "Name:     fragile.txt\nID:\nPath:     /e2e-folder/fragile.txt\n",
			keeps: []string{"ID:\nPath:     /e2e-folder/fragile.txt"},
		},
		{
			name: "an unclosed quote",
			input: `level=DEBUG msg="cut off` + "\n" + `level=DEBUG item_id=` + outsideItemID + "\n" +
				`{"note":"cut off` + "\n" + `{"name":"fragile.txt","id":"` + outsideItemID + `"}`,
			keeps: []string{"level=DEBUG item_id=masked", `{"name":"fragile.txt","id":"masked"}`},
		},
		{
			name: "recycle-bin list text",
			input: "NAME              SIZE  TYPE  DELETED                  ID                                \n" +
				"recycle-test.txt  24 B  file  2026-11-15 10:00:00 UTC  " + outsideItemID + "  \n" +
				"empty-test.txt    27 B  file  2026-11-15 10:01:00 UTC  00A1B2C3D4E5F607!987\n\nUp to date\n",
			keeps: []string{
				"NAME              SIZE  TYPE  DELETED                  ID",
				"recycle-test.txt  24 B  file  2026-11-15 10:00:00 UTC  masked",
				"empty-test.txt    27 B  file  2026-11-15 10:01:00 UTC  masked\n\nUp to date\n",
			},
		},
		{
			name: "recycle-bin restore command line",
			input: "CMD: --config /tmp/onedrive-e2e-1/config.toml --drive " + plantedSecondDrive + " --debug recycle-bin restore " +
				outsideItemID + "\nCLI command [recycle-bin restore 00A1B2C3D4E5F607!987] failed\n",
			keeps: []string{
				"--drive drive-2 --debug recycle-bin restore masked\n", "[recycle-bin restore masked] failed",
			},
		},
		{
			name: "throttle scope key",
			input: `level=DEBUG msg="cold start" scope_key=throttle:target:drive:b!OutsideDrive99 target=drive:b!OutsideDrive99` +
				"\n" + `{"scope":"throttle:target:drive:b!OutsideDrive99"}` + "\nnote=drive:" + plantedFirstGraphDrive,
			keeps: []string{
				"scope_key=throttle:target:drive:masked target=drive:masked",
				`{"scope":"throttle:target:drive:masked"}`, "note=drive:drive-1-id",
			},
		},
	}
}

// Validates: R-6.10.27
//
// What the live suite prints in a failure, a test log or its debug log names
// no drive, item, account or fixture of the test accounts. Each shape the
// suite meets is planted with invented values: stat's text, ls --json, the
// --debug log's Graph URLs and attributes, status --json, the debug log's
// command line, shared --json and drive list --json with an item no fixture
// holds, and a state database's file name. What a failure needs stays: the
// names and paths a test made, Graph request IDs, the roles.
func TestDriveRoles_RedactMasksTestAccountIdentitiesInEveryShape(t *testing.T) {
	t.Parallel()

	roles := plantedSuiteRoles()
	for _, tc := range redactShapeCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			redacted := roles.Redact(tc.input)
			for _, value := range plantedValues() {
				assert.NotContains(t, strings.ToLower(redacted), strings.ToLower(value), "masked text: %s", redacted)
			}
			for _, kept := range tc.keeps {
				assert.Contains(t, redacted, kept)
			}
		})
	}
}

// Validates: R-6.10.27
//
// A mask never hides what a failure needs: a name that holds a learned name
// inside a word, the long digit runs of the tests' own names, a Graph request
// ID, and values Redact does not know outside an identifier's position.
func TestDriveRoles_RedactKeepsTestNamesAndPaths(t *testing.T) {
	t.Parallel()

	roles := NewSuiteRoles([]string{plantedFirstDrive}, []IdentityRole{
		{Value: "Test", Role: "drive-1-account-name", IsName: true},
	})

	for _, text := range []string{
		"TestE2E_Sync_DirectionalModes/download_only",
		"e2e-sync-ed-download_only-1731700000000000000/fragile.txt",
		"graph: HTTP 404 (request-id: 0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0): itemNotFound",
		`{"name":"0123456789abcdef.txt","path":"/e2e-x/0123456789abcdef.txt"}`,
		"url=https://graph.microsoft.com/v1.0/drives/drive-1/root:/e2e-x/items/report.txt:/content",
		"path=/drives/drive-1/root:/e2e-x/items/report.txt:/content",
	} {
		assert.Equal(t, text, roles.Redact(text))
	}

	assert.Equal(t, "account drive-1-account-name signed in as drive-1-account",
		roles.Redact("account Test signed in as planted.one@example.test"))
}

// Validates: R-6.10.27
//
// A value that names a configured drive keeps the drive's role whatever a
// fixture calls it, and a value two fixtures share keeps the first role.
func TestFixtureIdentities_ConfiguredDrivesWinAndFirstRoleHolds(t *testing.T) {
	t.Parallel()

	roles := plantedSuiteRoles()

	assert.Equal(t, "drive-2-account", roles.Redact("planted_two@example.test"))
	assert.Equal(t, "drive-1", roles.Redact(plantedFirstDrive))
	assert.Equal(t, "read-only-shortcut-sharer", roles.Redact(plantedOutsideSharer))
	assert.Equal(t, "shortcut-sentinel", roles.Redact("planted-sentinel.txt"), "the sentinel both shortcuts share")
	assert.Equal(t, "shared-file-link", roles.Redact(plantedFileLink))
	assert.Equal(t, "read-only-shared-folder-item", roles.Redact("0F1E2D3C4B5A6978!123"))
	assert.Equal(t, "drive-1-id", roles.Redact("A1B2C3D4E5F607"), "Graph's unpadded, upper-cased spelling")

	var unset DriveRoles
	assert.Equal(t, plantedReadOnlyFolder, unset.Redact(plantedReadOnlyFolder), "the zero value masks nothing")

	fixtures := plantedFixtures()
	fixtures.ReadOnlyShortcutSentinelPath = "planted-read-only-sentinel.txt"
	apart := NewSuiteRoles(nil, FixtureIdentities(&fixtures))
	assert.Equal(t, "writable-shortcut-sentinel", apart.Redact("planted-sentinel.txt"))
	assert.Equal(t, "read-only-shortcut-sentinel", apart.Redact("planted-read-only-sentinel.txt"))
}

// Validates: R-6.10.27
//
// A name shorter than minLearnedNameLength characters is not learned: as a
// whole word it would replace ordinary words of the tests' own output. Its
// role is reported, never its value.
func TestNewSuiteRoles_ShortNamesAreNotLearned(t *testing.T) {
	t.Parallel()

	roles := NewSuiteRoles([]string{plantedFirstDrive}, []IdentityRole{
		{Value: "Ab", Role: "drive-1-account-org", IsName: true},
		{Value: "Äbc", Role: "writable-shortcut", IsName: true},
		{Value: "Abcd", Role: "drive-1-account-name", IsName: true},
	})

	assert.Equal(t, "Ab sync Äbc for drive-1-account-name", roles.Redact("Ab sync Äbc for Abcd"))
	assert.Equal(t, []string{"drive-1-account-org", "writable-shortcut"}, roles.UnlearnedNameRoles())
	assert.Empty(t, plantedSuiteRoles().UnlearnedNameRoles())
}
