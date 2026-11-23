package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// AccountLogin is what a sign-in learned about the account. Login hands it to
// AddDrive, which records it in the catalog in the same transaction that
// enrolls the account's primary drive. The CLI owns the Graph flow; config
// owns how these facts are represented in the catalog.
type AccountLogin struct {
	UserID         string
	DisplayName    string
	OrgName        string
	PrimaryDriveID driveid.ID
}

// IdentityCheckAnsweredInterval is how long after an identity check OneDrive
// answered (or a sign-in, which reads the same user) the next one is due. An
// email change is a local label change only: the sign-in and the drive IDs
// stay valid, so learning of it up to this late breaks nothing.
const IdentityCheckAnsweredInterval = 24 * time.Hour

// IdentityCheckUnansweredInterval is how long after an identity check OneDrive
// did not answer the next one is due.
const IdentityCheckUnansweredInterval = 15 * time.Minute

// IdentityCheckDue reports whether the account's identity check is due at
// now: when the record holds no time, one that does not parse, a time not
// after now, or a time further ahead than IdentityCheckAnsweredInterval, which
// only a clock that went back leaves.
func (a *CatalogAccount) IdentityCheckDue(now time.Time) bool {
	if a.IdentityCheckDueAt == "" {
		return true
	}
	dueAt, err := time.Parse(time.RFC3339, a.IdentityCheckDueAt)
	if err != nil {
		return true
	}

	return !dueAt.After(now) || dueAt.Sub(now) > IdentityCheckAnsweredInterval
}

// recordIdentityCheck sets when the account's next identity check is due:
// IdentityCheckAnsweredInterval after now when OneDrive answered,
// IdentityCheckUnansweredInterval after now when it did not.
func (a *CatalogAccount) recordIdentityCheck(now time.Time, answered bool) {
	interval := IdentityCheckUnansweredInterval
	if answered {
		interval = IdentityCheckAnsweredInterval
	}
	a.IdentityCheckDueAt = now.Add(interval).UTC().Format(time.RFC3339)
}

// RecordAccountIdentityCheck records an identity check of the account in the
// catalog, under the catalog lock, so checks of several accounts that end
// together keep each other's record. An account the catalog no longer holds
// is left alone.
func RecordAccountIdentityCheck(ctx context.Context, accountCID driveid.CanonicalID, now time.Time, answered bool) error {
	err := UpdateCatalog(ctx, func(catalog *Catalog) error {
		account, found := catalog.AccountByCanonicalID(accountCID)
		if !found {
			return nil
		}
		account.recordIdentityCheck(now, answered)
		catalog.UpsertAccount(&account)

		return nil
	})
	if err != nil {
		return fmt.Errorf("record identity check of %s: %w", accountCID, err)
	}

	return nil
}

