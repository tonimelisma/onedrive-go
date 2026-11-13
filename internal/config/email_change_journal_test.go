package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const emailChangeTestNewEmail = "renamed@example.com"

// emailChangeStop cuts a change short with interrupted, as a crash or a
// failed write would.
type emailChangeStop func(changer *emailChanger, interrupted error)

// stopAfterStep stops the change once step is done.
func stopAfterStep(step emailChangeStep) emailChangeStop {
	return func(changer *emailChanger, interrupted error) {
		changer.afterStep = func(done emailChangeStep) error {
			if done == step {
				return interrupted
			}
			return nil
		}
	}
}

// stopAfterStateDBMoves stops the change inside its first step, once count
// of its state databases have moved.
func stopAfterStateDBMoves(t *testing.T, count int) emailChangeStop {
	t.Helper()

	return func(changer *emailChanger, interrupted error) {
		changer.moveStateDBs = func(moves []StateDBMove, begin func() error) error {
			require.NotNil(t, begin, "a new change records itself through the mover")
			if err := begin(); err != nil {
				return err
			}
			require.Greater(t, len(moves), count, "the stop must leave state databases to move")
			for _, move := range moves[:count] {
				require.NoError(t, os.Rename(move.From, move.To))
			}
			return interrupted
		}
	}
}

// interruptEmailChange starts the fixture's email change and cuts it short
// with stop. The change stays on record.
func interruptEmailChange(t *testing.T, fixture *emailReconcileFixture, stop emailChangeStop) {
	t.Helper()

	interrupted := errors.New("interrupted")
	var moved []StateDBMove
	changer := newEmailChanger(stateDBMoverForTest(t, &moved), testLogger(t))
	stop(changer, interrupted)
	_, err := changer.reconcile(fixture.configPath, fixture.newBusiness, emailReconcileTestUser123, emailChangeTestNewEmail)
	require.ErrorIs(t, err, interrupted)
	require.FileExists(t, emailChangeJournalPath(DefaultDataDir()), "the change stays on record until its last step is done")
}

// interruptEmailChangeAfter starts the fixture's email change and cuts it
// short once step is done.
func interruptEmailChangeAfter(t *testing.T, fixture *emailReconcileFixture, step emailChangeStep) {
	t.Helper()

	interruptEmailChange(t, fixture, stopAfterStep(step))
}

// finishEmailChange is the next command of any kind: it completes whatever
// change a journal names.
func finishEmailChange(t *testing.T) (EmailReconcileResult, error) {
	t.Helper()

	var moved []StateDBMove

	return CompleteEmailChange(stateDBMoverForTest(t, &moved), testLogger(t))
}

func readEmailChangeTestFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	require.NoError(t, err)

	return string(data)
}

// assertEmailReconcileFixtureKeptItsState checks that what each renamed file
// and record held before the change is what its new name holds after it.
func assertEmailReconcileFixtureKeptItsState(t *testing.T, fixture *emailReconcileFixture) {
	t.Helper()

	assert.Equal(t, "business-state", readEmailChangeTestFile(t, DriveStatePath(fixture.newBusiness)))
	assert.Equal(t, "sharepoint-state", readEmailChangeTestFile(t, DriveStatePath(fixture.newSharePoint)))
	assert.Equal(t, "shared-state", readEmailChangeTestFile(t, DriveStatePath(fixture.newShared)))
	assert.Equal(t, "personal-state", readEmailChangeTestFile(t, DriveStatePath(fixture.personal)))
	assert.JSONEq(t, `{"token":"old-business"}`, readEmailChangeTestFile(t, DriveTokenPath(fixture.newBusiness)))

	_, oldAccountFound := loadCatalogAccount(t, fixture.oldBusiness)
	assert.False(t, oldAccountFound)
	account, found := loadCatalogAccount(t, fixture.newBusiness)
	require.True(t, found)
	assert.Equal(t, emailReconcileTestUser123, account.UserID)
	assert.Equal(t, emailChangeTestNewEmail, account.Email)
	assert.Equal(t, fixture.newBusiness.String(), account.PrimaryDriveCanonical)

	_, oldDriveFound := loadCatalogDrive(t, fixture.oldBusiness)
	assert.False(t, oldDriveFound)
	drive, found := loadCatalogDrive(t, fixture.newBusiness)
	require.True(t, found)
	assert.Equal(t, emailReconcileTestDriveBusiness, drive.RemoteDriveID)
	assert.Equal(t, fixture.newBusiness.String(), drive.OwnerAccountCanonical)
	sharePoint, found := loadCatalogDrive(t, fixture.newSharePoint)
	require.True(t, found)
	assert.Equal(t, "site-123", sharePoint.SiteID)
	assert.Equal(t, fixture.newBusiness.String(), sharePoint.OwnerAccountCanonical)
}

