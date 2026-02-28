package graph

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ChildCountUnknown indicates the child count was not present in the API response.
const ChildCountUnknown = -1

// Item represents a OneDrive drive item (file, folder, or package).
// Fields are normalized from the Graph API response — callers never see raw API data.
type Item struct {
	ID            string
	Name          string
	DriveID       driveid.ID // normalized: lowercase, zero-padded (Graph API casing is inconsistent)
	ParentID      string
	ParentDriveID driveid.ID // drive containing parent (for cross-drive references)
	Size          int64
	ETag          string
	CTag          string
	IsFolder      bool
	IsRoot        bool // drive root folder (root facet present in API response)
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

// DeltaPage holds one page of delta query results.
type DeltaPage struct {
	Items     []Item
	NextLink  string // non-empty if more pages available
	DeltaLink string // non-empty on final page (use as token for next sync)
}

// User represents a Microsoft Graph user profile.
type User struct {
	ID          string
	DisplayName string
	Email       string
}

// Drive represents a OneDrive drive.
type Drive struct {
	ID         driveid.ID
	Name       string
	DriveType  string // "personal", "business", "documentLibrary"
	OwnerName  string
	QuotaUsed  int64
	QuotaTotal int64
}

// Site represents a SharePoint site.
type Site struct {
	ID          string
	DisplayName string
	Name        string // URL slug (used in canonical ID construction)
	WebURL      string
}

// Organization represents a user's organizational membership.
// Personal accounts have no organization; DisplayName is empty.
type Organization struct {
	DisplayName string
}

// UploadSession represents an in-progress resumable upload.
type UploadSession struct {
	UploadURL      string // pre-authenticated, ephemeral; NEVER log (architecture.md §9.2)
	ExpirationTime time.Time
}

// UploadSessionStatus represents the current state of a resumable upload session.
// Returned by QueryUploadSession to determine which byte ranges have been accepted.
type UploadSessionStatus struct {
	UploadURL          string // pre-authenticated, ephemeral; NEVER log (architecture.md §9.2)
	ExpirationTime     time.Time
	NextExpectedRanges []string // e.g., ["0-", "327680-"]
}
