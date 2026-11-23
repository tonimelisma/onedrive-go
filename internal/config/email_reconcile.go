package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// CanonicalIDRename records one exact canonical-ID rewrite.
type CanonicalIDRename struct {
	From driveid.CanonicalID `json:"from"`
	To   driveid.CanonicalID `json:"to"`
}

// StateDBMove is one state DB an account email change renames, by path.
type StateDBMove struct {
	From string
	To   string
	// FromMountID and ToMountID are the mount IDs of the drive whose own
	// store this is, before and after the change, or empty for a store that
	// is no drive's own. A drive's shortcut children name their stores and
	// their records by ChildMountID of that mount ID, which holds the email,
	// so they move with it (syncengine.MoveStateDBs).
	FromMountID string
	ToMountID   string
}

// StateDBMover renames state DBs, each with its whole file family, under the
// lock that excludes every engine using them, and refuses every move while
// any of them is in use. internal/sync owns that lock and the family
// (syncengine.MoveStateDBs); config plans the renames but cannot import it,
// so the caller passes the mover in.
//
// A non-nil begin means the moves start a new change: the mover calls it
// once, holding every source and target store lock, after its checks pass
// and before its first rename, and stops with its error. begin writes the
// change's journal, so a store in use refuses with no journal and nothing
// renamed. A nil begin means a journal already records these moves and the
// mover finishes them.
type StateDBMover func(moves []StateDBMove, begin func() error) error

// EmailReconcileResult reports the deterministic renames applied for one
// account email change. CLI callers use this to remap exact selectors for the
// current invocation after config/data have been rewritten.
type EmailReconcileResult struct {
	AccountRenames []CanonicalIDRename
	DriveRenames   []CanonicalIDRename
	// KeptStateDBs are shared folders' state DBs left under the old email:
	// two accounts of different types had that email, and no catalog record
	// says which of them the folder belongs to. The CLI names each to the
	// user; nothing else reads it.
	KeptStateDBs []string
}

// Changed reports whether any persisted artifact was rewritten.
func (r *EmailReconcileResult) Changed() bool {
	if r == nil {
		return false
	}

	return len(r.AccountRenames) > 0 || len(r.DriveRenames) > 0
}

// RemapCanonicalID returns the renamed canonical ID for the current run when
// the exact old ID was rewritten.
func (r *EmailReconcileResult) RemapCanonicalID(cid driveid.CanonicalID) (driveid.CanonicalID, bool) {
	if r == nil {
		return driveid.CanonicalID{}, false
	}

	for i := range r.DriveRenames {
		if r.DriveRenames[i].From.Equal(cid) {
			return r.DriveRenames[i].To, true
		}
	}
	for i := range r.AccountRenames {
		if r.AccountRenames[i].From.Equal(cid) {
			return r.AccountRenames[i].To, true
		}
	}

	return driveid.CanonicalID{}, false
}

// ReconcileAccountEmail detects an account rename by matching the stable Graph
// user GUID to stored catalog accounts of the same account type. When the
// current Graph email differs, it renames the account's state DBs, token
// files, config sections and catalog records from the old email to the
// current one, as one journaled change (email_change_journal.go): the state
// DBs move first, through moveStateDBs, and one an engine still has open
// refuses the whole change before anything is renamed or recorded. A change
// an earlier command left unfinished is finished first.
func ReconcileAccountEmail(
	ctx context.Context,
	configPath string,
	currentAccount driveid.CanonicalID,
	userID string,
	currentEmail string,
	moveStateDBs StateDBMover,
	logger *slog.Logger,
) (EmailReconcileResult, error) {
	return newEmailChanger(moveStateDBs, logger).reconcile(ctx, configPath, currentAccount, userID, currentEmail)
}

