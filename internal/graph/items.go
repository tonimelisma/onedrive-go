package graph

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// listChildrenPageSize is the $top value for ListChildren requests.
// 200 is the maximum allowed by the Graph API for drive item collections.
const listChildrenPageSize = 200

// Timestamp validation bounds — timestamps outside this range are treated as
// unknown and logged. Unknown is represented as the zero time so downstream
// sync code can persist NULL/0 instead of fabricating "now".
const (
	minValidYear = 1970
	maxValidYear = 2100
)

// ErrInvalidPath is returned when a remote path is empty or has a leading slash.
// These indicate a caller bug — path-based API methods require a relative path
// without leading slashes. For root, callers should use the ID-based methods instead.
var ErrInvalidPath = errors.New("graph: invalid remote path (empty or has leading slash)")

// validateRemotePath rejects empty paths and paths with a leading slash.
// Both produce malformed Graph API URLs (e.g. "/drives/d/root:/:" or "/drives/d/root://foo:").
func validateRemotePath(remotePath string) error {
	if remotePath == "" || strings.HasPrefix(remotePath, "/") {
		return ErrInvalidPath
	}

	return nil
}

// encodePathSegments URL-encodes each segment of a slash-separated path.
// Characters like #, ?, %, and spaces are encoded per-segment so the
// resulting path is safe for interpolation into Graph API URLs.
func encodePathSegments(path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}

	return strings.Join(segments, "/")
}

// driveItemResponse mirrors the Graph API driveItem JSON exactly.
// Unexported — callers use Item via toItem() normalization.
type driveItemResponse struct {
	ID                   string              `json:"id"`
	Name                 string              `json:"name"`
	Size                 int64               `json:"size"`
	ETag                 string              `json:"eTag"`
	CTag                 string              `json:"cTag"`
	CreatedDateTime      string              `json:"createdDateTime"`
	LastModifiedDateTime string              `json:"lastModifiedDateTime"`
	ParentReference      *parentRef          `json:"parentReference"`
	File                 *fileFacet          `json:"file"`
	Folder               *folderFacet        `json:"folder"`
	Root                 *json.RawMessage    `json:"root"`
	Deleted              *json.RawMessage    `json:"deleted"`
	Package              *json.RawMessage    `json:"package"`
	DownloadURL          string              `json:"@microsoft.graph.downloadUrl"`
	SpecialFolder        *specialFolderFacet `json:"specialFolder"`
	RemoteItem           *remoteItemFacet    `json:"remoteItem"`
	Shared               *sharedFacet        `json:"shared"`
}

type specialFolderFacet struct {
	Name string `json:"name"`
}

type parentRef struct {
	ID      string `json:"id"`
	DriveID string `json:"driveId"`
	Path    string `json:"path"`
}

type fileFacet struct {
	MimeType string     `json:"mimeType"`
	Hashes   *hashFacet `json:"hashes"`
}

type hashFacet struct {
	QuickXorHash string `json:"quickXorHash"`
	SHA1Hash     string `json:"sha1Hash"`
	SHA256Hash   string `json:"sha256Hash"`
}

type folderFacet struct {
	ChildCount int `json:"childCount"`
}

type remoteItemFacet struct {
	ID              string            `json:"id"`
	ParentReference *parentRef        `json:"parentReference"`
	Folder          *folderFacet      `json:"folder"`
	CreatedBy       *identitySetFacet `json:"createdBy"`
	Shared          *sharedFacet      `json:"shared"`
}

// identitySetFacet represents the createdBy/lastModifiedBy identity set
// in Graph API responses. Reuses sharedUserFacet for the user — the API
// returns displayName and email (undocumented but works on both personal
// and business accounts, confirmed via live testing 2026-03-06).
type identitySetFacet struct {
	User *sharedUserFacet `json:"user"`
}

type sharedFacet struct {
	Owner    *sharedOwnerFacet `json:"owner"`
	SharedBy *sharedOwnerFacet `json:"sharedBy"`
}

