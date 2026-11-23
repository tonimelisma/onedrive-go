package cli

import "context"

func noStatusLiveOverlay(
	_ context.Context,
	_ *CLIContext,
	_ accountViewSnapshot,
) map[string]statusAccountLiveOverlay {
	return nil
}

// asBuiltStatusAccounts applies to hand-written accounts what the status
// builder derives for the accounts it makes (buildStatusAccountsFromViews):
// the sign-in requirement from the auth state, and each drive's final state
// from its sync state. The printers derive nothing, so a test that feeds
// them hand-written accounts states those derivations here.
func asBuiltStatusAccounts(accounts []statusAccount) []statusAccount {
	for i := range accounts {
		asBuiltStatusAccount(&accounts[i])
	}

	return accounts
}

// asBuiltStatusAccount is asBuiltStatusAccounts for one account.
func asBuiltStatusAccount(account *statusAccount) *statusAccount {
	if account.SignInRequired == nil {
		setAccountSignInRequired(account)
	}
	for j := range account.Drives {
		asBuiltStatusDrive(&account.Drives[j])
	}

	return account
}

// asBuiltStatusDrive finalizes the state of a hand-written drive and of its
// shared folders, as the builder does for the drives it makes, and returns
// the drive.
func asBuiltStatusDrive(drive *statusDrive) *statusDrive {
	for i := range drive.SharedFolders {
		asBuiltStatusDrive(&drive.SharedFolders[i])
	}
	finalizeStatusDriveState(drive)

	return drive
}
