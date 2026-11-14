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
	Value    []Permission `json:"value"`
	NextLink string       `json:"@odata.nextLink"`
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

// permissionApplicability says whom one permission row grants its roles to,
// as far as the row shows.
type permissionApplicability uint8

const (
	// permissionApplicabilityUnattributed is a row whose grantees carry no
	// e-mail address the client can compare: no sharing link, no granted
	// address and no owner role. A row granted to a group, an application or
	// a user without an address is one. Its roles are counted as the
	// caller's, like a matching grant's: the client cannot know group
	// membership, and a wrong guess ends in a refused write.
	permissionApplicabilityUnattributed permissionApplicability = iota
	permissionApplicabilityApplies
	permissionApplicabilityOtherPrincipal
	permissionApplicabilityAmbiguous
)

// EvaluateWriteAccess classifies the caller's effective write access from a
// Graph permissions payload:
//   - grants to the caller's address and sharing-link grants count as the
//     caller's
//   - grants to another address are ignored
//   - grants whose grantees carry no address (a group, an application, a
//     user without one) count as the caller's, since membership cannot be
//     checked
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
		case permissionApplicabilityUnattributed, permissionApplicabilityApplies:
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

	return permissionApplicabilityUnattributed
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

	var permissions []Permission

	for page := 1; apiPath != ""; page++ {
		if page > defaultMaxListPages {
			return nil, fmt.Errorf("graph: listing item permissions exceeded %d pages", defaultMaxListPages)
		}

		lpr, err := c.fetchPermissionsPage(ctx, apiPath)
		if err != nil {
			return nil, err
		}

		permissions = append(permissions, lpr.Value...)

		apiPath = ""
		if lpr.NextLink != "" {
			// Graph returns @odata.nextLink as an absolute URL, while c.do
			// prepends the base URL, so the link must be made relative first;
			// stripBaseURL also refuses a continuation off the Graph host.
			apiPath, err = c.stripBaseURL(lpr.NextLink)
			if err != nil {
				return nil, err
			}
		}
	}

	return permissions, nil
}

func (c *Client) fetchPermissionsPage(ctx context.Context, apiPath string) (*listPermissionsResponse, error) {
	resp, err := c.do(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

	var lpr listPermissionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&lpr); err != nil {
		return nil, fmt.Errorf("graph: decoding permissions response: %w", err)
	}

	return &lpr, nil
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
	c.logger.Debug("restoring item",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
	)

	apiPath := fmt.Sprintf("/drives/%s/items/%s/restore", driveID, itemID)

	// Restore takes optional JSON options; it is sent with none, and declares
	// JSON as it always has: the request has never been sent without the
	// header, and restore is exercised live only on Business.
	resp, err := c.doRequest(ctx, http.MethodPost, apiPath, nil, http.Header{headerContentType: {contentTypeJSON}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

	var dir driveItemResponse
	if err := json.NewDecoder(resp.Body).Decode(&dir); err != nil {
		return nil, fmt.Errorf("graph: decoding restore response: %w", err)
	}

	item := dir.toItem(c.logger)

	return &item, nil
}
