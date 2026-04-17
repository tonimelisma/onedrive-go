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

func accountLifecycle(entry *accountView) accountLifecycleView {
	view := accountLifecycleView{
		Known:               entry != nil && entry.Email != "",
		HasUsableSavedLogin: entry != nil && entry.SavedLoginReason == "",
		HasConfiguredDrives: entry != nil && entry.Configured,
	}
	view.SelectableForLogout = view.Known && view.HasUsableSavedLogin
	view.SelectableForPurge = view.Known

	switch {
	case !view.Known:
		view.State = accountLifecycleAbsent
	case entry.SavedLoginReason == authReasonMissingLogin:
		view.State = accountLifecycleAuthRequiredMissingLogin
	case entry.SavedLoginReason == authReasonInvalidSavedLogin:
		view.State = accountLifecycleAuthRequiredInvalidLogin
	case entry.AuthRequirementReason == authReasonSyncAuthRejected:
		view.State = accountLifecycleAuthRequiredSyncRejected
	case view.HasConfiguredDrives:
		view.State = accountLifecycleLoggedInWithConfigured
	default:
		view.State = accountLifecycleLoggedInWithoutConfigured
	}

	return view
}

func knownAccountViews(views []accountView) []accountView {
	known := make([]accountView, 0, len(views))
	for i := range views {
		if !accountLifecycle(&views[i]).Known {
			continue
		}
		known = append(known, views[i])
	}

	return known
}

func logoutSelectableAccountViews(views []accountView) []accountView {
	selectable := make([]accountView, 0, len(views))
	for i := range views {
		if !accountLifecycle(&views[i]).SelectableForLogout {
			continue
		}
		selectable = append(selectable, views[i])
	}

	return selectable
}