// An account email change is recorded, then four steps, and a failure after
// any of them, or between two of its state database moves, leaves a journal
// that the next command finishes: every name ends new, and each new name
// holds what the old one held. The table has a row for every step boundary,
// from the first step to the last.
//
// Validates: R-3.7.3
func TestReconcileAccountEmail_AFailureAfterAnyStepIsFinishedByTheNextCommand(t *testing.T) {
	type stopCase struct {
		name string
		stop func(t *testing.T) emailChangeStop
	}
	var cases []stopCase
	for step := emailChangeStepRecorded; step <= emailChangeStepCatalog; step++ {
		cases = append(cases, stopCase{
			name: "after " + step.String(),
			stop: func(*testing.T) emailChangeStop { return stopAfterStep(step) },
		})
	}
	cases = append(cases, stopCase{
		name: "after one state database moved",
		stop: func(t *testing.T) emailChangeStop {
			t.Helper()

			return stopAfterStateDBMoves(t, 1)
		},
	})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := setTestDataDir(t)
			logger := testLogger(t)
			fixture := seedEmailReconcileFixture(t)

			interruptEmailChange(t, &fixture, tc.stop(t))

			result, err := finishEmailChange(t)
			require.NoError(t, err)
			assertEmailReconcileFixtureRenamed(t, logger, &fixture, &result)
			assertEmailReconcileFixtureKeptItsState(t, &fixture)
			assert.NoFileExists(t, emailChangeJournalPath(dataDir), "a finished change leaves no journal")

			again, err := finishEmailChange(t)
			require.NoError(t, err)
			assert.False(t, again.Changed(), "there is nothing left to finish")
		})
	}
}

// A change stopped before it was recorded (the mover failed before it called
// begin, as after it cleared a stranger's sidecars) left every name old and
// no journal: the next command has nothing to finish, and the next
// reconciliation plans the change afresh.
//
// Validates: R-3.7.3
func TestReconcileAccountEmail_AChangeStoppedBeforeItWasRecordedStartsAfresh(t *testing.T) {
	dataDir := setTestDataDir(t)
	logger := testLogger(t)
	fixture := seedEmailReconcileFixture(t)

	unrecorded := errors.New("no room for the journal")
	_, err := ReconcileAccountEmail(fixture.configPath, fixture.newBusiness, emailReconcileTestUser123,
		emailChangeTestNewEmail, func([]StateDBMove, func() error) error { return unrecorded }, logger)
	require.ErrorIs(t, err, unrecorded)
	assert.NoFileExists(t, emailChangeJournalPath(dataDir))

	finished, err := finishEmailChange(t)
	require.NoError(t, err)
	assert.False(t, finished.Changed(), "nothing was recorded, so nothing is finished")

	var moved []StateDBMove
	result, err := ReconcileAccountEmail(fixture.configPath, fixture.newBusiness, emailReconcileTestUser123,
		emailChangeTestNewEmail, stateDBMoverForTest(t, &moved), logger)
	require.NoError(t, err)
	assertEmailReconcileFixtureRenamed(t, logger, &fixture, &result)
	assertEmailReconcileFixtureKeptItsState(t, &fixture)
	assert.NoFileExists(t, emailChangeJournalPath(dataDir))
}

