package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Permission represents a single permission entry on a drive item.
// Graph returns more than bare roles here: link-grant shape and granted-to
// identities are both needed to decide whether a role applies to the current
// caller or to some other principal such as the owner.
type Permission struct {
	ID                    string                  `json:"id"`
	Roles                 []string                `json:"roles"`
	ShareID               string                  `json:"shareId"`
	Link                  *permissionLink         `json:"link,omitempty"`
	GrantedToV2           *permissionIdentitySet  `json:"grantedToV2,omitempty"`
	GrantedTo             *permissionIdentitySet  `json:"grantedTo,omitempty"`
	GrantedToIdentitiesV2 []permissionIdentitySet `json:"grantedToIdentitiesV2,omitempty"`
	GrantedToIdentities   []permissionIdentitySet `json:"grantedToIdentities,omitempty"`
}

type permissionLink struct {
	Scope            string `json:"scope"`
	Type             string `json:"type"`
	WebURL           string `json:"webUrl"`
	PreventsDownload bool   `json:"preventsDownload"`
}

type permissionIdentitySet struct {
	User     *sharedUserFacet `json:"user,omitempty"`
	SiteUser *sharedUserFacet `json:"siteUser,omitempty"`
}

type listPermissionsResponse struct {
	Value []Permission `json:"value"`
}

// PermissionWriteAccess reports what the permissions payload proves about the
// caller's ability to write.
type PermissionWriteAccess uint8

const (
	PermissionWriteAccessInconclusive PermissionWriteAccess = iota
	PermissionWriteAccessReadOnly
	PermissionWriteAccessWritable
)

const permissionWriteAccessInconclusiveLabel = "inconclusive"

func (a PermissionWriteAccess) String() string {
	switch a {
	case PermissionWriteAccessInconclusive:
		return permissionWriteAccessInconclusiveLabel
	case PermissionWriteAccessReadOnly:
		return "read_only"
	case PermissionWriteAccessWritable:
		return "writable"
	}

	return permissionWriteAccessInconclusiveLabel
}

type permissionApplicability uint8

const (
	permissionApplicabilityLegacy permissionApplicability = iota
	permissionApplicabilityApplies
	permissionApplicabilityOtherPrincipal
	permissionApplicabilityAmbiguous
)

// EvaluateWriteAccess classifies the caller's effective write access from a
// Graph permissions payload. It is intentionally conservative:
//   - explicit matching grants or link grants count as caller-applicable
//   - explicit grants for some other principal are ignored
//   - owner-only entries do not imply the caller can write unless they match
//   - ambiguous write evidence yields Inconclusive so sync can fail open
func EvaluateWriteAccess(perms []Permission, accountEmail string) PermissionWriteAccess {
	accountEmail = strings.ToLower(strings.TrimSpace(accountEmail))

	var sawReadOnly bool
	var sawAmbiguousWrite bool

	for i := range perms {
		perm := perms[i]

		switch classifyPermissionApplicability(&perm, accountEmail) {
		case permissionApplicabilityOtherPrincipal:
			continue
		case permissionApplicabilityAmbiguous:
			if permissionGrantsWrite(&perm) {
				sawAmbiguousWrite = true
			}
		case permissionApplicabilityLegacy, permissionApplicabilityApplies:
			if permissionGrantsWrite(&perm) {
				return PermissionWriteAccessWritable
			}
			if permissionGrantsReadOnly(&perm) {
				sawReadOnly = true
			}
		}
	}

	if sawAmbiguousWrite {
		return PermissionWriteAccessInconclusive
	}
	if sawReadOnly {
		return PermissionWriteAccessReadOnly
	}

	return PermissionWriteAccessInconclusive
}

// HasWriteAccess returns true only when the permission payload positively
// proves the caller can write. It no longer treats unrelated owner rows as
// caller write access.
func HasWriteAccess(perms []Permission) bool {
	return EvaluateWriteAccess(perms, "") == PermissionWriteAccessWritable
}

func classifyPermissionApplicability(perm *Permission, accountEmail string) permissionApplicability {
	if permissionLinkCarriesGrant(perm.Link) {
		return permissionApplicabilityApplies
	}

	emails := permissionGrantedEmails(perm)
	if len(emails) > 0 {
		if accountEmail == "" {
			return permissionApplicabilityAmbiguous
		}
		for _, email := range emails {
			if email == accountEmail {
				return permissionApplicabilityApplies
			}
		}

		return permissionApplicabilityOtherPrincipal
	}

	if permissionHasOwnerRole(perm) {
		return permissionApplicabilityAmbiguous
	}

	return permissionApplicabilityLegacy
}

