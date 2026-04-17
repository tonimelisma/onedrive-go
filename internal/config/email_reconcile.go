package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/fsroot"
)

// CanonicalIDRename records one exact canonical-ID rewrite.
type CanonicalIDRename struct {
	From driveid.CanonicalID
	To   driveid.CanonicalID
}

// EmailReconcileResult reports the deterministic renames applied for one
// account email change. CLI callers use this to remap exact selectors for the
// current invocation after config/data have been rewritten.
type EmailReconcileResult struct {
	UserID         string
	CurrentAccount driveid.CanonicalID
	AccountRenames []CanonicalIDRename
	DriveRenames   []CanonicalIDRename
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
// current Graph email differs, it rewrites owned config sections, token/state
// paths, and catalog ownership from the old email to the current one.
func ReconcileAccountEmail(
	configPath string,
	currentAccount driveid.CanonicalID,
	userID string,
	currentEmail string,
	logger *slog.Logger,
) (EmailReconcileResult, error) {
	result := EmailReconcileResult{
		UserID:         userID,
		CurrentAccount: currentAccount,
	}

	if currentAccount.IsZero() || userID == "" || currentEmail == "" {
		return result, nil
	}

	if !currentAccount.IsPersonal() && !currentAccount.IsBusiness() {
		return result, fmt.Errorf("email reconciliation requires a personal or business account CID")
	}

	accountRenames, err := matchingAccountRenames(currentAccount, userID, currentEmail, logger)
	if err != nil {
		return result, err
	}
	if len(accountRenames) == 0 {
		return result, nil
	}

	plan, err := buildEmailReconcilePlan(configPath, accountRenames, currentEmail, logger)
	if err != nil {
		return result, err
	}
	if !plan.hasChanges() {
		return result, nil
	}

	if err := plan.validate(); err != nil {
		return result, err
	}

	if err := plan.apply(configPath); err != nil {
		return result, err
	}
	if err := reconcileCatalogEmail(accountRenames, currentEmail); err != nil {
		return result, err
	}

	result.AccountRenames = plan.accountRenames()
	result.DriveRenames = plan.driveRenames()

	return result, nil
}

type emailReconcilePlan struct {
	accountRenameMap map[driveid.CanonicalID]driveid.CanonicalID
	driveRenameMap   map[driveid.CanonicalID]driveid.CanonicalID
	pathRenames      []managedPathRename
}

type managedPathRename struct {
	source string
	target string
}

func (p *emailReconcilePlan) hasChanges() bool {
	return len(p.accountRenameMap) > 0 || len(p.driveRenameMap) > 0 || len(p.pathRenames) > 0
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
		accountRenameMap: mapsClone(accountRenames),
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
		addPathRename(plan, DriveStatePath(from), DriveStatePath(to))
	}

	for _, oldEmail := range oldEmails {
		collectStateRenames(plan, oldEmail, currentEmail, ownedOldAccounts, logger)
	}

	return plan, nil
}

func mapsClone(src map[driveid.CanonicalID]driveid.CanonicalID) map[driveid.CanonicalID]driveid.CanonicalID {
	if len(src) == 0 {
		return nil
	}

	out := make(map[driveid.CanonicalID]driveid.CanonicalID, len(src))
	for k, v := range src {
		out[k] = v
	}

	return out
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

		if _, exists := existingSections[updated.String()]; exists {
			if _, fromExists := existingSections[cid.String()]; fromExists {
				return fmt.Errorf("config section rename collision: %q already exists", updated.String())
			}
		}

		plan.driveRenameMap[cid] = updated
	}

	return nil
}

func collectStateRenames(
	plan *emailReconcilePlan,
	oldEmail string,
	currentEmail string,
	ownedOldAccounts map[string]driveid.CanonicalID,
	logger *slog.Logger,
) {
	paths := DiscoverStateDBsForEmail(oldEmail, logger)
	for _, path := range paths {
		if pathRenameExists(plan.pathRenames, path) {
			continue
		}

		name := filepath.Base(path)
		switch managedFileDriveType(name, "state_", ".db") {
		case driveid.DriveTypePersonal:
			if ownedOldAccountOfType(ownedOldAccounts, driveid.DriveTypePersonal, oldEmail) {
				addPathRename(plan, path, rewriteManagedEmailPath(path, oldEmail, currentEmail))
			}
		case driveid.DriveTypeBusiness, driveid.DriveTypeSharePoint:
			if ownedOldAccountOfType(ownedOldAccounts, driveid.DriveTypeBusiness, oldEmail) {
				addPathRename(plan, path, rewriteManagedEmailPath(path, oldEmail, currentEmail))
			}
		case driveid.DriveTypeShared:
			targeted, err := sharedStateOwnedByRenamedAccount(path, ownedOldAccounts)
			if err != nil {
				logger.Warn("skip shared state email reconciliation",
					"path", path,
					"error", err,
				)
				continue
			}
			if targeted {
				addPathRename(plan, path, rewriteManagedEmailPath(path, oldEmail, currentEmail))
			}
		}
	}
}