// A command that reaches the reconciliation with a journal still on disk
// finishes that change itself, whether or not the catalog already shows the
// new email, and reports its renames.
//
// Validates: R-3.7.3
func TestReconcileAccountEmail_FinishesAnInterruptedChangeItFinds(t *testing.T) {
	steps := []struct {
		name string
		step emailChangeStep
	}{
		{name: "the catalog still has the old email", step: emailChangeStepTokens},
		{name: "the catalog already has the new email", step: emailChangeStepCatalog},
	}
	for _, tc := range steps {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := setTestDataDir(t)
			logger := testLogger(t)
			fixture := seedEmailReconcileFixture(t)

			interruptEmailChangeAfter(t, &fixture, tc.step)

			var moved []StateDBMove
			result, err := ReconcileAccountEmail(fixture.configPath, fixture.newBusiness, emailReconcileTestUser123,
				emailChangeTestNewEmail, stateDBMoverForTest(t, &moved), logger)
			require.NoError(t, err)
			assertEmailReconcileFixtureRenamed(t, logger, &fixture, &result)
			assertEmailReconcileFixtureKeptItsState(t, &fixture)
			assert.NoFileExists(t, emailChangeJournalPath(dataDir))
		})
	}
}

// A mover that renames without recording the change would leave new names no
// journal accounts for, so its success without a record stops the change
// before any further step.
//
// Validates: R-3.7.3
func TestReconcileAccountEmail_RefusesAMoverThatDidNotRecordTheChange(t *testing.T) {
	dataDir := setTestDataDir(t)
	fixture := seedEmailReconcileFixture(t)

	_, err := ReconcileAccountEmail(fixture.configPath, fixture.newBusiness, emailReconcileTestUser123,
		emailChangeTestNewEmail, func([]StateDBMove, func() error) error { return nil }, testLogger(t))
	require.ErrorContains(t, err, "did not record")

	assert.NoFileExists(t, emailChangeJournalPath(dataDir))
	assert.FileExists(t, DriveTokenPath(fixture.oldBusiness), "no later step ran")
	assert.NoFileExists(t, DriveTokenPath(fixture.newBusiness))
}

// With no journal there is nothing to finish, and looking for one creates
// nothing: not the journal's folder, not even the data directory.
//
// Validates: R-3.7.3
func TestCompleteEmailChange_WithNoJournalTouchesNothing(t *testing.T) {
	setConfigTestHome(t)
	dataDir := DefaultDataDir()

	result, err := CompleteEmailChange(func([]StateDBMove, func() error) error {
		return errors.New("the mover must not run")
	}, testLogger(t))
	require.NoError(t, err)
	assert.False(t, result.Changed())
	assert.NoDirExists(t, dataDir)

	moves, err := UnfinishedEmailChangeStateDBs(dataDir)
	require.NoError(t, err)
	assert.Empty(t, moves)
	assert.NoDirExists(t, dataDir)
}

// An engine asks which state stores an unfinished change names, so that it
// can refuse to start on one of them: every store the change moves, at its
// old and its new path, until the change is finished.
//
// Validates: R-3.7.3
func TestUnfinishedEmailChangeStateDBs_NamesEveryStoreUntilTheChangeIsFinished(t *testing.T) {
	dataDir := setTestDataDir(t)
	fixture := seedEmailReconcileFixture(t)

	interruptEmailChangeAfter(t, &fixture, emailChangeStepStateDBs)

	moves, err := UnfinishedEmailChangeStateDBs(dataDir)
	require.NoError(t, err)
	assert.ElementsMatch(t, []StateDBMove{
		{From: DriveStatePath(fixture.oldBusiness), To: DriveStatePath(fixture.newBusiness)},
		{From: DriveStatePath(fixture.oldSharePoint), To: DriveStatePath(fixture.newSharePoint)},
		{From: DriveStatePath(fixture.oldShared), To: DriveStatePath(fixture.newShared)},
	}, moves)

	_, err = finishEmailChange(t)
	require.NoError(t, err)
	moves, err = UnfinishedEmailChangeStateDBs(dataDir)
	require.NoError(t, err)
	assert.Empty(t, moves)
}

