package graph

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var (
	bearerTokenPattern = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
	jsonTokenPattern   = regexp.MustCompile(`(?i)("?(?:access_token|refresh_token|client_secret|authorization)"?\s*:\s*")([^"]+)(")`)
	urlPattern         = regexp.MustCompile(`https://[^\s"'<>]+`)
)

const (
	// websocketSecureScheme is the scheme the Socket.IO notification URL
	// carries once observation has rewritten it for the websocket dial.
	websocketSecureScheme = "wss"
	// socketIONotificationTokenQueryKey names the query parameter that holds
	// the Socket.IO notification URL's credential.
	socketIONotificationTokenQueryKey = "snthgk"
	// socketIONotificationWebsocketPath is the path observation gives the
	// notification URL when dialing it.
	socketIONotificationWebsocketPath = "/socket.io"
)

func sanitizeGraphErrorText(raw string) string {
	if raw == "" {
		return ""
	}

	if sanitized, ok := sanitizeGraphErrorJSON(raw); ok {
		return redactSensitiveURLs(sanitized)
	}

	redacted := bearerTokenPattern.ReplaceAllString(raw, "Bearer [REDACTED]")
	redacted = jsonTokenPattern.ReplaceAllString(redacted, `${1}[REDACTED]${3}`)

	return redactSensitiveURLs(redacted)
}

func sanitizeGraphErrorJSON(raw string) (string, bool) {
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return "", false
	}

	sanitized := sanitizeJSONValue(decoded)
	body, err := json.Marshal(sanitized)
	if err != nil {
		return "", false
	}

	return string(body), true
}

func sanitizeJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		sanitized := make(map[string]any, len(typed))
		for key, child := range typed {
			if isSensitiveJSONKey(key) {
				sanitized[key] = "[REDACTED]"
				continue
			}

			sanitized[key] = sanitizeJSONValue(child)
		}

		return sanitized
	case []any:
		sanitized := make([]any, len(typed))
		for i := range typed {
			sanitized[i] = sanitizeJSONValue(typed[i])
		}

		return sanitized
	case string:
		return sanitizeGraphErrorString(typed)
	default:
		return value
	}
}

func isSensitiveJSONKey(key string) bool {
	switch strings.ToLower(key) {
	case "access_token", "authorization", "client_secret", "downloadurl", "notificationurl", "refresh_token", "uploadurl":
		return true
	default:
		return false
	}
}

func sanitizeGraphErrorString(raw string) string {
	redacted := bearerTokenPattern.ReplaceAllString(raw, "Bearer [REDACTED]")
	redacted = redactSensitiveURLs(redacted)

	return redacted
}

func redactSensitiveURLs(raw string) string {
	return urlPattern.ReplaceAllStringFunc(raw, func(candidate string) string {
		trimmed, suffix := trimTrailingURLPunctuation(candidate)
		if !isSensitiveURL(trimmed) {
			return candidate
		}

		return "[REDACTED_URL]" + suffix
	})
}

func trimTrailingURLPunctuation(raw string) (string, string) {
	trimmed := strings.TrimRight(raw, ".,);]")
	return trimmed, raw[len(trimmed):]
}

func isSensitiveURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}

	// wss is the websocket spelling of the same Socket.IO notification URL.
	if parsed.Scheme != httpsScheme && parsed.Scheme != websocketSecureScheme {
		return false
	}

	hostname := parsed.Hostname()
	if hostname == "" {
		return false
	}

	if matchesAllowedHost(
		hostname,
		"1drv.com",
		"livefilestore.com",
		"microsoftpersonalcontent.com",
		"onedrive.com",
		"sharepoint.com",
		"sharepoint-df.com",
		"sharepoint.us",
		"sharepoint-df.us",
		"svc.ms",
		"storage.live.com",
	) {
		if matchesAllowedHost(hostname, "svc.ms") && !isSocketIONotificationURL(parsed) {
			return false
		}

		return true
	}

	if matchesAllowedHost(
		hostname,
		"graph.microsoft.com",
		"graph.microsoft.us",
		"dod-graph.microsoft.us",
		"microsoftgraph.chinacloudapi.cn",
	) && strings.Contains(strings.ToLower(parsed.Path), "/operations/") {
		return true
	}

	queryKeys := []string{
		"access_token",
		"authkey",
		"sig",
		socketIONotificationTokenQueryKey,
		"tempauth",
		"token",
	}
	query := parsed.Query()
	for _, key := range queryKeys {
		if query.Has(key) {
			return true
		}
	}

	return false
}

// isSocketIONotificationURL recognizes the Socket.IO notification URL on an
// svc.ms host in both of its spellings: the /callback URL Graph returns and
// the /socket.io websocket URL the dial rewrites it to. Either carries the
// credential in its snthgk query parameter, so a bare snthgk counts too.
func isSocketIONotificationURL(parsed *url.URL) bool {
	path := strings.ToLower(parsed.Path)
	if strings.Contains(path, "/callback") || strings.Contains(path, socketIONotificationWebsocketPath) {
		return true
	}

	return parsed.Query().Has(socketIONotificationTokenQueryKey)
}

// redactURLError scrubs the request URL that net/http embeds in a *url.Error
// when that URL carries a credential. Only the scheme and host survive, which
// is enough to tell a user which service failed.
//
// It edits the *url.Error in place, so it must run on the error as the HTTP
// client returned it, before anything wraps it: fmt.Errorf captures the
// wrapped text at wrap time, and a later scrub no longer reaches it
// (TestRedactURLError_MustRunBeforeWrapping).
func redactURLError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) || !isSensitiveURL(urlErr.URL) {
		return err
	}

	redacted := "[REDACTED_URL]"
	if parsed, parseErr := url.Parse(urlErr.URL); parseErr == nil && parsed.Host != "" {
		redacted = parsed.Scheme + "://" + parsed.Host + "/[REDACTED]"
	}

	urlErr.URL = redacted

	return err
}