func (c *emailChanger) reconcile(
	ctx context.Context,
	configPath string,
	currentAccount driveid.CanonicalID,
	userID string,
	currentEmail string,
) (EmailReconcileResult, error) {
	if currentAccount.IsZero() || userID == "" || currentEmail == "" {
		return EmailReconcileResult{}, nil
	}
	if !currentAccount.IsPersonal() && !currentAccount.IsBusiness() {
		return EmailReconcileResult{}, errors.New("email reconciliation requires a personal or business account CID")
	}

	// The common case is no change and no journal: answer it without the lock.
	pending, err := c.journalExists()
	if err != nil {
		return EmailReconcileResult{}, err
	}
	if !pending {
		accountRenames, matchErr := matchingAccountRenames(currentAccount, userID, currentEmail, c.logger)
		if matchErr != nil || len(accountRenames) == 0 {
			return EmailReconcileResult{}, matchErr
		}
	}

	var result EmailReconcileResult
	err = c.withLock(ctx, func() error {
		finished, finishErr := c.finishLocked(ctx)
		if finishErr != nil {
			return finishErr
		}
		started, startErr := c.startLocked(ctx, configPath, currentAccount, userID, currentEmail)
		if startErr != nil {
			return startErr
		}
		result = EmailReconcileResult{
			AccountRenames: slices.Concat(finished.AccountRenames, started.AccountRenames),
			DriveRenames:   slices.Concat(finished.DriveRenames, started.DriveRenames),
			KeptStateDBs:   started.KeptStateDBs,
		}

		return nil
	})

	return result, err
}

// startLocked plans a new change from the catalog as it is now, refuses it
// while any new name is taken, and runs it. The caller holds the email
// change lock and has finished any change a journal records.
func (c *emailChanger) startLocked(
	ctx context.Context,
	configPath string,
	currentAccount driveid.CanonicalID,
	userID string,
	currentEmail string,
) (EmailReconcileResult, error) {
	accountRenames, err := matchingAccountRenames(currentAccount, userID, currentEmail, c.logger)
	if err != nil || len(accountRenames) == 0 {
		return EmailReconcileResult{}, err
	}

	plan, err := buildEmailReconcilePlan(configPath, accountRenames, currentEmail, c.logger)
	if err != nil {
		return EmailReconcileResult{}, err
	}
	if validateErr := plan.validate(); validateErr != nil {
		return EmailReconcileResult{}, validateErr
	}
	journal, err := plan.journal(c.dataDir, configPath)
	if err != nil {
		return EmailReconcileResult{}, err
	}

	begin := func() error {
		if writeErr := c.writeJournal(journal); writeErr != nil {
			return writeErr
		}

		return c.reached(emailChangeStepRecorded)
	}
	if applyErr := c.apply(ctx, journal, begin); applyErr != nil {
		return EmailReconcileResult{}, applyErr
	}

	result := journal.result()
	result.KeptStateDBs = slices.Clone(plan.keptStateDBs)

	return result, nil
}

type emailReconcilePlan struct {
	accountRenameMap map[driveid.CanonicalID]driveid.CanonicalID
	driveRenameMap   map[driveid.CanonicalID]driveid.CanonicalID
	pathRenames      []managedPathRename
	// keptStateDBs are the shared stores the change cannot assign to an
	// account (EmailReconcileResult.KeptStateDBs).
	keptStateDBs []string
}

type managedPathRename struct {
	source string
	target string
	// isStateDB marks a state DB, which moves with its whole file family
	// under its store lock (StateDBMover), never by a plain rename here.
	isStateDB bool
}