// A journal that cannot be read is not guessed at and not deleted: every
// reader fails and names the file.
//
// Validates: R-3.7.3
func TestEmailChangeJournal_AJournalThatCannotBeReadFailsClosed(t *testing.T) {
	dataDir := setTestDataDir(t)
	journalPath := emailChangeJournalPath(dataDir)
	writeManagedFixture(t, journalPath, []byte("{not a journal"))

	_, err := finishEmailChange(t)
	require.ErrorContains(t, err, journalPath)
	_, err = UnfinishedEmailChangeStateDBs(dataDir)
	require.ErrorContains(t, err, journalPath)
	assert.FileExists(t, journalPath)
}

const emailChangeTestJournal = `{
  "schema_version": 1,
  "config_path": "/etc/onedrive-go/config.toml",
  "account_renames": [{"from": "business:user@example.com", "to": "business:renamed@example.com"}],
  "drive_renames": [{"from": "business:user@example.com", "to": "business:renamed@example.com"}],
  "state_db_moves": [{"from": "state_business_user@example.com.db", "to": "state_business_renamed@example.com.db"}],
  "token_moves": [{"from": "token_business_user@example.com.json", "to": "token_business_renamed@example.com.json"}]
}`

// The journal is decoded strictly, because finishing a change from a record
// that was half understood renames the wrong files. A file it names is a
// plain name in the data directory, never a path: that is what lets a resumed
// rename stay inside that one directory.
//
// Validates: R-3.7.3
func TestDecodeEmailChangeJournal_RefusesWhatItCannotFinishExactly(t *testing.T) {
	t.Parallel()

	journal, err := decodeEmailChangeJournal([]byte(emailChangeTestJournal))
	require.NoError(t, err)
	oldBusiness := driveid.MustCanonicalID("business:user@example.com")
	newBusiness := driveid.MustCanonicalID("business:renamed@example.com")
	assert.Equal(t, []CanonicalIDRename{{From: oldBusiness, To: newBusiness}}, journal.AccountRenames)
	assert.Equal(t, []journalFileMove{{
		From: "token_business_user@example.com.json", To: "token_business_renamed@example.com.json",
	}}, journal.TokenMoves)

	refused := []struct {
		name string
		old  string
		new  string
	}{
		{name: "a field it does not know", old: `"schema_version": 1,`, new: `"schema_version": 1, "undo": true,`},
		{name: "a newer schema", old: `"schema_version": 1,`, new: `"schema_version": 2,`},
		{name: "a config path that is not absolute", old: `"/etc/onedrive-go/config.toml"`, new: `"config.toml"`},
		{name: "no account", old: `[{"from": "business:user@example.com", "to": "business:renamed@example.com"}],
  "drive_renames"`, new: `[],
  "drive_renames"`},
		{name: "an ID that is not one", old: `"to": "business:renamed@example.com"}],
  "state_db_moves"`, new: `"to": "renamed@example.com"}],
  "state_db_moves"`},
		{name: "an ID renamed to itself", old: `"to": "business:renamed@example.com"}],
  "state_db_moves"`, new: `"to": "business:user@example.com"}],
  "state_db_moves"`},
		{
			name: "a state database in another folder", old: `"state_business_renamed@example.com.db"`,
			new: `"elsewhere/state_business_renamed@example.com.db"`,
		},
		{
			name: "a token in a parent folder", old: `"token_business_renamed@example.com.json"`,
			new: `"../token_business_renamed@example.com.json"`,
		},
		{
			name: "a token at an absolute path", old: `"token_business_user@example.com.json"`,
			new: `"/tmp/token_business_user@example.com.json"`,
		},
		{
			name: "a file renamed to itself", old: `"token_business_renamed@example.com.json"`,
			new: `"token_business_user@example.com.json"`,
		},
		{name: "a file with no name", old: `"state_business_user@example.com.db"`, new: `""`},
		{name: "two files renamed to one name", old: `"token_moves": [`, new: `"token_moves": [` +
			`{"from": "token_business_other@example.com.json", "to": "token_business_renamed@example.com.json"}, `},
		{name: "a rename onto a name another rename frees", old: `"token_moves": [`, new: `"token_moves": [` +
			`{"from": "token_business_renamed@example.com.json", "to": "token_business_other@example.com.json"}, `},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			text := strings.Replace(emailChangeTestJournal, tc.old, tc.new, 1)
			require.NotEqual(t, emailChangeTestJournal, text, "the case must change the journal")
			_, decodeErr := decodeEmailChangeJournal([]byte(text))
			require.Error(t, decodeErr)
		})
	}

	_, err = decodeEmailChangeJournal([]byte(emailChangeTestJournal + "{}"))
	require.Error(t, err, "data after the journal is refused")

	newer := strings.Replace(emailChangeTestJournal, `"schema_version": 1,`, `"schema_version": 2,`, 1)
	_, err = decodeEmailChangeJournal([]byte(newer))
	require.ErrorContains(t, err, "newer onedrive-go", "a journal a newer version wrote says who can finish it")
	unversioned := strings.Replace(emailChangeTestJournal, `"schema_version": 1,`, ``, 1)
	_, err = decodeEmailChangeJournal([]byte(unversioned))
	require.ErrorContains(t, err, "unsupported schema version 0")
}

