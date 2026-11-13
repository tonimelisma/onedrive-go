package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/fsroot"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// An account email change renames files in the data directory (state
// databases, tokens), sections of the config file and records of the
// catalog: four writes no single rename covers. It is journaled and rolled
// forward. Before the first rename the whole list of renames is written to
// the journal, every step is idempotent, and the journal is deleted after the
// last one. A change cut short at any point leaves the journal, and the next
// command finishes it (CompleteEmailChange) before it reads anything else.
//
// The invariant every refusal rests on: with no journal, nothing of a change
// is at a new name; with a journal, everything at a new name was put there by
// the change it records. So a file at both names when a change is resumed
// was created at the old name afterwards, and that is the one to remove.

const (
	// emailChangeDirName is the data-directory folder that holds the journal.
	// The folder is also the lock that lets one email change run per data
	// directory; it stays, and only the journal in it is deleted.
	emailChangeDirName     = "email-change"
	emailChangeJournalName = "journal.json"
	emailChangeJournalV1   = 1
)

// emailChangeJournal is the record of one account email change. File moves
// name files directly inside the data directory, by base name, so a journal
// cannot rename anything outside it. The whole list is recorded because a
// plan rebuilt after an interruption would see half-renamed inputs. The
// change is finished on the config file it started with, not on the one the
// finishing command uses.
type emailChangeJournal struct {
	SchemaVersion  int                 `json:"schema_version"`
	ConfigPath     string              `json:"config_path"`
	AccountRenames []CanonicalIDRename `json:"account_renames"`
	DriveRenames   []CanonicalIDRename `json:"drive_renames"`
	StateDBMoves   []journalFileMove   `json:"state_db_moves"`
	TokenMoves     []journalFileMove   `json:"token_moves"`
}

// journalFileMove is the rename of one file of the data directory.
type journalFileMove struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// emailChangeStep names the points of a change between two steps, where an
// interruption leaves the journal and the next command finishes the rest.
type emailChangeStep int

const (
	// emailChangeStepRecorded: the journal is written and nothing is renamed.
	emailChangeStepRecorded emailChangeStep = iota + 1
	// emailChangeStepStateDBs: every state database is at its new name.
	emailChangeStepStateDBs
	// emailChangeStepTokens: every token file is at its new name.
	emailChangeStepTokens
	// emailChangeStepConfigSections: every config section has its new name.
	emailChangeStepConfigSections
	// emailChangeStepCatalog: every catalog record has its new key; only the
	// journal remains.
	emailChangeStepCatalog
)

func (s emailChangeStep) String() string {
	switch s {
	case emailChangeStepRecorded:
		return "the change was recorded"
	case emailChangeStepStateDBs:
		return "the state databases moved"
	case emailChangeStepTokens:
		return "the tokens were renamed"
	case emailChangeStepConfigSections:
		return "the config sections were renamed"
	case emailChangeStepCatalog:
		return "the catalog was rewritten"
	default:
		return fmt.Sprintf("email change step %d", int(s))
	}
}

// emailChanger runs account email changes in one data directory.
type emailChanger struct {
	dataDir      string
	moveStateDBs StateDBMover
	logger       *slog.Logger
	// afterStep, when set, is called once each step is done; an error stops
	// the change there, as a crash would. Test-only; nil in production.
	afterStep func(emailChangeStep) error
}

func newEmailChanger(moveStateDBs StateDBMover, logger *slog.Logger) *emailChanger {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &emailChanger{dataDir: DefaultDataDir(), moveStateDBs: moveStateDBs, logger: logger}
}

// CompleteEmailChange finishes the account email change an earlier command
// started and did not finish, if a journal says there is one, and reports
// its renames. The CLI calls it once at its entry, before the command reads
// its config: completing needs the state DB mover of internal/sync, and a
// config read that renames files is a side effect no reader expects. With no
// journal it is one stat: no lock is taken and nothing is created.
func CompleteEmailChange(moveStateDBs StateDBMover, logger *slog.Logger) (EmailReconcileResult, error) {
	return newEmailChanger(moveStateDBs, logger).complete()
}

func (c *emailChanger) complete() (EmailReconcileResult, error) {
	pending, err := c.journalExists()
	if err != nil || !pending {
		return EmailReconcileResult{}, err
	}

	var result EmailReconcileResult
	err = c.withLock(func() error {
		var finishErr error
		result, finishErr = c.finishLocked()

		return finishErr
	})

	return result, err
}

