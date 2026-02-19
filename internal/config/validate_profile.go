package config

import (
	"fmt"
)

// validAccountTypes enumerates accepted account_type values.
var validAccountTypes = map[string]bool{
	AccountTypePersonal:   true,
	AccountTypeBusiness:   true,
	AccountTypeSharePoint: true,
}

// validAzureEndpoints enumerates accepted azure_ad_endpoint values.
var validAzureEndpoints = map[string]bool{
	"":                true,
	AzureEndpointUSL4: true,
	AzureEndpointUSL5: true,
	AzureEndpointDE:   true,
	AzureEndpointCN:   true,
}

// validateProfiles checks all profile-level constraints.
func validateProfiles(profiles map[string]Profile) []error {
	if len(profiles) == 0 {
		return nil
	}

	var errs []error

	syncDirs := make(map[string]string, len(profiles))

	for name := range profiles {
		p := profiles[name]
		errs = append(errs, validateSingleProfile(name, &p)...)
		errs = append(errs, checkDuplicateSyncDir(name, &p, syncDirs)...)
	}

	return errs
}

// validateSingleProfile validates one profile's fields.
func validateSingleProfile(name string, p *Profile) []error {
	var errs []error

	errs = append(errs, validateAccountType(name, p.AccountType)...)
	errs = append(errs, validateSyncDir(name, p.SyncDir)...)
	errs = append(errs, validateDriveID(name, p)...)
	errs = append(errs, validateAzureEndpoint(name, p.AzureADEndpoint)...)
	errs = append(errs, validateProfileOverrides(p)...)

	return errs
}

// validateAccountType checks that account_type is one of the valid values.
func validateAccountType(profileName, accountType string) []error {
	if !validAccountTypes[accountType] {
		return []error{fmt.Errorf(
			"profile.%s.account_type: must be one of personal, business, sharepoint; got %q",
			profileName, accountType)}
	}

	return nil
}

// validateSyncDir checks that sync_dir is set.
func validateSyncDir(profileName, syncDir string) []error {
	if syncDir == "" {
		return []error{fmt.Errorf("profile.%s.sync_dir: must not be empty", profileName)}
	}

	return nil
}

// validateDriveID checks that drive_id is set for sharepoint accounts.
func validateDriveID(profileName string, p *Profile) []error {
	if p.AccountType == AccountTypeSharePoint && p.DriveID == "" {
		return []error{fmt.Errorf(
			"profile.%s.drive_id: required for sharepoint account type",
			profileName)}
	}

	return nil
}

// validateAzureEndpoint checks that azure_ad_endpoint is valid.
func validateAzureEndpoint(profileName, endpoint string) []error {
	if !validAzureEndpoints[endpoint] {
		return []error{fmt.Errorf(
			"profile.%s.azure_ad_endpoint: must be one of USL4, USL5, DE, CN, or empty; got %q",
			profileName, endpoint)}
	}

	return nil
}

// checkDuplicateSyncDir ensures no two profiles share the same expanded sync_dir.
func checkDuplicateSyncDir(name string, p *Profile, seen map[string]string) []error {
	if p.SyncDir == "" {
		return nil
	}

	expanded := expandTilde(p.SyncDir)

	if other, exists := seen[expanded]; exists {
		return []error{fmt.Errorf(
			"profile.%s.sync_dir: %q conflicts with profile.%s (same directory)",
			name, p.SyncDir, other)}
	}

	seen[expanded] = name

	return nil
}

// validateProfileOverrides validates per-profile section overrides.
func validateProfileOverrides(p *Profile) []error {
	var errs []error

	if p.Filter != nil {
		errs = append(errs, validateFilter(p.Filter)...)
	}

	if p.Transfers != nil {
		errs = append(errs, validateTransfers(p.Transfers)...)
	}

	if p.Safety != nil {
		errs = append(errs, validateSafety(p.Safety)...)
	}

	if p.Sync != nil {
		errs = append(errs, validateSync(p.Sync)...)
	}

	if p.Logging != nil {
		errs = append(errs, validateLogging(p.Logging)...)
	}

	if p.Network != nil {
		errs = append(errs, validateNetwork(p.Network)...)
	}

	return errs
}
