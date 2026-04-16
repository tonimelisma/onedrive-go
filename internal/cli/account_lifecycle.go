package cli

type accountLifecycleState string

const (
	accountLifecycleAbsent                    accountLifecycleState = "absent"
	accountLifecycleLoggedInWithConfigured    accountLifecycleState = "logged_in_with_configured_drives"
	accountLifecycleLoggedInWithoutConfigured accountLifecycleState = "logged_in_without_configured_drives"
	accountLifecycleAuthRequiredMissingLogin  accountLifecycleState = "auth_required_missing_login"
	accountLifecycleAuthRequiredInvalidLogin  accountLifecycleState = "auth_required_invalid_saved_login"
	accountLifecycleAuthRequiredSyncRejected  accountLifecycleState = "auth_required_sync_rejected"
)

type accountLifecycleView struct {
	State               accountLifecycleState
	Known               bool
	HasUsableSavedLogin bool
	HasConfiguredDrives bool
	SelectableForLogout bool
	SelectableForPurge  bool
}

func accountLifecycle(entry *accountCatalogEntry) accountLifecycleView {
	view := accountLifecycleView{
		Known:               entry != nil && entry.Email != "",
		HasUsableSavedLogin: entry != nil && entry.SavedLoginState == savedLoginStateUsable,
		HasConfiguredDrives: entry != nil && entry.Configured,
	}
	view.SelectableForLogout = view.Known && view.HasUsableSavedLogin
	view.SelectableForPurge = view.Known

	switch {
	case !view.Known:
		view.State = accountLifecycleAbsent
	case entry.SavedLoginState == savedLoginStateMissing:
		view.State = accountLifecycleAuthRequiredMissingLogin
	case entry.SavedLoginState == savedLoginStateInvalid:
		view.State = accountLifecycleAuthRequiredInvalidLogin
	case entry.HasPersistedAuthScope:
		view.State = accountLifecycleAuthRequiredSyncRejected
	case view.HasConfiguredDrives:
		view.State = accountLifecycleLoggedInWithConfigured
	default:
		view.State = accountLifecycleLoggedInWithoutConfigured
	}

	return view
}

func knownAccountCatalogEntries(catalog []accountCatalogEntry) []accountCatalogEntry {
	known := make([]accountCatalogEntry, 0, len(catalog))
	for i := range catalog {
		if !accountLifecycle(&catalog[i]).Known {
			continue
		}
		known = append(known, catalog[i])
	}

	return known
}

func logoutSelectableAccountEntries(catalog []accountCatalogEntry) []accountCatalogEntry {
	selectable := make([]accountCatalogEntry, 0, len(catalog))
	for i := range catalog {
		if !accountLifecycle(&catalog[i]).SelectableForLogout {
			continue
		}
		selectable = append(selectable, catalog[i])
	}

	return selectable
}