// UnfinishedEmailChangeStateDBs lists the state databases an unfinished
// account email change in dataDir moves, by their old and new paths, so that
// an engine can refuse to start on either. It takes no lock and creates
// nothing: the journal is written by an atomic rename, so a reader sees it
// whole or not at all. A journal that cannot be read is an error, because it
// may name any store.
func UnfinishedEmailChangeStateDBs(dataDir string) ([]StateDBMove, error) {
	journal, found, err := readEmailChangeJournal(dataDir)
	if err != nil || !found {
		return nil, err
	}

	return journal.stateDBMoves(dataDir), nil
}

func emailChangeJournalPath(dataDir string) string {
	if dataDir == "" {
		return ""
	}

	return filepath.Join(dataDir, emailChangeDirName, emailChangeJournalName)
}

func (c *emailChanger) journalExists() (bool, error) {
	path := emailChangeJournalPath(c.dataDir)
	if path == "" {
		return false, nil
	}

	_, err := statManagedPath(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("check for an unfinished account email change: %w", err)
	}
}

// withLock runs fn under the email change lock of the data directory: an
// exclusive lock on the journal's folder, with the bounded wait of every
// config and catalog change. Lock order: this lock, then the store locks
// the state DB mover takes, then the config and catalog locks the later
// steps take; never the reverse.
func (c *emailChanger) withLock(fn func() error) error {
	path := emailChangeJournalPath(c.dataDir)
	if path == "" {
		return errors.New("account email change: no data directory")
	}

	return defaultConfigLocker().withMutationLock(path, true, fn)
}

// finishLocked finishes the change the journal records, if there is one.
// The caller holds the email change lock.
func (c *emailChanger) finishLocked() (EmailReconcileResult, error) {
	journal, found, err := readEmailChangeJournal(c.dataDir)
	if err != nil || !found {
		return EmailReconcileResult{}, err
	}

	for i := range journal.AccountRenames {
		c.logger.Info("finishing an interrupted account email change",
			slog.String("from", journal.AccountRenames[i].From.String()),
			slog.String("to", journal.AccountRenames[i].To.String()),
		)
	}
	if applyErr := c.apply(&journal, nil); applyErr != nil {
		return EmailReconcileResult{}, applyErr
	}

	return journal.result(), nil
}

// apply runs the steps of the change journal records. A non-nil begin starts
// a new change: it writes the journal, and the state DB mover calls it once
// it holds every store and before its first rename. A nil begin finishes a
// change the journal on disk already records. Every step is idempotent, so a
// resumed change runs them all again.
func (c *emailChanger) apply(journal *emailChangeJournal, begin func() error) error {
	if err := c.moveJournalStateDBs(journal, begin); err != nil {
		return err
	}
	if err := c.reached(emailChangeStepStateDBs); err != nil {
		return err
	}

	for _, move := range journal.TokenMoves {
		if err := renameDataDirFile(c.dataDir, move); err != nil {
			return err
		}
	}
	if err := c.reached(emailChangeStepTokens); err != nil {
		return err
	}

	if err := renameJournalDriveSections(journal); err != nil {
		return err
	}
	if err := c.reached(emailChangeStepConfigSections); err != nil {
		return err
	}

	if err := renameCatalogAccounts(c.dataDir, journal.AccountRenames); err != nil {
		return fmt.Errorf("rename catalog accounts: %w", err)
	}
	if err := c.reached(emailChangeStepCatalog); err != nil {
		return err
	}

	return c.removeJournal()
}

// moveJournalStateDBs moves the change's state databases through the mover.
// A new change must be on record before the first rename, so a mover that
// succeeds without calling begin has renamed what no journal accounts for:
// the change stops there.
func (c *emailChanger) moveJournalStateDBs(journal *emailChangeJournal, begin func() error) error {
	moves := journal.stateDBMoves(c.dataDir)
	switch {
	case len(moves) == 0 && begin == nil:
		return nil
	case len(moves) == 0:
		return begin()
	case begin == nil:
		// The mover is the caller's and wraps its own error.
		return c.moveStateDBs(moves, nil)
	}

	recorded := false
	record := func() error {
		if err := begin(); err != nil {
			return err
		}
		recorded = true

		return nil
	}
	if err := c.moveStateDBs(moves, record); err != nil {
		return err
	}
	if !recorded {
		return errors.New("account email change: the state database mover did not record the change before it renamed")
	}

	return nil
}

// reached reports a finished step to the test hook.
func (c *emailChanger) reached(step emailChangeStep) error {
	if c.afterStep == nil {
		return nil
	}
	if err := c.afterStep(step); err != nil {
		return fmt.Errorf("account email change stopped after %s: %w", step, err)
	}

	return nil
}

