package graph

import (
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// userResponse mirrors the Graph API /me JSON response.
// Unexported — callers use User via toUser() normalization.
type userResponse struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Mail        string `json:"mail"`
	// UPN is a fallback when mail is empty (common on Personal accounts
	// where the mail field is often blank).
	UPN string `json:"userPrincipalName"`
}

// toUser normalizes a Graph API user response into our User type.
// Email resolution: prefer `mail` (authoritative for Business accounts),
// fall back to `userPrincipalName` when mail is empty (Personal accounts).
// See B-280 for the rationale behind this fallback chain.
func (u *userResponse) toUser() User {
	email := u.Mail
	if email == "" {
		email = u.UPN
	}

	return User{
		ID:          u.ID,
		DisplayName: u.DisplayName,
		Email:       email,
	}
}

// driveResponse mirrors the Graph API drive JSON response.
// Unexported — callers use Drive via toDrive() normalization.
type driveResponse struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	DriveType string      `json:"driveType"`
	Owner     *ownerFacet `json:"owner"`
	Quota     *quotaFacet `json:"quota"`
}

// ownerFacet represents the owner block in a Graph API drive response.
type ownerFacet struct {
	User struct {
		DisplayName string `json:"displayName"`
		Email       string `json:"email"`
	} `json:"user"`
}

// quotaFacet represents the quota block in a Graph API drive response.
type quotaFacet struct {
	Used  int64 `json:"used"`
	Total int64 `json:"total"`
}

// drivesListResponse wraps the value array from GET /me/drives.
type drivesListResponse struct {
	Value []driveResponse `json:"value"`
}

// toDrive normalizes a Graph API drive response into our Drive type.
// Nil-safe for optional owner and quota facets.
func (d *driveResponse) toDrive() Drive {
	drive := Drive{
		ID:        driveid.New(d.ID),
		Name:      d.Name,
		DriveType: d.DriveType,
	}

	if d.Owner != nil {
		drive.OwnerName = d.Owner.User.DisplayName
		drive.OwnerEmail = d.Owner.User.Email
	}

	if d.Quota != nil {
		drive.QuotaUsed = d.Quota.Used
		drive.QuotaTotal = d.Quota.Total
	}

	return drive
}

// siteResponse mirrors the Graph API site JSON response.
// Unexported — callers use Site via toSite() normalization.
type siteResponse struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Name        string `json:"name"`
	WebURL      string `json:"webUrl"`
}

// sitesListResponse wraps the value array from GET /sites?search=.
type sitesListResponse struct {
	Value []siteResponse `json:"value"`
}

// toSite normalizes a Graph API site response into our Site type.
func (s *siteResponse) toSite() Site {
	return Site{
		ID:          s.ID,
		DisplayName: s.DisplayName,
		Name:        s.Name,
		WebURL:      s.WebURL,
	}
}

// orgResponse mirrors the Graph API /me/organization response.
// Unexported — callers use Organization via the Organization() method.
type orgResponse struct {
	Value []orgEntry `json:"value"`
}

// orgEntry represents a single organization entry from the Graph API.
type orgEntry struct {
	DisplayName string `json:"displayName"`
}