// journal is the record of the change p makes: every file rename by its name
// in dataDir, sorted, so that a resumed change renames the same files and
// nothing outside the data directory.
func (p *emailReconcilePlan) journal(dataDir string, configPath string) (*emailChangeJournal, error) {
	absConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("account email change: resolve config path %s: %w", configPath, err)
	}

	journal := &emailChangeJournal{
		SchemaVersion:  emailChangeJournalV1,
		ConfigPath:     absConfigPath,
		AccountRenames: p.accountRenames(),
		DriveRenames:   p.driveRenames(),
		StateDBMoves:   []journalFileMove{},
		TokenMoves:     []journalFileMove{},
	}
	if journal.DriveRenames == nil {
		journal.DriveRenames = []CanonicalIDRename{}
	}
	for _, rename := range p.pathRenames {
		from, fromErr := dataDirFileName(dataDir, rename.source)
		if fromErr != nil {
			return nil, fromErr
		}
		to, toErr := dataDirFileName(dataDir, rename.target)
		if toErr != nil {
			return nil, toErr
		}
		move := journalFileMove{From: from, To: to}
		if rename.isStateDB {
			journal.StateDBMoves = append(journal.StateDBMoves, move)
		} else {
			journal.TokenMoves = append(journal.TokenMoves, move)
		}
	}
	byFrom := func(a, b journalFileMove) int { return strings.Compare(a.From, b.From) }
	slices.SortFunc(journal.StateDBMoves, byFrom)
	slices.SortFunc(journal.TokenMoves, byFrom)

	if validateErr := journal.validate(); validateErr != nil {
		return nil, fmt.Errorf("account email change: %w", validateErr)
	}

	return journal, nil
}

func (p *emailReconcilePlan) accountRenames() []CanonicalIDRename {
	return canonicalRenameSlice(p.accountRenameMap)
}

func (p *emailReconcilePlan) driveRenames() []CanonicalIDRename {
	return canonicalRenameSlice(p.driveRenameMap)
}

func canonicalRenameSlice(m map[driveid.CanonicalID]driveid.CanonicalID) []CanonicalIDRename {
	if len(m) == 0 {
		return nil
	}

	keys := make([]driveid.CanonicalID, 0, len(m))
	for from := range m {
		keys = append(keys, from)
	}

	slices.SortFunc(keys, func(a, b driveid.CanonicalID) int {
		return strings.Compare(a.String(), b.String())
	})

	out := make([]CanonicalIDRename, 0, len(keys))
	for _, from := range keys {
		out = append(out, CanonicalIDRename{
			From: from,
			To:   m[from],
		})
	}

	return out
}

func matchingAccountRenames(
	currentAccount driveid.CanonicalID,
	userID string,
	currentEmail string,
	logger *slog.Logger,
) (map[driveid.CanonicalID]driveid.CanonicalID, error) {
	renames := make(map[driveid.CanonicalID]driveid.CanonicalID)
	catalog, err := LoadCatalog()
	if err != nil {
		return nil, fmt.Errorf("loading catalog for email reconciliation: %w", err)
	}

	for _, key := range catalog.SortedAccountKeys() {
		account := catalog.Accounts[key]
		cid, err := driveid.NewCanonicalID(account.CanonicalID)
		if err != nil {
			logger.Debug("skipping malformed catalog account during email reconciliation",
				"canonical_id", account.CanonicalID,
				"error", err,
			)
			continue
		}
		if cid.DriveType() != currentAccount.DriveType() {
			continue
		}
		if cid == currentAccount && account.UserID != "" && account.UserID != userID {
			// The saved sign-in answers as another user than the one the
			// catalog recorded for it: no rename follows from it, and saying
			// so is the only way the mismatch reaches anyone.
			logger.Warn("saved sign-in answers as a different user than the catalog records",
				"account", cid.String(),
			)
			continue
		}
		if account.UserID != userID || cid.Email() == currentEmail {
			continue
		}

		updated, err := cid.WithEmail(currentEmail)
		if err != nil {
			return nil, fmt.Errorf("rewrite account canonical ID %s: %w", cid, err)
		}

		renames[cid] = updated
	}

	return renames, nil
}

