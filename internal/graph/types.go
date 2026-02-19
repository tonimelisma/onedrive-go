package graph

import "time"

// ChildCountUnknown indicates the child count was not present in the API response.
const ChildCountUnknown = -1

// Item represents a OneDrive drive item (file, folder, or package).
// Fields are normalized from the Graph API response — callers never see raw API data.
type Item struct {
	ID            string
	Name          string
	DriveID       string // normalized: lowercase (Graph API casing is inconsistent)
	ParentID      string
	ParentDriveID string // drive containing parent (for cross-drive references)
	Size          int64
	ETag          string
	CTag          string
	IsFolder      bool
	IsDeleted     bool
	IsPackage     bool // OneNote packages — sync should skip these
	MimeType      string
	QuickXorHash  string // base64-encoded
	SHA1Hash      string // hex (Personal accounts only)
	SHA256Hash    string // hex (Business accounts, sometimes)
	CreatedAt     time.Time
	ModifiedAt    time.Time
	ChildCount    int    // ChildCountUnknown if not present
	DownloadURL   string // pre-authenticated, ephemeral; NEVER log (architecture.md §9.2)
}
