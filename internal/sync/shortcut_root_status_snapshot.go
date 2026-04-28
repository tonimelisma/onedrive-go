package sync

import "github.com/tonimelisma/onedrive-go/internal/config"

// ShortcutRootStatusRow is the status-facing read model for parent-owned
// shortcut-root lifecycle state. It intentionally omits durable implementation
// fields such as filesystem identity and raw waiting records so presentation
// code cannot grow parent lifecycle policy.
type ShortcutRootStatusRow struct {
	NamespaceID        string
	BindingItemID      string
	MountID            string
	RelativeLocalPath  string
	LocalAlias         string
	RemoteDriveID      string
	RemoteItemID       string
	State              ShortcutRootState
	Metadata           ShortcutRootStatusMetadata
	BlockedDetail      string
	ProtectedPaths     []string
	WaitingReplacement string
}

func shortcutRootStatusRows(records []ShortcutRootRecord) []ShortcutRootStatusRow {
	if len(records) == 0 {
		return nil
	}
	rows := make([]ShortcutRootStatusRow, 0, len(records))
	for i := range records {
		record := normalizeShortcutRootRecord(records[i])
		waitingReplacement := ""
		if record.Waiting != nil {
			waitingReplacement = record.Waiting.RelativeLocalPath
		}
		rows = append(rows, ShortcutRootStatusRow{
			NamespaceID:        record.NamespaceID,
			BindingItemID:      record.BindingItemID,
			MountID:            config.ChildMountID(record.NamespaceID, record.BindingItemID),
			RelativeLocalPath:  record.RelativeLocalPath,
			LocalAlias:         record.LocalAlias,
			RemoteDriveID:      record.RemoteDriveID.String(),
			RemoteItemID:       record.RemoteItemID,
			State:              record.State,
			Metadata:           ShortcutRootStatus(record.State),
			BlockedDetail:      record.BlockedDetail,
			ProtectedPaths:     append([]string(nil), record.ProtectedPaths...),
			WaitingReplacement: waitingReplacement,
		})
	}
	return rows
}