func buildEmailReconcilePlan(
	configPath string,
	accountRenames map[driveid.CanonicalID]driveid.CanonicalID,
	currentEmail string,
	logger *slog.Logger,
) (*emailReconcilePlan, error) {
	plan := &emailReconcilePlan{
		accountRenameMap: maps.Clone(accountRenames),
		driveRenameMap:   make(map[driveid.CanonicalID]driveid.CanonicalID),
	}

	ownedOldAccounts := make(map[string]driveid.CanonicalID, len(accountRenames))
	oldEmails := make([]string, 0, len(accountRenames))
	for from := range accountRenames {
		ownedOldAccounts[from.String()] = from
		oldEmails = append(oldEmails, from.Email())
		addPathRename(plan, DriveTokenPath(from), DriveTokenPath(accountRenames[from]))
	}

	if err := collectConfiguredDriveRenames(plan, configPath, ownedOldAccounts, currentEmail); err != nil {
		return nil, err
	}

	for from, to := range plan.driveRenameMap {
		addStateDBRename(plan, DriveStatePath(from), DriveStatePath(to))
	}

	catalog, err := LoadCatalog()
	if err != nil {
		return nil, fmt.Errorf("loading catalog for email reconciliation: %w", err)
	}
	for _, oldEmail := range oldEmails {
		if err := collectStateRenames(plan, catalog, oldEmail, currentEmail, ownedOldAccounts, logger); err != nil {
			return nil, err
		}
	}

	return plan, nil
}

func collectConfiguredDriveRenames(
	plan *emailReconcilePlan,
	configPath string,
	ownedOldAccounts map[string]driveid.CanonicalID,
	currentEmail string,
) error {
	data, err := readManagedFile(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("read config for email reconciliation: %w", err)
	}

	lines := parseLines(string(data))
	existingSections := make(map[string]struct{})
	for _, line := range lines {
		if line.kind == lineSection {
			existingSections[line.sectionName] = struct{}{}
		}
	}

	for _, line := range lines {
		if line.kind != lineSection {
			continue
		}

		cid, err := driveid.NewCanonicalID(line.sectionName)
		if err != nil {
			continue
		}

		owner, err := TokenAccountCanonicalID(cid)
		if err != nil {
			return fmt.Errorf("resolve token owner for %s: %w", cid, err)
		}
		if _, ok := ownedOldAccounts[owner.String()]; !ok {
			continue
		}

		updated, err := cid.WithEmail(currentEmail)
		if err != nil {
			return fmt.Errorf("rewrite configured drive ID %s: %w", cid, err)
		}

		if cid.Equal(updated) {
			continue
		}

		// cid is a section of this file, so both names are taken.
		if _, exists := existingSections[updated.String()]; exists {
			return fmt.Errorf("account email change: config sections %q and %q both exist in %s, so the first "+
				"cannot be renamed to the second: remove the one you no longer use and run the command again",
				cid.String(), updated.String(), configPath)
		}

		plan.driveRenameMap[cid] = updated
	}

	return nil
}

// collectStateRenames plans the move of every state DB under oldEmail that
// belongs to a renamed account. A personal store follows a renamed personal
// account and a work or SharePoint store a renamed work or school account.
// A shared folder's store follows its catalog owner; with no catalog record
// its name is the evidence when one account had the old email, and it moves.
// When two accounts of different types had it and no record says which owns
// the folder, the store stays under the old email and the plan reports it.
func collectStateRenames(
	plan *emailReconcilePlan,
	catalog *Catalog,
	oldEmail string,
	currentEmail string,
	ownedOldAccounts map[string]driveid.CanonicalID,
	logger *slog.Logger,
) error {
	paths, err := DiscoverStateDBsForEmail(oldEmail, logger)
	if err != nil {
		return fmt.Errorf("planning state database renames for %s: %w", oldEmail, err)
	}
	for _, path := range paths {
		if pathRenameExists(plan.pathRenames, path) {
			continue
		}

		name := filepath.Base(path)
		switch managedFileDriveType(name, stateDBPrefix, stateDBSuffix) {
		case driveid.DriveTypePersonal:
			if ownedOldAccountOfType(ownedOldAccounts, driveid.DriveTypePersonal, oldEmail) {
				addStateDBRename(plan, path, rewriteManagedEmailPath(path, oldEmail, currentEmail))
			}
		case driveid.DriveTypeBusiness, driveid.DriveTypeSharePoint:
			if ownedOldAccountOfType(ownedOldAccounts, driveid.DriveTypeBusiness, oldEmail) {
				addStateDBRename(plan, path, rewriteManagedEmailPath(path, oldEmail, currentEmail))
			}
		case driveid.DriveTypeShared:
			targeted, assigned := sharedStateOwnedByRenamedAccount(catalog, path, oldEmail, ownedOldAccounts)
			if !assigned {
				plan.keptStateDBs = append(plan.keptStateDBs, path)
				continue
			}
			if targeted {
				addStateDBRename(plan, path, rewriteManagedEmailPath(path, oldEmail, currentEmail))
			}
		}
	}

	return nil
}

