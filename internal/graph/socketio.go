package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

type socketIOEndpointResponse struct {
	ID                 string `json:"id"`
	NotificationURL    string `json:"notificationUrl"`
	ExpirationDateTime string `json:"expirationDateTime"`
}

// SocketIOEndpoint fetches the outbound Socket.IO notification endpoint for a
// drive root. The returned notification URL is sensitive and validated before
// leaving the Graph boundary.
func (c *Client) SocketIOEndpoint(ctx context.Context, driveID driveid.ID) (*SocketIOEndpoint, error) {
	resp, err := c.doGetWithHeaders(ctx, fmt.Sprintf("/drives/%s/root/subscriptions/socketIo", driveID), http.Header{
		"Content-Type": {"application/json"},
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var decoded socketIOEndpointResponse
	if decodeErr := json.NewDecoder(resp.Body).Decode(&decoded); decodeErr != nil {
		return nil, fmt.Errorf("graph: decoding socket.io endpoint response: %w", decodeErr)
	}

	if decoded.NotificationURL == "" {
		return nil, fmt.Errorf("graph: socket.io endpoint response missing notificationUrl")
	}

	notificationURL, err := c.validatedSocketIONotificationURL(SocketIONotificationURL(decoded.NotificationURL))
	if err != nil {
		return nil, fmt.Errorf("graph: invalid socket.io notification URL: %w", err)
	}

	var expiration time.Time
	if decoded.ExpirationDateTime != "" {
		expiration, err = time.Parse(time.RFC3339, decoded.ExpirationDateTime)
		if err != nil {
			return nil, fmt.Errorf("graph: parsing socket.io expiration: %w", err)
		}
	}

	return &SocketIOEndpoint{
		ID:              decoded.ID,
		NotificationURL: SocketIONotificationURL(notificationURL),
		ExpirationTime:  expiration,
	}, nil
}

func (c *Client) validatedSocketIONotificationURL(raw SocketIONotificationURL) (string, error) {
	parsed, err := url.Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("graph: parsing socket.io notification URL: invalid URL")
	}

	if err := c.socketIOValidator(parsed); err != nil {
		return "", err
	}

	return parsed.String(), nil
}
