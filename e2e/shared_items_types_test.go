package e2e

type sharedItemE2E struct {
	Selector      string `json:"selector"`
	Type          string `json:"type"`
	Name          string `json:"name"`
	AccountEmail  string `json:"account_email"`
	SharedByEmail string `json:"shared_by_email"`
	RemoteDriveID string `json:"remote_drive_id"`
	RemoteItemID  string `json:"remote_item_id"`
}

// Item types in shared --json output.
const (
	sharedItemTypeFile   = "file"
	sharedItemTypeFolder = "folder"
)

type sharedListE2EOutput struct {
	Items            []sharedItemE2E `json:"items"`
	AccountsDegraded []struct {
		Email     string `json:"email"`
		Reason    string `json:"reason"`
		DriveType string `json:"drive_type"`
	} `json:"accounts_degraded"`
}

type sharedStatE2EOutput struct {
	Name           string `json:"name"`
	AccountEmail   string `json:"account_email"`
	RemoteDriveID  string `json:"remote_drive_id"`
	RemoteItemID   string `json:"remote_item_id"`
	SharedSelector string `json:"shared_selector"`
}

type driveListE2EOutput struct {
	Configured []struct {
		CanonicalID string `json:"canonical_id"`
		SyncDir     string `json:"sync_dir"`
	} `json:"configured"`
	Available []struct {
		CanonicalID string `json:"canonical_id"`
	} `json:"available"`
}

type resolvedSharedFileFixture struct {
	RecipientDriveID string
	RecipientEmail   string
	RawStat          sharedStatE2EOutput
	FileItem         sharedItemE2E
}

type resolvedSharedFolderFixture struct {
	RecipientDriveID string
	RecipientEmail   string
	FolderItem       sharedItemE2E
}

type shortcutFixtureKind string

const (
	shortcutFixtureWritable shortcutFixtureKind = "writable"
	shortcutFixtureReadOnly shortcutFixtureKind = "read-only"
)

type resolvedShortcutFixture struct {
	Kind         shortcutFixtureKind
	ParentDrive  string
	ParentEmail  string
	ShortcutName string
	SharerEmail  string
	SentinelPath string
	SharedItem   sharedItemE2E
}
