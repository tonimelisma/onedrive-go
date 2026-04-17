package config

import (
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

// LoadValidatedState loads config plus catalog and validates cross-file
// invariants before returning a durable snapshot. Lenient config loading is
// intended for informational CLI surfaces that must preserve warnings.
func LoadValidatedState(cfgPath string, lenient bool, logger *slog.Logger) (ValidatedState, []ConfigWarning, error) {
	var (
		cfg      *Config
		warnings []ConfigWarning
		err      error
	)

	if lenient {
		cfg, warnings, err = LoadOrDefaultLenient(cfgPath, logger)
	} else {
		cfg, err = LoadOrDefault(cfgPath, logger)
	}
	if err != nil {
		return ValidatedState{}, warnings, err
	}

	stored, err := LoadCatalog()
	if err != nil {
		return ValidatedState{}, warnings, fmt.Errorf("loading catalog: %w", err)
	}

	if err := validateConfiguredDrivesInCatalog(cfg, stored); err != nil {
		return ValidatedState{}, warnings, err
	}
	if err := validateCatalogDriveOwners(stored); err != nil {
		return ValidatedState{}, warnings, err
	}
	if err := validatePrimaryDriveOwnership(stored); err != nil {
		return ValidatedState{}, warnings, err
	}

	return ValidatedState{
		Config:  cfg,
		Catalog: stored,
	}, warnings, nil
}

func validateConfiguredDrivesInCatalog(cfg *Config, stored *Catalog) error {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	if stored == nil {
		stored = DefaultCatalog()
	}

	for cid := range cfg.Drives {
		if _, found := stored.DriveByCanonicalID(cid); !found {
			return fmt.Errorf("catalog invariant: configured drive %s has no catalog entry", cid)
		}
	}

	return nil
}

func validateCatalogDriveOwners(stored *Catalog) error {
	if stored == nil {
		stored = DefaultCatalog()
	}

	for _, key := range stored.SortedDriveKeys() {
		drive := stored.Drives[key]
		if drive.OwnerAccountCanonical == "" {
			return fmt.Errorf("catalog invariant: drive %s has no owning account", drive.CanonicalID)
		}
		ownerCID, err := driveid.NewCanonicalID(drive.OwnerAccountCanonical)
		if err != nil {
			return fmt.Errorf("catalog invariant: drive %s has malformed owning account %q: %w", drive.CanonicalID, drive.OwnerAccountCanonical, err)
		}
		if _, found := stored.AccountByCanonicalID(ownerCID); !found {
			return fmt.Errorf("catalog invariant: drive %s owner %s is missing from the catalog", drive.CanonicalID, ownerCID)
		}
	}

	return nil
}

func validatePrimaryDriveOwnership(stored *Catalog) error {
	if stored == nil {
		stored = DefaultCatalog()
	}

	for _, key := range stored.SortedAccountKeys() {
		account := stored.Accounts[key]
		if account.PrimaryDriveCanonical == "" {
			continue
		}
		primaryCID, err := driveid.NewCanonicalID(account.PrimaryDriveCanonical)
		if err != nil {
			return fmt.Errorf(
				"catalog invariant: account %s has malformed primary drive %q: %w",
				account.CanonicalID,
				account.PrimaryDriveCanonical,
				err,
			)
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

func MarkAccountAuthRequired(dataDir, email string, reason authstate.Reason) error {
	if email == "" {
		return nil
	}

	return UpdateCatalogForDataDir(dataDir, func(catalog *Catalog) error {
		account, found := catalog.AccountByEmail(email)
		if !found {
			return nil
		}
		account.AuthRequirementReason = reason
		catalog.UpsertAccount(&account)
		return nil
	})
}

func ClearAccountAuthRequirement(dataDir, email string, _ AuthClearSource) error {
	if email == "" {
		return nil
	}

	return UpdateCatalogForDataDir(dataDir, func(catalog *Catalog) error {
		account, found := catalog.AccountByEmail(email)
		if !found {
			return nil
		}
		account.AuthRequirementReason = ""
		catalog.UpsertAccount(&account)
		return nil
	})
}