func permissionLinkCarriesGrant(link *permissionLink) bool {
	if link == nil {
		return false
	}

	return strings.TrimSpace(link.Type) != "" || strings.TrimSpace(link.Scope) != ""
}

func permissionGrantedEmails(perm *Permission) []string {
	var emails []string

	emails = appendPermissionIdentityEmail(emails, perm.GrantedToV2)
	emails = appendPermissionIdentityEmail(emails, perm.GrantedTo)

	for i := range perm.GrantedToIdentitiesV2 {
		emails = appendPermissionIdentityEmail(emails, &perm.GrantedToIdentitiesV2[i])
	}
	for i := range perm.GrantedToIdentities {
		emails = appendPermissionIdentityEmail(emails, &perm.GrantedToIdentities[i])
	}

	return emails
}

func appendPermissionIdentityEmail(emails []string, identity *permissionIdentitySet) []string {
	if identity == nil {
		return emails
	}
	if email := normalizedPermissionEmail(identity.User); email != "" {
		return append(emails, email)
	}
	if email := normalizedPermissionEmail(identity.SiteUser); email != "" {
		return append(emails, email)
	}

	return emails
}

func normalizedPermissionEmail(user *sharedUserFacet) string {
	if user == nil {
		return ""
	}

	return strings.ToLower(strings.TrimSpace(user.Email))
}

func permissionGrantsWrite(perm *Permission) bool {
	if perm.Link != nil && strings.EqualFold(perm.Link.Type, "edit") {
		return true
	}
	for _, role := range perm.Roles {
		if strings.EqualFold(role, "write") || strings.EqualFold(role, "owner") {
			return true
		}
	}

	return false
}

func permissionGrantsReadOnly(perm *Permission) bool {
	if permissionGrantsWrite(perm) {
		return false
	}
	if perm.Link != nil && strings.EqualFold(perm.Link.Type, "view") {
		return true
	}
	for _, role := range perm.Roles {
		if strings.EqualFold(role, "read") {
			return true
		}
	}

	return false
}

func permissionHasOwnerRole(perm *Permission) bool {
	for _, role := range perm.Roles {
		if strings.EqualFold(role, "owner") {
			return true
		}
	}

	return false
}

// ListItemPermissions returns the permissions for a drive item. For non-owner
// callers, Graph may still include owner and link grants in addition to the
// caller's own grant. Callers must evaluate the returned permission set rather
// than assuming every row applies to the authenticated principal.
func (c *Client) ListItemPermissions(ctx context.Context, driveID driveid.ID, itemID string) ([]Permission, error) {
	c.logger.Debug("listing item permissions",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
	)

	apiPath := fmt.Sprintf("/drives/%s/items/%s/permissions", driveID, itemID)

	resp, err := c.do(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var lpr listPermissionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&lpr); err != nil {
		return nil, fmt.Errorf("graph: decoding permissions response: %w", err)
	}

	return lpr.Value, nil
}

// ListRecycleBinItems returns all items in the drive's recycle bin.
func (c *Client) ListRecycleBinItems(
	ctx context.Context, driveID driveid.ID,
) ([]Item, error) {
	return c.fetchAllChildren(
		ctx,
		fmt.Sprintf("/drives/%s/special/recyclebin/children?$top=%d",
			driveID, listChildrenPageSize),
		"listing recycle bin items",
		"listed recycle bin items complete",
		[]slog.Attr{
			slog.String("drive_id", driveID.String()),
		},
	)
}

// RestoreItem restores a deleted item from the recycle bin to its original
// location. Returns the restored item. Returns ErrConflict if an item with
// the same name already exists at the original location.
func (c *Client) RestoreItem(
	ctx context.Context, driveID driveid.ID, itemID string,
) (*Item, error) {
	c.logger.Info("restoring item",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
	)

	apiPath := fmt.Sprintf("/drives/%s/items/%s/restore", driveID, itemID)

	resp, err := c.do(ctx, http.MethodPost, apiPath, http.NoBody)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dir driveItemResponse
	if err := json.NewDecoder(resp.Body).Decode(&dir); err != nil {
		return nil, fmt.Errorf("graph: decoding restore response: %w", err)
	}

	item := dir.toItem(c.logger)

	return &item, nil
}
