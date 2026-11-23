package config

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ValidatedState is the config-owned durable snapshot used by CLI inventory
// surfaces. The loader owns config/catalog invariant validation so callers do
// not rebuild those rules independently.
type ValidatedState struct {
	Config  *Config
	Catalog *Catalog
}

// AuthClearSource identifies the proof or lifecycle boundary that cleared a
// persisted account auth requirement.
type AuthClearSource string

const (
	AuthClearSourceLogin            AuthClearSource = "login"
	AuthClearSourceLogout           AuthClearSource = "logout"
	AuthClearSourceCLIProof         AuthClearSource = "cli_proof"
	AuthClearSourceSyncStartupProof AuthClearSource = "sync_startup_proof"
)

// LoadValidatedState loads the config leniently (LoadOrDefaultLenient) plus
// the catalog and validates cross-file invariants before returning a durable
// snapshot, for the informational CLI surfaces that must show what they can
// and preserve the config's warnings.
//
// Its warnings also cover each drive that sets no sync_dir and whose default
// folder meets a folder the client keeps for itself, in the words sync
// refuses that drive with. The check runs here, not in LoadOrDefaultLenient:
// the default comes from the catalog's account names, and this load computes
// it from the catalog it has just validated, while LoadOrDefaultLenient reads
// the config file alone and also serves commands that discard its warnings.
func LoadValidatedState(cfgPath string, logger *slog.Logger) (ValidatedState, []ConfigWarning, error) {
	cfg, warnings, err := LoadOrDefaultLenient(cfgPath, logger)
	if err != nil {
		return ValidatedState{}, warnings, err
	}

	stored, err := LoadCatalog()
	if err != nil {
		return ValidatedState{}, warnings, fmt.Errorf("loading catalog: %w", err)
	}

	if err := validateConfiguredDrivesInCatalog(cfgPath, cfg, stored); err != nil {
		return ValidatedState{}, warnings, markInvalidConfig(err)
	}
	if err := validateCatalogDriveOwners(stored); err != nil {
		return ValidatedState{}, warnings, err
	}
	if err := validatePrimaryDriveOwnership(stored); err != nil {
		return ValidatedState{}, warnings, err
	}

	warnings = appendWarnings(warnings, checkDefaultSyncDirsMeetNoManagedFolder(cfg, stored))

	return ValidatedState{
		Config:  cfg,
		Catalog: stored,
	}, warnings, nil
}

// validateConfiguredDrivesInCatalog refuses a drive section of the config at
// cfgPath that the catalog has no record of. The refusal names the config
// file the load was given, which --config can make any file.
func validateConfiguredDrivesInCatalog(cfgPath string, cfg *Config, stored *Catalog) error {
	for _, cid := range sortedDriveIDs(cfg) {
		if _, found := stored.DriveByCanonicalID(cid); !found {
			return fmt.Errorf("drive %s is in %s but not in onedrive-go's drive inventory (catalog.json); "+
				"run 'onedrive-go drive add %s' to complete it, or remove its section from %s",
				cid, cfgPath, cid, cfgPath)
		}
	}

	return nil
}

func validateCatalogDriveOwners(stored *Catalog) error {
	for _, key := range stored.SortedDriveKeys() {
		drive := stored.Drives[key]
		ownerCID, hasOwner := drive.Owner()
		if !hasOwner {
			return fmt.Errorf("catalog invariant: drive %s has no owning account", drive.CanonicalID)
		}
		if _, found := stored.AccountByCanonicalID(ownerCID); !found {
			return fmt.Errorf("catalog invariant: drive %s owner %s is missing from the catalog", drive.CanonicalID, ownerCID)
		}
	}

	return nil
}

func validatePrimaryDriveOwnership(stored *Catalog) error {
	for _, key := range stored.SortedAccountKeys() {
		account := stored.Accounts[key]
		primaryCID, hasPrimary := account.PrimaryDrive()
		if !hasPrimary {
			continue
		}
		drive, found := stored.DriveByCanonicalID(primaryCID)
		if !found {
			return fmt.Errorf("catalog invariant: account %s primary drive %s is missing from the catalog", account.CanonicalID, primaryCID)
		}
		if drive.OwnerAccountCanonical != account.CanonicalID {
			return fmt.Errorf(
				"catalog invariant: account %s primary drive %s is owned by %s",
				account.CanonicalID,
				primaryCID,
				drive.OwnerAccountCanonical,
			)
		}
	}

	return nil
}

// MarkAccountAuthRequired records that the account must sign in again, for
// the reason given. An account the catalog does not know is left alone.
//
// The account is named by its canonical ID, never its email: a personal and a
// work or school account can share an address, and the requirement belongs to
// the one whose sign-in was refused. The same holds for every reader and
// clear of it.
func MarkAccountAuthRequired(ctx context.Context, dataDir string, account driveid.CanonicalID, reason authstate.Reason) error {
	if account.IsZero() {
		return nil
	}

	return UpdateCatalogForDataDir(ctx, dataDir, func(catalog *Catalog) error {
		record, found := catalog.AccountByCanonicalID(account)
		if !found {
			return nil
		}
		record.AuthRequirementReason = reason
		catalog.UpsertAccount(&record)
		return nil
	})
}

// ClearAccountAuthRequirement clears the account's persisted auth
// requirement. It is the one writer of that field's cleared state outside a
// login, whose enrollment ends the requirement in the catalog write that
// records the login (recordLogin). Callers name the reason they are clearing
// it in their own logging, because the catalog records only whether a
// requirement stands, not how it ended.
func ClearAccountAuthRequirement(ctx context.Context, dataDir string, account driveid.CanonicalID) error {
	if account.IsZero() {
		return nil
	}

	return UpdateCatalogForDataDir(ctx, dataDir, func(catalog *Catalog) error {
		record, found := catalog.AccountByCanonicalID(account)
		if !found {
			return nil
		}
		record.AuthRequirementReason = ""
		catalog.UpsertAccount(&record)
		return nil
	})
}