// writeJournal records a new change. The journal's folder is created on the
// way, and the data directory is synced so that the folder's entry is as
// durable as the renames that follow it.
func (c *emailChanger) writeJournal(journal *emailChangeJournal) error {
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the account email change journal: %w", err)
	}

	path := emailChangeJournalPath(c.dataDir)
	if writeErr := atomicWriteFile(path, data); writeErr != nil {
		return fmt.Errorf("write the account email change journal %s: %w", path, writeErr)
	}

	return syncDataDir(c.dataDir)
}

// removeJournal ends a finished change. The state databases and tokens were
// renamed inside the data directory, so it is synced first: the record that
// lists those renames must not outlive them. A journal that is already gone
// is no error.
func (c *emailChanger) removeJournal() error {
	if err := syncDataDir(c.dataDir); err != nil {
		return err
	}

	path := emailChangeJournalPath(c.dataDir)
	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return fmt.Errorf("open the account email change journal %s: %w", path, err)
	}
	if removeErr := root.Remove(name); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("remove the account email change journal %s: %w", path, removeErr)
	}

	return nil
}

// syncDataDir flushes the data directory's entries. A filesystem that cannot
// sync a directory is accepted, as every atomic write here accepts it: the
// renames are visible, only their durability across a power cut is unknown.
func syncDataDir(dataDir string) error {
	if err := localpath.SyncDir(dataDir); err != nil && !localpath.IsDirSyncUnsupported(err) {
		return fmt.Errorf("sync the data directory: %w", err)
	}

	return nil
}

func readEmailChangeJournal(dataDir string) (emailChangeJournal, bool, error) {
	path := emailChangeJournalPath(dataDir)
	if path == "" {
		return emailChangeJournal{}, false, nil
	}

	data, err := readManagedFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return emailChangeJournal{}, false, nil
	}
	if err != nil {
		return emailChangeJournal{}, false, fmt.Errorf("read the account email change journal %s: %w", path, err)
	}

	journal, err := decodeEmailChangeJournal(data)
	if err != nil {
		return emailChangeJournal{}, false, fmt.Errorf("read the account email change journal %s: %w", path, err)
	}

	return journal, true, nil
}

// decodeEmailChangeJournal reads a journal strictly: an unknown field, data
// after it, another schema version or an entry that fails validation is an
// error, because finishing a change from a record that was half understood
// renames the wrong files.
func decodeEmailChangeJournal(data []byte) (emailChangeJournal, error) {
	var journal emailChangeJournal
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return emailChangeJournal{}, fmt.Errorf("decode: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return emailChangeJournal{}, errors.New("decode: data after the journal")
	}
	if err := journal.validate(); err != nil {
		return emailChangeJournal{}, err
	}

	return journal, nil
}

// validate holds what a journal must be for its change to be finished
// exactly. A journal is validated when it is built and again when it is read.
func (j *emailChangeJournal) validate() error {
	if j.SchemaVersion > emailChangeJournalV1 {
		return fmt.Errorf("schema version %d is newer than this onedrive-go reads (%d): the newer onedrive-go "+
			"that started the change must finish it, so run any command with that version",
			j.SchemaVersion, emailChangeJournalV1)
	}
	if j.SchemaVersion != emailChangeJournalV1 {
		return fmt.Errorf("unsupported schema version %d", j.SchemaVersion)
	}
	if !filepath.IsAbs(j.ConfigPath) {
		return fmt.Errorf("config path %q is not absolute", j.ConfigPath)
	}
	if len(j.AccountRenames) == 0 {
		return errors.New("no account is renamed")
	}

	if err := validateJournalIDRenames(j.AccountRenames); err != nil {
		return err
	}
	if err := validateJournalIDRenames(j.DriveRenames); err != nil {
		return err
	}
	if err := validateJournalFileMoves(j.StateDBMoves); err != nil {
		return err
	}

	return validateJournalFileMoves(j.TokenMoves)
}

// validateJournalIDRenames refuses an empty ID, an ID renamed to itself, and
// two renames that meet: one target for two sources, or a target that is
// another rename's source.
func validateJournalIDRenames(renames []CanonicalIDRename) error {
	pairs := make([][2]string, 0, len(renames))
	for i := range renames {
		rename := &renames[i]
		if rename.From.IsZero() || rename.To.IsZero() {
			return errors.New("a rename names no ID")
		}
		pairs = append(pairs, [2]string{rename.From.String(), rename.To.String()})
	}

	return validateRenamePairs(pairs)
}

// validateJournalFileMoves refuses a name that is not a plain file name of
// the data directory, a file renamed to itself, and moves that meet.
func validateJournalFileMoves(moves []journalFileMove) error {
	pairs := make([][2]string, 0, len(moves))
	for _, move := range moves {
		for _, name := range []string{move.From, move.To} {
			if !isPlainFileName(name) {
				return fmt.Errorf("%q is not a file name in the data directory", name)
			}
		}
		pairs = append(pairs, [2]string{move.From, move.To})
	}

	return validateRenamePairs(pairs)
}

