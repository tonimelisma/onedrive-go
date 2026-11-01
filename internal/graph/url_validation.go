package graph

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

const httpsScheme = "https"

func validateGraphBaseURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("graph: parsing base URL: %w", err)
	}

	if parsed.User != nil {
		return fmt.Errorf("graph: base URL must not contain userinfo")
	}

	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("graph: base URL must not contain query or fragment")
	}

	if parsed.Hostname() == "" {
		return fmt.Errorf("graph: base URL host is empty")
	}

	if isLoopbackHostname(parsed.Hostname()) {
		if parsed.Scheme != deltaHTTPPrefix && parsed.Scheme != httpsScheme {
			return fmt.Errorf("graph: loopback base URL must use http or https")
		}

		return nil
	}

	if parsed.Scheme != httpsScheme {
		return fmt.Errorf("graph: base URL must use https")
	}

	if !matchesAllowedHost(
		parsed.Hostname(),
		"graph.microsoft.com",
		"graph.microsoft.us",
		"dod-graph.microsoft.us",
		"microsoftgraph.chinacloudapi.cn",
	) {
		return fmt.Errorf("graph: base URL host %q is not allowed", parsed.Hostname())
	}

	return nil
}

// preAuthContentHosts are the Microsoft hosts that serve pre-authenticated
// content traffic: upload sessions, `@microsoft.graph.downloadUrl` and the
// `/content` redirect target all land on this family. One list, so a host
// learned for one direction (Personal accounts on
// `microsoftpersonalcontent.com`, found by live upload coverage) is trusted
// for the other too.
func preAuthContentHosts() []string {
	return []string{
		"1drv.com",
		"microsoftpersonalcontent.com",
		"onedrive.com",
		"sharepoint.com",
		"sharepoint-df.com",
		"sharepoint.us",
		"sharepoint-df.us",
		"storage.live.com",
	}
}

func validateUploadURL(parsed *url.URL) error {
	return validateTrustedPreAuthURL(parsed, "upload", preAuthContentHosts()...)
}

// validateDownloadURL guards `@microsoft.graph.downloadUrl` before the
// unauthenticated download request is built. The URL is server-supplied, so
// without this a spoofed metadata answer could send the request anywhere.
func validateDownloadURL(parsed *url.URL) error {
	return validateTrustedPreAuthURL(parsed, "download", preAuthContentHosts()...)
}

func validateCopyMonitorURL(parsed *url.URL) error {
	return validateTrustedPreAuthURL(
		parsed,
		"copy monitor",
		"1drv.com",
		"microsoftpersonalcontent.com",
		"onedrive.com",
		"sharepoint.com",
		"sharepoint-df.com",
		"sharepoint.us",
		"sharepoint-df.us",
		"storage.live.com",
		"graph.microsoft.com",
		"graph.microsoft.us",
		"dod-graph.microsoft.us",
		"microsoftgraph.chinacloudapi.cn",
	)
}

func validateSocketIONotificationURL(parsed *url.URL) error {
	return validateTrustedPreAuthURL(
		parsed,
		"socket.io notification",
		"svc.ms",
	)
}

func validateTrustedPreAuthURL(parsed *url.URL, kind string, allowedHosts ...string) error {
	if parsed == nil {
		return fmt.Errorf("graph: %s URL is nil", kind)
	}

	if parsed.User != nil {
		return fmt.Errorf("graph: %s URL must not contain userinfo", kind)
	}

	if parsed.Scheme != httpsScheme {
		return fmt.Errorf("graph: %s URL must use https", kind)
	}

	hostname := parsed.Hostname()
	if hostname == "" {
		return fmt.Errorf("graph: %s URL host is empty", kind)
	}

	if net.ParseIP(hostname) != nil {
		return fmt.Errorf("graph: %s URL host %q must not be an IP literal", kind, hostname)
	}

	if !matchesAllowedHost(hostname, allowedHosts...) {
		return fmt.Errorf("graph: %s URL host %q is not allowed", kind, hostname)
	}

	return nil
}

// parseAndValidatePreAuthURL parses a server-supplied pre-authenticated URL
// and runs validate on it. kind names the URL in error text ("upload",
// "download", "copy monitor"); the raw URL itself never appears there.
func parseAndValidatePreAuthURL(raw string, kind string, validate func(*url.URL) error) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		// Not wrapped: *url.Error embeds the raw pre-authenticated URL, which
		// carries its credential, in its message.
		return "", fmt.Errorf("graph: parsing %s URL: OneDrive returned a malformed %s URL", kind, kind)
	}

	if err := validate(parsed); err != nil {
		return "", err
	}

	return parsed.String(), nil
}

func matchesAllowedHost(host string, allowed ...string) bool {
	normalized := strings.ToLower(host)
	for _, candidate := range allowed {
		if normalized == candidate || strings.HasSuffix(normalized, "."+candidate) {
			return true
		}
	}

	return false
}

// loopbackTolerant accepts loopback http/https URLs ahead of validate. A
// client whose base URL is loopback is a test fixture (validateGraphBaseURL
// admits loopback for that reason alone), and the fixture's pre-authenticated
// URLs point back at the same loopback server. Production clients, whose
// base URL is a Graph host, never get this wrapper, so their allow-lists stay
// exactly validate's.
func loopbackTolerant(validate func(*url.URL) error) func(*url.URL) error {
	return func(parsed *url.URL) error {
		if parsed != nil && isLoopbackHostname(parsed.Hostname()) &&
			(parsed.Scheme == deltaHTTPPrefix || parsed.Scheme == httpsScheme) {
			return nil
		}

		return validate(parsed)
	}
}

// isLoopbackBaseURL reports whether an already validated base URL points at
// loopback, the only non-Graph host validateGraphBaseURL admits.
func isLoopbackBaseURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && isLoopbackHostname(parsed.Hostname())
}

func isLoopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