// A planned rename whose file is not directly in the data directory is
// refused when the journal is built. The renames once went by the target's
// base name inside the source's folder, so such a target landed beside its
// source without a word.
//
// Validates: R-3.7.3
func TestDataDirFileName_RefusesAFileOutsideTheDataDir(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()

	name, err := dataDirFileName(dataDir, filepath.Join(dataDir, "token_business_user@example.com.json"))
	require.NoError(t, err)
	assert.Equal(t, "token_business_user@example.com.json", name)

	_, err = dataDirFileName(dataDir, filepath.Join(dataDir, "elsewhere", "token_business_user@example.com.json"))
	require.ErrorContains(t, err, "elsewhere")
	_, err = dataDirFileName(dataDir, filepath.Join(filepath.Dir(dataDir), "token_business_user@example.com.json"))
	require.Error(t, err)
	_, err = dataDirFileName(dataDir, dataDir)
	require.Error(t, err)
}

// A token at both names when a change is resumed: the journal says the token
// already moved, so the one under the old name was written afterwards. Both
// are kept, and the error names the one to remove; with it gone the next
// command finishes.
//
// Validates: R-3.7.3
func TestCompleteEmailChange_ATokenAtBothNamesSaysWhichToRemove(t *testing.T) {
	dataDir := setTestDataDir(t)
	logger := testLogger(t)
	fixture := seedEmailReconcileFixture(t)
	interruptEmailChangeAfter(t, &fixture, emailChangeStepTokens)
	staleToken := DriveTokenPath(fixture.oldBusiness)
	writeManagedFixture(t, staleToken, []byte(`{"token":"written-after-the-change"}`))

	_, err := finishEmailChange(t)
	require.ErrorContains(t, err, staleToken)
	require.ErrorContains(t, err, "remove")
	assert.FileExists(t, emailChangeJournalPath(dataDir), "the change stays unfinished")
	assert.JSONEq(t, `{"token":"old-business"}`, readEmailChangeTestFile(t, DriveTokenPath(fixture.newBusiness)))
	assert.JSONEq(t, `{"token":"written-after-the-change"}`, readEmailChangeTestFile(t, staleToken))

	require.NoError(t, os.Remove(staleToken))
	result, err := finishEmailChange(t)
	require.NoError(t, err)
	assertEmailReconcileFixtureRenamed(t, logger, &fixture, &result)
	assertEmailReconcileFixtureKeptItsState(t, &fixture)
}

// A config section at both names when a change is resumed: the section was
// renamed already, so the one under the old name was added afterwards. The
// config file is left as it is, and the error names the section to remove.
//
// Validates: R-3.7.3
func TestCompleteEmailChange_AConfigSectionAtBothNamesSaysWhichToRemove(t *testing.T) {
	dataDir := setTestDataDir(t)
	logger := testLogger(t)
	fixture := seedEmailReconcileFixture(t)
	interruptEmailChangeAfter(t, &fixture, emailChangeStepConfigSections)
	renamedConfig := readEmailChangeTestFile(t, fixture.configPath)
	withStaleSection := renamedConfig + "\n[\"business:user@example.com\"]\nsync_dir = \"~/Stale\"\n"
	writeConfigFixture(t, fixture.configPath, []byte(withStaleSection))

	_, err := finishEmailChange(t)
	require.ErrorContains(t, err,
		`config sections "business:user@example.com" and "business:renamed@example.com" both exist`)
	require.ErrorContains(t, err, `remove section "business:user@example.com"`)
	require.ErrorContains(t, err, fixture.configPath)
	assert.FileExists(t, emailChangeJournalPath(dataDir), "the change stays unfinished")
	assert.Equal(t, withStaleSection, readEmailChangeTestFile(t, fixture.configPath))

	writeConfigFixture(t, fixture.configPath, []byte(renamedConfig))
	result, err := finishEmailChange(t)
	require.NoError(t, err)
	assertEmailReconcileFixtureRenamed(t, logger, &fixture, &result)
}