func validateRenamePairs(pairs [][2]string) error {
	sources := make(map[string]struct{}, len(pairs))
	targets := make(map[string]struct{}, len(pairs))
	for _, pair := range pairs {
		from, to := pair[0], pair[1]
		if from == to {
			return fmt.Errorf("%s is renamed to itself", from)
		}
		if _, seen := sources[from]; seen {
			return fmt.Errorf("%s is renamed twice", from)
		}
		if _, seen := targets[to]; seen {
			return fmt.Errorf("two renames lead to %s", to)
		}
		sources[from] = struct{}{}
		targets[to] = struct{}{}
	}
	for to := range targets {
		if _, isSource := sources[to]; isSource {
			return fmt.Errorf("%s is both renamed and renamed to", to)
		}
	}

	return nil
}

// isPlainFileName reports a name that stays in the folder it is joined to:
// one element that is not empty, "." or "..".
func isPlainFileName(name string) bool {
	return fs.ValidPath(name) && name != "." && !strings.ContainsRune(name, '/')
}

// dataDirFileName is the journal's name for path, a file directly inside
// dataDir. Anything else is refused when the journal is built: the renames
// once went by the target's base name inside the source's folder, so a
// target in another folder landed beside its source without a word.
func dataDirFileName(dataDir string, path string) (string, error) {
	name := filepath.Base(path)
	if filepath.Clean(filepath.Dir(path)) != filepath.Clean(dataDir) || !isPlainFileName(name) {
		return "", fmt.Errorf("account email change: %s is not a file directly in the data directory %s", path, dataDir)
	}

	return name, nil
}

// stateDBMoves is the journal's state database moves as paths in dataDir.
func (j *emailChangeJournal) stateDBMoves(dataDir string) []StateDBMove {
	moves := make([]StateDBMove, 0, len(j.StateDBMoves))
	for _, move := range j.StateDBMoves {
		moves = append(moves, StateDBMove{From: filepath.Join(dataDir, move.From), To: filepath.Join(dataDir, move.To)})
	}

	return moves
}

func (j *emailChangeJournal) result() EmailReconcileResult {
	return EmailReconcileResult{
		AccountRenames: slices.Clone(j.AccountRenames),
		DriveRenames:   slices.Clone(j.DriveRenames),
	}
}

// renameDataDirFile renames one file of the data directory for a change. A
// missing source is done already (or there was nothing to rename). A file at
// both names is refused and both are kept: the journal says the source was
// renamed, so the file under the old name was written afterwards.
func renameDataDirFile(dataDir string, move journalFileMove) error {
	root, err := fsroot.Open(dataDir)
	if err != nil {
		return fmt.Errorf("open the data directory %s: %w", dataDir, err)
	}

	sourceExists, err := rootFileExists(root, move.From)
	if err != nil || !sourceExists {
		return err
	}
	targetExists, err := rootFileExists(root, move.To)
	if err != nil {
		return err
	}
	if targetExists {
		source := filepath.Join(dataDir, move.From)
		return fmt.Errorf("finish the account email change: %s already exists, and the change renamed %s to it; "+
			"%s was written after the account's email changed: remove %s and run the command again",
			filepath.Join(dataDir, move.To), source, source, source)
	}

	if renameErr := root.Rename(move.From, move.To); renameErr != nil {
		return fmt.Errorf("rename %s for the account email change: %w", filepath.Join(dataDir, move.From), renameErr)
	}

	return nil
}

func rootFileExists(root *fsroot.Root, name string) (bool, error) {
	_, err := root.Stat(name)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("check %s for the account email change: %w", name, err)
	}
}

// renameJournalDriveSections renames the change's config sections. A
// section at both names when a change is resumed was added under the old
// name after the change renamed it; the file is left as it is.
func renameJournalDriveSections(journal *emailChangeJournal) error {
	renames := make(map[driveid.CanonicalID]driveid.CanonicalID, len(journal.DriveRenames))
	for i := range journal.DriveRenames {
		renames[journal.DriveRenames[i].From] = journal.DriveRenames[i].To
	}

	err := RenameDriveSections(journal.ConfigPath, renames)
	var collision *driveSectionCollisionError
	if errors.As(err, &collision) {
		return fmt.Errorf("finish the account email change: config sections %q and %q both exist in %s; the change "+
			"renamed the first to the second, so the one under the old name was added after the account's email "+
			"changed: remove section %q from %s and run the command again",
			collision.from, collision.to, journal.ConfigPath, collision.from, journal.ConfigPath)
	}
	if err != nil {
		return fmt.Errorf("rename drive sections: %w", err)
	}

	return nil
}
