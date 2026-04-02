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

func validateUploadURL(parsed *url.URL) error {
	return validateTrustedPreAuthURL(
		parsed,
		"upload",
		"1drv.com",
		"microsoftpersonalcontent.com",
		"onedrive.com",
		"sharepoint.com",
		"sharepoint-df.com",
		"sharepoint.us",
		"sharepoint-df.us",
		"storage.live.com",
	)
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

func parseAndValidateUploadURL(raw UploadURL, validate func(*url.URL) error) (string, error) {
	parsed, err := url.Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("graph: parsing upload URL: %w", err)
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

func isLoopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