// recordLogin upserts the account and its primary drive with what a sign-in
// learned. The account's record is written without an auth requirement: the
// sign-in it records works, so a requirement that stood ends in this same
// write.
func (c *Catalog) recordLogin(accountCID driveid.CanonicalID, login *AccountLogin, now time.Time) {
	cachedAt := now.UTC().Format(time.RFC3339)
	account := CatalogAccount{
		CanonicalID:           accountCID.String(),
		Email:                 accountCID.Email(),
		DriveType:             accountCID.DriveType(),
		UserID:                login.UserID,
		DisplayName:           login.DisplayName,
		OrgName:               login.OrgName,
		PrimaryDriveID:        login.PrimaryDriveID.String(),
		PrimaryDriveCanonical: accountCID.String(),
	}
	// The sign-in read the user, which is the identity check's answer.
	account.recordIdentityCheck(now, true)
	if existing, found := c.AccountByCanonicalID(accountCID); found {
		account.FormerEmails = existing.FormerEmails
	}
	// The address this sign-in brings is an account's own now, no former
	// one: a config section under it is that account's, never caught up to
	// another (CatchUpConfigFromFormerEmails).
	for key := range c.Accounts {
		other := c.Accounts[key]
		if slices.Contains(other.FormerEmails, accountCID.Email()) {
			other.FormerEmails = slices.DeleteFunc(slices.Clone(other.FormerEmails), func(email string) bool {
				return email == accountCID.Email()
			})
			c.Accounts[key] = other
		}
	}
	account.FormerEmails = slices.DeleteFunc(slices.Clone(account.FormerEmails), func(email string) bool {
		return email == accountCID.Email()
	})
	if len(account.FormerEmails) == 0 {
		account.FormerEmails = nil
	}

	drive := CatalogDrive{
		CanonicalID:           accountCID.String(),
		OwnerAccountCanonical: accountCID.String(),
		DriveType:             accountCID.DriveType(),
		DisplayName:           DefaultDisplayName(accountCID),
	}
	if existing, found := c.DriveByCanonicalID(accountCID); found {
		drive = existing
		drive.OwnerAccountCanonical = accountCID.String()
		drive.DriveType = accountCID.DriveType()
		if drive.DisplayName == "" {
			drive.DisplayName = DefaultDisplayName(accountCID)
		}
	}
	drive.PrimaryForAccount = true
	drive.RemoteDriveID = login.PrimaryDriveID.String()
	drive.CachedAt = cachedAt

	c.UpsertAccount(&account)
	c.UpsertDrive(&drive)
}

// registerOwnedDrive upserts a drive the signed-in account owns, keeping what
// an earlier record already knew about it.
func (c *Catalog) registerOwnedDrive(cid, ownerCID driveid.CanonicalID) {
	drive := CatalogDrive{
		CanonicalID: cid.String(),
		DisplayName: DefaultDisplayName(cid),
	}
	if existing, found := c.DriveByCanonicalID(cid); found {
		drive = existing
		if drive.DisplayName == "" {
			drive.DisplayName = DefaultDisplayName(cid)
		}
	}
	drive.OwnerAccountCanonical = ownerCID.String()
	drive.DriveType = cid.DriveType()

	c.UpsertDrive(&drive)
}

// registerSharedDrive upserts a shared folder's record under the account it
// is shared with, named as its config section names it.
func (c *Catalog) registerSharedDrive(cid, ownerCID driveid.CanonicalID, displayName string) {
	drive := CatalogDrive{CanonicalID: cid.String()}
	if existing, found := c.DriveByCanonicalID(cid); found {
		drive = existing
	}
	drive.OwnerAccountCanonical = ownerCID.String()
	drive.DriveType = cid.DriveType()
	drive.DisplayName = displayName

	c.UpsertDrive(&drive)
}

// ApplyPurgeLogout removes the catalog account, named by its canonical ID,
// and every drive record it owns. The account is never named by email: a
// personal and a work or school account can share one, and a purge of one
// must leave the other. A zero account, or one the catalog does not hold,
// changes nothing.
func ApplyPurgeLogout(ctx context.Context, dataDir string, accountCID driveid.CanonicalID) error {
	if accountCID.IsZero() {
		return nil
	}

	return UpdateCatalogForDataDir(ctx, dataDir, func(catalog *Catalog) error {
		if _, found := catalog.AccountByCanonicalID(accountCID); !found {
			return nil
		}

		drives := catalog.DrivesForAccount(accountCID)
		for i := range drives {
			delete(catalog.Drives, drives[i].CanonicalID)
		}
		catalog.DeleteAccount(accountCID)
		return nil
	})
}