// renameCatalogAccounts is the catalog step of an email change: one atomic
// catalog write that re-keys each renamed account and the drives it owns.
// A drive of any other account keeps its key, even one under the same old
// address with another account type. The step is idempotent: an account
// already at its new key, whose drives already name it as their owner, is
// left as it is.
func renameCatalogAccounts(ctx context.Context, dataDir string, accountRenames []CanonicalIDRename) error {
	return UpdateCatalogForDataDir(ctx, dataDir, func(catalog *Catalog) error {
		for i := range accountRenames {
			if err := renameCatalogAccount(catalog, &accountRenames[i]); err != nil {
				return err
			}
		}

		return nil
	})
}

func renameCatalogAccount(catalog *Catalog, rename *CanonicalIDRename) error {
	from, to := rename.From, rename.To
	if account, found := catalog.AccountByCanonicalID(from); found {
		catalog.DeleteAccount(from)
		account.FormerEmails = account.withFormerEmail(from.Email(), to.Email())
		account.CanonicalID = to.String()
		account.Email = to.Email()
		account.DriveType = to.DriveType()
		if account.PrimaryDriveCanonical == from.String() {
			account.PrimaryDriveCanonical = to.String()
		}
		catalog.UpsertAccount(&account)
	}

	for _, key := range catalog.SortedDriveKeys() {
		drive := catalog.Drives[key]
		if drive.OwnerAccountCanonical != from.String() {
			continue
		}
		drive.OwnerAccountCanonical = to.String()

		// The catalog decoder and encoder refuse a record whose key does not
		// parse, so a failure here is a catalog built past those checks.
		driveCID, err := driveid.NewCanonicalID(drive.CanonicalID)
		if err != nil {
			return fmt.Errorf("rename catalog drive %q: %w", drive.CanonicalID, err)
		}
		// A drive not under the account's old address names no file this
		// change renames: it keeps its key and only learns its owner's new ID.
		if driveCID.Email() != from.Email() {
			catalog.UpsertDrive(&drive)
			continue
		}
		renamed, err := driveCID.WithEmail(to.Email())
		if err != nil {
			return fmt.Errorf("rewrite catalog drive ID %s: %w", driveCID, err)
		}
		catalog.DeleteDrive(driveCID)
		drive.DisplayName = followedDisplayName(drive.DisplayName, driveCID, renamed)
		drive.CanonicalID = renamed.String()
		drive.DriveType = renamed.DriveType()
		catalog.UpsertDrive(&drive)
	}

	return nil
}

func ownedOldAccountOfType(
	ownedOldAccounts map[string]driveid.CanonicalID,
	driveType string,
	email string,
) bool {
	for _, cid := range ownedOldAccounts {
		if cid.DriveType() == driveType && cid.Email() == email {
			return true
		}
	}

	return false
}

func managedFileDriveType(name, prefix, suffix string) string {
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return ""
	}

	inner := strings.TrimPrefix(name, prefix)
	inner = strings.TrimSuffix(inner, suffix)

	parts := strings.SplitN(inner, "_", 2)
	if len(parts) != 2 {
		return ""
	}

	return parts[0]
}

func pathRenameExists(renames []managedPathRename, source string) bool {
	for i := range renames {
		if renames[i].source == source {
			return true
		}
	}

	return false
}

// addPathRename plans the rename of one managed file (a token).
func addPathRename(plan *emailReconcilePlan, source string, target string) {
	addManagedRename(plan, managedPathRename{source: source, target: target})
}

// addStateDBRename plans the move of one state DB.
func addStateDBRename(plan *emailReconcilePlan, source string, target string) {
	addManagedRename(plan, managedPathRename{source: source, target: target, isStateDB: true})
}