type sharedOwnerFacet struct {
	User *sharedUserFacet `json:"user"`
}

type sharedUserFacet struct {
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
}

type listChildrenResponse struct {
	Value    []driveItemResponse `json:"value"`
	NextLink string              `json:"@odata.nextLink"`
}

type createFolderRequest struct {
	Name             string      `json:"name"`
	Folder           folderFacet `json:"folder"`
	ConflictBehavior string      `json:"@microsoft.graph.conflictBehavior"`
}

type moveItemRequest struct {
	ParentReference *moveParentRef `json:"parentReference,omitempty"`
	Name            string         `json:"name,omitempty"`
}

type moveParentRef struct {
	ID string `json:"id"`
}

// toItem normalizes a Graph API driveItem response into our Item type.
func (d *driveItemResponse) toItem(logger *slog.Logger) Item {
	item := Item{
		ID:          d.ID,
		Name:        d.Name,
		Size:        d.Size,
		ETag:        d.ETag,
		CTag:        d.CTag,
		IsFolder:    d.Folder != nil,
		IsRoot:      d.Root != nil,
		IsDeleted:   d.Deleted != nil,
		IsPackage:   d.Package != nil,
		ChildCount:  ChildCountUnknown,
		DownloadURL: DownloadURL(d.DownloadURL),
	}

	if d.SpecialFolder != nil {
		item.SpecialFolderName = d.SpecialFolder.Name
	}

	// Normalize DriveID via driveid.New — lowercase + zero-pad for short IDs.
	// Graph API returns inconsistent casing across endpoints (see docs/tier1-research/).
	//
	// Both DriveID and ParentDriveID come from parentReference.driveId — the
	// Graph API only provides one drive identifier per item. For same-drive
	// items these are always equal. Cross-drive resolution (e.g. shared items)
	// happens at a higher layer (sync's resolveParentDriveID).
	if d.ParentReference != nil {
		item.DriveID = driveid.New(d.ParentReference.DriveID)
		item.ParentID = d.ParentReference.ID
		item.ParentPath, item.ParentPathKnown = decodeParentReferencePath(d.ParentReference.Path, logger)
		item.ParentDriveID = driveid.New(d.ParentReference.DriveID)
	}

	// Folder child count
	if d.Folder != nil {
		item.ChildCount = d.Folder.ChildCount
	}

	// File hashes — nil-safe at each level
	if d.File != nil {
		item.MimeType = d.File.MimeType

		if d.File.Hashes != nil {
			item.QuickXorHash = d.File.Hashes.QuickXorHash
			item.SHA1Hash = d.File.Hashes.SHA1Hash
			item.SHA256Hash = d.File.Hashes.SHA256Hash
		}
	}

	// Remote item metadata for embedded shared-folder items surfaced by Graph.
	if d.RemoteItem != nil {
		item.RemoteItemID = d.RemoteItem.ID
		item.RemoteIsFolder = d.RemoteItem.Folder != nil
		if d.RemoteItem.ParentReference != nil {
			item.RemoteDriveID = driveid.New(d.RemoteItem.ParentReference.DriveID).String()
		}
	}

	// Shared owner identity — see resolveSharedOwner for fallback chain.
	item.SharedOwnerName, item.SharedOwnerEmail = d.resolveSharedOwner()

	// Timestamps — validate and fallback to now if invalid.
	// Deleted items routinely have empty timestamps (known OneDrive API behavior),
	// so we log at DEBUG instead of WARN to avoid noise.
	item.CreatedAt = parseTimestamp(d.CreatedDateTime, "createdDateTime", d.ID, item.IsDeleted, logger)
	item.ModifiedAt = parseTimestamp(d.LastModifiedDateTime, "lastModifiedDateTime", d.ID, item.IsDeleted, logger)

	return item
}