func reconcileCatalogEmail(accountRenames map[driveid.CanonicalID]driveid.CanonicalID, currentEmail string) error {
	if len(accountRenames) == 0 {
		return nil
	}

	return UpdateCatalog(func(catalog *Catalog) error {
		for from, to := range accountRenames {
			account, found := catalog.AccountByCanonicalID(from)
			if found {
				catalog.DeleteAccount(from)
				account.CanonicalID = to.String()
				account.Email = currentEmail
				account.DriveType = to.DriveType()
				if account.PrimaryDriveCanonical == from.String() {
					account.PrimaryDriveCanonical = to.String()
				}
				catalog.UpsertAccount(&account)
			}

			for _, key := range catalog.SortedDriveKeys() {
				drive := catalog.Drives[key]
				driveCID, err := driveid.NewCanonicalID(drive.CanonicalID)
				if err != nil {
					continue
				}
				if drive.OwnerAccountCanonical == from.String() {
					drive.OwnerAccountCanonical = to.String()
				}
				updatedCID, err := driveCID.WithEmail(currentEmail)
				if err != nil || driveCID.Equal(updatedCID) {
					catalog.UpsertDrive(&drive)
					continue
				}
				catalog.DeleteDrive(driveCID)
				drive.CanonicalID = updatedCID.String()
				drive.DriveType = updatedCID.DriveType()
				catalog.UpsertDrive(&drive)
			}
		}

		return nil
	})
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

func addPathRename(plan *emailReconcilePlan, source string, target string) {
	if source == "" || target == "" || source == target {
		return
	}

	for i := range plan.pathRenames {
		if plan.pathRenames[i].source == source {
			return
		}
	}

	plan.pathRenames = append(plan.pathRenames, managedPathRename{
		source: source,
		target: target,
	})
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

func sharedStateOwnedByRenamedAccount(path string, ownedOldAccounts map[string]driveid.CanonicalID) (bool, error) {
	catalog, err := LoadCatalog()
	if err != nil {
		return false, fmt.Errorf("loading catalog for shared state reconciliation: %w", err)
	}

	for _, key := range catalog.SortedDriveKeys() {
		drive := catalog.Drives[key]
		driveCID, cidErr := driveid.NewCanonicalID(drive.CanonicalID)
		if cidErr != nil {
			continue
		}
		if DriveStatePath(driveCID) != path {
			continue
		}

		_, ok := ownedOldAccounts[drive.OwnerAccountCanonical]
		return ok, nil
	}

	return false, fmt.Errorf("matching catalog drive missing")
}

func (p *emailReconcilePlan) validate() error {
	for _, rename := range p.pathRenames {
		if err := validateManagedRename(rename.source, rename.target); err != nil {
			return err
		}
	}

	return nil
}

func validateManagedRename(source string, target string) error {
	if source == "" || target == "" || source == target {
		return nil
	}

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
		return fmt.Errorf("email reconciliation target already exists: %s", target)
	}
}

func (p *emailReconcilePlan) apply(configPath string) error {
	for _, rename := range p.pathRenames {
		if err := renameManagedPathIfPresent(rename.source, rename.target); err != nil {
			return err
		}
	}

	if err := RenameDriveSections(configPath, p.driveRenameMap); err != nil {
		return fmt.Errorf("rename drive sections: %w", err)
	}

	return nil
}

func renameManagedPathIfPresent(source string, target string) error {
	if source == "" || target == "" || source == target {
		return nil
	}

	root, sourceName, err := fsroot.OpenPath(source)
	if err != nil {
		return fmt.Errorf("open source root for %s: %w", source, err)
	}

	targetName := filepath.Base(target)
	if _, err := root.Stat(sourceName); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat source %s: %w", source, err)
	}

	if err := root.Rename(sourceName, targetName); err != nil {
		return fmt.Errorf("rename %s to %s: %w", source, target, err)
	}

	return nil
}