func addManagedRename(plan *emailReconcilePlan, rename managedPathRename) {
	if rename.source == "" || rename.target == "" || rename.source == rename.target {
		return
	}

	if pathRenameExists(plan.pathRenames, rename.source) {
		return
	}

	plan.pathRenames = append(plan.pathRenames, rename)
}

func rewriteManagedEmailPath(path string, oldEmail string, newEmail string) string {
	dir := filepath.Dir(path)
	name := filepath.Base(path)
	needle := "_" + oldEmail
	idx := strings.Index(name, needle)
	if idx < 0 {
		return path
	}

	afterEmail := idx + len(needle)
	if afterEmail < len(name) {
		switch name[afterEmail] {
		case '.', '_':
		default:
			return path
		}
	}

	rewritten := name[:idx] + "_" + newEmail + name[afterEmail:]

	return filepath.Join(dir, rewritten)
}

// sharedStateOwnedByRenamedAccount reports whether the shared folder's store
// at path belongs to a renamed account, and whether that could be decided.
// The catalog record's owner decides; without a record the store is the one
// account's that had oldEmail, when there was one account.
func sharedStateOwnedByRenamedAccount(
	catalog *Catalog,
	path string,
	oldEmail string,
	ownedOldAccounts map[string]driveid.CanonicalID,
) (owned, assigned bool) {
	for _, key := range catalog.SortedDriveKeys() {
		drive := catalog.Drives[key]
		driveCID, cidErr := driveid.NewCanonicalID(drive.CanonicalID)
		if cidErr != nil || DriveStatePath(driveCID) != path {
			continue
		}

		_, ok := ownedOldAccounts[drive.OwnerAccountCanonical]
		return ok, true
	}

	accountsAtOldEmail := 0
	for _, key := range catalog.SortedAccountKeys() {
		if catalog.Accounts[key].Email == oldEmail {
			accountsAtOldEmail++
		}
	}
	if accountsAtOldEmail > 1 {
		return false, false
	}

	return true, true
}

// validate refuses a new change while any new name is taken, before
// anything is renamed or recorded: with no journal, a file at a new name was
// there before the change, and renaming onto it would merge two owners.
func (p *emailReconcilePlan) validate() error {
	for _, rename := range p.pathRenames {
		if err := validateManagedRename(rename.source, rename.target); err != nil {
			return err
		}
	}

	return nil
}

// validateManagedRename refuses a rename whose source and target both exist.
// addManagedRename keeps empty and identical paths out of a reconcile plan.
func validateManagedRename(source string, target string) error {
	_, sourceErr := statManagedPath(source)
	if errors.Is(sourceErr, os.ErrNotExist) {
		return nil
	}
	if sourceErr != nil {
		return fmt.Errorf("stat source %s: %w", source, sourceErr)
	}

	_, targetErr := statManagedPath(target)
	switch {
	case errors.Is(targetErr, os.ErrNotExist):
		return nil
	case targetErr != nil:
		return fmt.Errorf("stat target %s: %w", target, targetErr)
	default:
		return fmt.Errorf("account email change target already exists: %s; it was there before the email "+
			"changed, so %s cannot be renamed to it: remove %s and run the command again", target, source, target)
	}
}