// decodeParentReferencePath converts Graph's parentReference.path into a
// decoded path relative to the drive root. Graph returns an absolute API path
// prefix ("/drives/{id}/root:") plus URL-encoded segments in non-delta
// responses. Callers only need the root-relative filesystem portion.
func decodeParentReferencePath(rawPath string, logger *slog.Logger) (string, bool) {
	if rawPath == "" {
		return "", false
	}

	decoded, err := url.PathUnescape(rawPath)
	if err != nil {
		if logger != nil {
			logger.Debug("failed to decode parentReference.path, ignoring",
				slog.String("path", rawPath),
				slog.String("error", err.Error()),
			)
		}

		return "", false
	}

	_, rootRelativePath, found := strings.Cut(decoded, "/root:")
	if !found {
		if logger != nil {
			logger.Debug("parentReference.path missing root marker, ignoring",
				slog.String("path", decoded),
			)
		}

		return "", false
	}

	return strings.TrimPrefix(rootRelativePath, "/"), true
}

// resolveSharedOwner extracts sharer identity using a four-level fallback chain:
//  1. remoteItem.shared.sharedBy (correct semantics: who shared it)
//  2. remoteItem.shared.owner
//  3. remoteItem.createdBy
//  4. top-level shared.owner
//
// Graph shared-item responses return the most reliable identity under
// remoteItem.shared rather than the top-level shared facet, confirmed via
// live API testing on personal accounts (2026-03-06).
func (d *driveItemResponse) resolveSharedOwner() (name, email string) {
	switch {
	case d.RemoteItem != nil && d.RemoteItem.Shared != nil && d.RemoteItem.Shared.SharedBy != nil &&
		d.RemoteItem.Shared.SharedBy.User != nil:
		return d.RemoteItem.Shared.SharedBy.User.DisplayName, d.RemoteItem.Shared.SharedBy.User.Email
	case d.RemoteItem != nil && d.RemoteItem.Shared != nil && d.RemoteItem.Shared.Owner != nil &&
		d.RemoteItem.Shared.Owner.User != nil:
		return d.RemoteItem.Shared.Owner.User.DisplayName, d.RemoteItem.Shared.Owner.User.Email
	case d.RemoteItem != nil && d.RemoteItem.CreatedBy != nil && d.RemoteItem.CreatedBy.User != nil:
		return d.RemoteItem.CreatedBy.User.DisplayName, d.RemoteItem.CreatedBy.User.Email
	case d.Shared != nil && d.Shared.Owner != nil && d.Shared.Owner.User != nil:
		return d.Shared.Owner.User.DisplayName, d.Shared.Owner.User.Email
	default:
		return "", ""
	}
}

// parseTimestamp parses an RFC3339 timestamp and validates the year range.
// Invalid, missing, JSON-null-decoded, or out-of-range timestamps remain
// unknown and are represented as the zero time instead of fabricating a
// current timestamp.
// For deleted items, anomalies are logged at DEBUG (expected sparse API
// behavior); for live items, they're logged at WARN (genuinely unexpected).
func parseTimestamp(raw, field, itemID string, isDeleted bool, logger *slog.Logger) time.Time {
	logFunc := logger.Warn
	if isDeleted {
		logFunc = logger.Debug
	}

	if raw == "" {
		logFunc("empty timestamp, leaving unknown",
			slog.String("field", field),
			slog.String("item_id", itemID),
		)

		return time.Time{}
	}

	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		logFunc("invalid timestamp, leaving unknown",
			slog.String("field", field),
			slog.String("item_id", itemID),
			slog.String("raw", raw),
			slog.String("error", err.Error()),
		)

		return time.Time{}
	}

	if t.Year() < minValidYear || t.Year() > maxValidYear {
		logFunc("timestamp out of valid range, leaving unknown",
			slog.String("field", field),
			slog.String("item_id", itemID),
			slog.String("raw", raw),
		)

		return time.Time{}
	}

	return t
}
