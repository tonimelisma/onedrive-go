package graph

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const graphSharingURLPrefix = "u!"

// ResolveShareURL resolves a raw OneDrive sharing URL through the Graph shares
// API and returns the normalized drive item it points to.
func (c *Client) ResolveShareURL(ctx context.Context, rawURL string) (*Item, error) {
	shareToken, err := encodeSharingURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("graph: encoding sharing URL: %w", err)
	}

	resp, err := c.do(ctx, http.MethodGet, "/shares/"+shareToken+"/driveItem", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dir driveItemResponse
	if err := json.NewDecoder(resp.Body).Decode(&dir); err != nil {
		return nil, fmt.Errorf("graph: decoding item response: %w", err)
	}

	item := dir.toItem(c.logger)

	// `/shares/.../driveItem` resolves to the owner-side item. Make that raw
	// owner identity available through the same Remote* fields used by shared
	// discovery so higher layers can normalize on one identity shape.
	if item.RemoteDriveID == "" && dir.ParentReference != nil {
		item.RemoteDriveID = driveid.New(dir.ParentReference.DriveID).String()
	}
	if item.RemoteItemID == "" {
		item.RemoteItemID = item.ID
	}

	return &item, nil
}

func encodeSharingURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parsing sharing URL: %w", err)
	}

	if parsed.Scheme != "https" {
		return "", fmt.Errorf("sharing URL must use https")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("sharing URL host is empty")
	}

	return graphSharingURLPrefix + base64.RawURLEncoding.EncodeToString([]byte(parsed.String())), nil
}