// CatchUpConfigFromFormerEmails renames the sections of the config file at
// configPath that still name a drive under a former email of an account
// (CatalogAccount.FormerEmails) to the account's email now. An account email
// change renames the sections of the one config file its command ran with; a
// second config file sharing the data directory is caught up here, at the
// CLI's entry, the first time a command uses it. A former email that an
// account has now names that account's drives and is never caught up. A
// shared folder's section under an email two accounts had is left as it is,
// since nothing says whose it is. The rename is the same idempotent, locked
// section rename the change itself makes (RenameDriveSections), which also
// carries a stored default display name over to the new address.
func CatchUpConfigFromFormerEmails(ctx context.Context, configPath string) (EmailReconcileResult, error) {
	catalog, err := LoadCatalog()
	if err != nil {
		return EmailReconcileResult{}, fmt.Errorf("load the catalog to catch up %s: %w", configPath, err)
	}
	formerOwners, currentEmails := formerEmailIndex(catalog)
	if len(formerOwners) == 0 {
		return EmailReconcileResult{}, nil
	}

	data, err := readManagedFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return EmailReconcileResult{}, nil
	}
	if err != nil {
		return EmailReconcileResult{}, fmt.Errorf("read %s to catch it up: %w", configPath, err)
	}

	renames, err := formerEmailSectionRenames(parseLines(string(data)), formerOwners, currentEmails)
	if err != nil || len(renames) == 0 {
		return EmailReconcileResult{}, err
	}

	if err := RenameDriveSections(ctx, configPath, renames); err != nil {
		var collision *driveSectionCollisionError
		if errors.As(err, &collision) {
			return EmailReconcileResult{}, fmt.Errorf("catch up %s: config sections %q and %q both exist, so the "+
				"first, under a former email of the account, cannot be renamed to the second: remove the one you "+
				"no longer use and run the command again", configPath, collision.from, collision.to)
		}

		return EmailReconcileResult{}, fmt.Errorf("catch up %s: %w", configPath, err)
	}

	return EmailReconcileResult{DriveRenames: canonicalRenameSlice(renames)}, nil
}

// formerEmailIndex maps each former email of the catalog's accounts to the
// accounts that had it, and lists the emails accounts have now.
func formerEmailIndex(catalog *Catalog) (map[string][]driveid.CanonicalID, map[string]struct{}) {
	formerOwners := make(map[string][]driveid.CanonicalID)
	currentEmails := make(map[string]struct{}, len(catalog.Accounts))
	for _, key := range catalog.SortedAccountKeys() {
		account := catalog.Accounts[key]
		currentEmails[account.Email] = struct{}{}
		cid, err := driveid.NewCanonicalID(account.CanonicalID)
		if err != nil {
			continue
		}
		for _, former := range account.FormerEmails {
			formerOwners[former] = append(formerOwners[former], cid)
		}
	}

	return formerOwners, currentEmails
}

// formerEmailSectionRenames is the rename of every drive section of lines
// under a former email that no account has now, to the email its account has
// now (formerEmailOwner).
func formerEmailSectionRenames(
	lines []parsedLine,
	formerOwners map[string][]driveid.CanonicalID,
	currentEmails map[string]struct{},
) (map[driveid.CanonicalID]driveid.CanonicalID, error) {
	renames := make(map[driveid.CanonicalID]driveid.CanonicalID)
	for i := range lines {
		if lines[i].kind != lineSection {
			continue
		}
		cid, err := driveid.NewCanonicalID(lines[i].sectionName)
		if err != nil {
			continue
		}
		if _, stillCurrent := currentEmails[cid.Email()]; stillCurrent {
			continue
		}
		owner, found := formerEmailOwner(cid, formerOwners[cid.Email()])
		if !found {
			continue
		}
		renamed, err := cid.WithEmail(owner.Email())
		if err != nil {
			return nil, fmt.Errorf("rewrite configured drive ID %s: %w", cid, err)
		}
		renames[cid] = renamed
	}

	return renames, nil
}

// formerEmailOwner is the account a drive section under a former email
// belongs to, among the accounts that had that email: the personal account
// for a personal drive, the work or school account for a work, school or
// SharePoint drive, and the one account that had it for a shared folder.
func formerEmailOwner(section driveid.CanonicalID, owners []driveid.CanonicalID) (driveid.CanonicalID, bool) {
	wantType := ""
	switch section.DriveType() {
	case driveid.DriveTypePersonal:
		wantType = driveid.DriveTypePersonal
	case driveid.DriveTypeBusiness, driveid.DriveTypeSharePoint:
		wantType = driveid.DriveTypeBusiness
	case driveid.DriveTypeShared:
		if len(owners) == 1 {
			return owners[0], true
		}

		return driveid.CanonicalID{}, false
	}
	for _, owner := range owners {
		if owner.DriveType() == wantType {
			return owner, true
		}
	}

	return driveid.CanonicalID{}, false
}