// Only the drives the renamed account owns get its new email. A drive of
// another account keeps its ID, also when that account has the same old
// address under another account type.
//
// Validates: R-3.7.2
func TestReconcileAccountEmail_LeavesAnotherAccountsCatalogDrivesAlone(t *testing.T) {
	setTestDataDir(t)
	logger := testLogger(t)
	fixture := seedEmailReconcileFixture(t)
	const otherDriveID = "drive-other"
	other := driveid.MustCanonicalID("business:other@example.com")
	seedCatalogAccount(t, other, func(account *CatalogAccount) {
		account.UserID = "third-user"
		account.PrimaryDriveID = otherDriveID
		account.PrimaryDriveCanonical = other.String()
	})
	seedCatalogDrive(t, other, func(drive *CatalogDrive) {
		drive.RemoteDriveID = otherDriveID
	})
	seedCatalogDrive(t, fixture.personal, func(drive *CatalogDrive) {
		drive.RemoteDriveID = emailReconcileTestDrivePersonal
	})

	var moved []StateDBMove
	result, err := ReconcileAccountEmail(fixture.configPath, fixture.newBusiness, emailReconcileTestUser123,
		emailChangeTestNewEmail, stateDBMoverForTest(t, &moved), logger)
	require.NoError(t, err)
	assertEmailReconcileFixtureRenamed(t, logger, &fixture, &result)
	assertEmailReconcileFixtureKeptItsState(t, &fixture)

	otherDrive, found := loadCatalogDrive(t, other)
	require.True(t, found, "a drive of an account whose email did not change keeps its ID")
	assert.Equal(t, otherDriveID, otherDrive.RemoteDriveID)
	assert.Equal(t, other.String(), otherDrive.OwnerAccountCanonical)
	personalDrive, found := loadCatalogDrive(t, fixture.personal)
	require.True(t, found, "a drive of another account with the same old address keeps its ID")
	assert.Equal(t, emailReconcileTestDrivePersonal, personalDrive.RemoteDriveID)
	assert.Equal(t, fixture.personal.String(), personalDrive.OwnerAccountCanonical)
}

// A config section under the new name before the change starts was there
// first: the change is refused before it is put on record or renames
// anything, and the error names both sections and the file.
//
// Validates: R-3.7.2, R-3.7.3
func TestReconcileAccountEmail_ConfigSectionsAtBothNamesRefuseTheChange(t *testing.T) {
	dataDir := setTestDataDir(t)
	fixture := seedEmailReconcileFixture(t)
	original := readEmailChangeTestFile(t, fixture.configPath)
	withNewSection := original + "\n[\"business:renamed@example.com\"]\nsync_dir = \"~/Renamed\"\n"
	writeConfigFixture(t, fixture.configPath, []byte(withNewSection))

	var moved []StateDBMove
	_, err := ReconcileAccountEmail(fixture.configPath, fixture.newBusiness, emailReconcileTestUser123,
		emailChangeTestNewEmail, stateDBMoverForTest(t, &moved), testLogger(t))
	require.ErrorContains(t, err, `"business:user@example.com" and "business:renamed@example.com" both exist in `+
		fixture.configPath)
	require.ErrorContains(t, err, "remove the one you no longer use")

	assert.Empty(t, moved, "no state database moved")
	assert.NoFileExists(t, emailChangeJournalPath(dataDir), "a refused change is never put on record")
	assert.FileExists(t, DriveTokenPath(fixture.oldBusiness), "the token keeps its name")
	assert.Equal(t, withNewSection, readEmailChangeTestFile(t, fixture.configPath))
}