// PruneDriveAfterPurge removes non-primary retained drive records once local
// purge cleanup has completed, while preserving primary drives as durable
// inventory anchors.
func PruneDriveAfterPurge(ctx context.Context, dataDir string, cid driveid.CanonicalID) error {
	if cid.IsZero() {
		return nil
	}

	return UpdateCatalogForDataDir(ctx, dataDir, func(catalog *Catalog) error {
		drive, found := catalog.DriveByCanonicalID(cid)
		if !found {
			return nil
		}

		accountCID, hasOwner := drive.Owner()
		if drive.PrimaryForAccount || (hasOwner && accountOwnsPrimaryDrive(catalog, accountCID, cid)) {
			return nil
		}

		catalog.DeleteDrive(cid)
		return nil
	})
}

// AccountCanonicalIDByEmail resolves the durable account identity stored in
// the catalog for an email address: zero when the catalog has no such
// account. A decoded catalog holds only canonical IDs (checkCanonicalIDs), so
// the error is for a catalog built in memory with one that does not parse.
func AccountCanonicalIDByEmail(catalog *Catalog, email string) (driveid.CanonicalID, error) {
	if catalog == nil || email == "" {
		return driveid.CanonicalID{}, nil
	}

	account, found := catalog.AccountByEmail(email)
	if !found {
		return driveid.CanonicalID{}, nil
	}

	cid, hasID := account.ID()
	if !hasID {
		return driveid.CanonicalID{}, fmt.Errorf("catalog account of %s holds no canonical ID", email)
	}

	return cid, nil
}

func LoadAccountCanonicalIDByEmail(dataDir, email string) (driveid.CanonicalID, error) {
	catalog, err := LoadCatalogForDataDir(dataDir)
	if err != nil {
		return driveid.CanonicalID{}, err
	}

	return AccountCanonicalIDByEmail(catalog, email)
}

// HasPersistedAccountAuthRequirement reports whether the catalog currently
// marks the account, named by its canonical ID (see MarkAccountAuthRequired),
// as requiring re-authentication.
func HasPersistedAccountAuthRequirement(catalog *Catalog, account driveid.CanonicalID) bool {
	record, found := catalog.AccountByCanonicalID(account)
	if !found {
		return false
	}

	return record.AuthRequirementReason != ""
}

func LoadHasPersistedAccountAuthRequirement(dataDir string, account driveid.CanonicalID) (bool, error) {
	catalog, err := LoadCatalogForDataDir(dataDir)
	if err != nil {
		return false, err
	}

	return HasPersistedAccountAuthRequirement(catalog, account), nil
}

// AuthenticatedAccountEmails returns known catalog accounts that still have a
// usable token file path on disk. It preserves a stable sort for CLI account
// selection prompts.
func AuthenticatedAccountEmails(dataDir string) ([]string, error) {
	catalog, err := LoadCatalogForDataDir(dataDir)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	var emails []string
	for _, key := range catalog.SortedAccountKeys() {
		account := catalog.Accounts[key]
		if account.Email == "" {
			continue
		}
		if _, found := seen[account.Email]; found {
			continue
		}

		accountCID, hasID := account.ID()
		if !hasID {
			return nil, fmt.Errorf("catalog account %q holds no canonical ID", key)
		}

		tokenPath := DriveTokenPath(accountCID)
		if tokenPath == "" || !catalogTokenPathExists(tokenPath) {
			continue
		}

		seen[account.Email] = struct{}{}
		emails = append(emails, account.Email)
	}

	slices.Sort(emails)
	return emails, nil
}

func accountOwnsPrimaryDrive(catalog *Catalog, accountCID, driveCID driveid.CanonicalID) bool {
	if catalog == nil || accountCID.IsZero() || driveCID.IsZero() {
		return false
	}

	account, found := catalog.AccountByCanonicalID(accountCID)
	if !found {
		return false
	}

	return account.PrimaryDriveCanonical == driveCID.String()
}

// catalogTokenPathExists reports whether a token file may be on disk: only a
// file that is certainly missing counts as absent, so an unreadable data
// directory never hides an account. It stats the file; the token is never
// read here.
func catalogTokenPathExists(path string) bool {
	if path == "" {
		return false
	}

	_, err := statManagedPath(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}
