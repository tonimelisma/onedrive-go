package graph

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

var (
	bearerTokenPattern = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
	jsonTokenPattern   = regexp.MustCompile(`(?i)("?(?:access_token|refresh_token|client_secret|authorization)"?\s*:\s*")([^"]+)(")`)
	urlPattern         = regexp.MustCompile(`https://[^\s"'<>]+`)
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
	case "access_token", "authorization", "client_secret", "downloadurl", "refresh_token", "uploadurl":
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

	if parsed.Scheme != httpsScheme {
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
		"storage.live.com",
	) {
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