// Two catalog accounts of one user and account type under two old addresses
// would both be renamed to one ID, and their tokens to one file: the second
// rename would replace the first. The change is refused before anything is
// renamed or recorded.
//
// Validates: R-3.7.2, R-3.7.3
func TestReconcileAccountEmail_RefusesTwoAccountsRenamedToOne(t *testing.T) {
	dataDir := setTestDataDir(t)
	fixture := seedEmailReconcileFixture(t)
	second := driveid.MustCanonicalID("business:earlier@example.com")
	seedCatalogAccount(t, second, func(account *CatalogAccount) {
		account.UserID = emailReconcileTestUser123
		account.PrimaryDriveCanonical = second.String()
	})
	writeManagedFixture(t, DriveTokenPath(second), []byte(`{"token":"earlier"}`))

	var moved []StateDBMove
	_, err := ReconcileAccountEmail(fixture.configPath, fixture.newBusiness, emailReconcileTestUser123,
		emailChangeTestNewEmail, stateDBMoverForTest(t, &moved), testLogger(t))
	require.ErrorContains(t, err, "two renames lead to "+fixture.newBusiness.String())

	assert.Empty(t, moved)
	assert.NoFileExists(t, emailChangeJournalPath(dataDir))
	assert.FileExists(t, DriveTokenPath(fixture.oldBusiness))
	assert.FileExists(t, DriveTokenPath(second))
	assert.NoFileExists(t, DriveTokenPath(fixture.newBusiness))
}

// A journal is built from the reconcile plan with each file by its name in the data
// directory and the config file by its absolute path, so the next command
// finishes the same renames whichever directory it runs in.
//
// Validates: R-3.7.3
func TestReconcileAccountEmail_RecordsTheWholeChangeBeforeTheFirstRename(t *testing.T) {
	dataDir := setTestDataDir(t)
	fixture := seedEmailReconcileFixture(t)

	var recorded emailChangeJournal
	mover := func(moves []StateDBMove, begin func() error) error {
		require.NotNil(t, begin)
		require.NoError(t, begin())
		data, err := os.ReadFile(emailChangeJournalPath(dataDir))
		require.NoError(t, err)
		recorded, err = decodeEmailChangeJournal(data)
		require.NoError(t, err)
		for _, move := range moves {
			assert.FileExists(t, move.From, "nothing is renamed before the change is on record")
		}
		return errors.New("stop after recording")
	}
	_, err := ReconcileAccountEmail(fixture.configPath, fixture.newBusiness, emailReconcileTestUser123,
		emailChangeTestNewEmail, mover, testLogger(t))
	require.ErrorContains(t, err, "stop after recording")

	assert.Equal(t, fixture.configPath, recorded.ConfigPath)
	assert.Equal(t, []CanonicalIDRename{{From: fixture.oldBusiness, To: fixture.newBusiness}}, recorded.AccountRenames)
	assert.ElementsMatch(t, []CanonicalIDRename{
		{From: fixture.oldBusiness, To: fixture.newBusiness},
		{From: fixture.oldSharePoint, To: fixture.newSharePoint},
		{From: fixture.oldShared, To: fixture.newShared},
	}, recorded.DriveRenames)
	assert.Equal(t, []journalFileMove{
		{From: filepath.Base(DriveStatePath(fixture.oldBusiness)), To: filepath.Base(DriveStatePath(fixture.newBusiness))},
		{From: filepath.Base(DriveStatePath(fixture.oldShared)), To: filepath.Base(DriveStatePath(fixture.newShared))},
		{
			From: filepath.Base(DriveStatePath(fixture.oldSharePoint)),
			To:   filepath.Base(DriveStatePath(fixture.newSharePoint)),
		},
	}, recorded.StateDBMoves, "sorted by old name")
	assert.Equal(t, []journalFileMove{{
		From: filepath.Base(DriveTokenPath(fixture.oldBusiness)), To: filepath.Base(DriveTokenPath(fixture.newBusiness)),
	}}, recorded.TokenMoves)
}
