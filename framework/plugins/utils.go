package plugins

import (
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
)

var (
	ErrPluginNotFound = fmt.Errorf("plugin not found")
)

// pluginDownloadClientForRequest returns the http.Client used for an
// individual DownloadPlugin call. Production returns an SSRF-safe client
// from bifrost.NewSSRFSafeClient (which re-validates each redirect target
// against bifrost.ValidateExternalURL). Tests can override this hook to
// inject a per-test client whose Transport routes a synthetic example.com
// to a local httptest.Server while keeping the validator in play.
var pluginDownloadClientForRequest = func() *http.Client {
	return bifrost.NewSSRFSafeClient(120 * time.Second)
}

// DownloadPlugin downloads a plugin from a validated external URL and
// returns the local file path. SSRF protection has three layers:
//
//  1. bifrost.ValidateExternalURL performs the project-wide scheme +
//     hostname + IP-range check on the original URL string.
//  2. validatePluginURL (below) repeats the IP-resolution + private-range
//     rejection inline so CodeQL's intra-procedural taint tracker can see
//     the sanitization on the path to the http.NewRequest sink (closes
//     CodeQL go/request-forgery).
//  3. The client returned by pluginDownloadClientForRequest re-runs
//     ValidateExternalURL on every redirect via CheckRedirect, so an open
//     redirect cannot pivot to an internal address mid-download.
func DownloadPlugin(pluginURL string, extension string) (string, error) {
	if err := bifrost.ValidateExternalURL(pluginURL, false); err != nil {
		return "", fmt.Errorf("invalid plugin URL: %w", err)
	}

	sanitized, err := validatePluginURL(pluginURL)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodGet, sanitized, nil)
	if err != nil {
		return "", fmt.Errorf("failed to build plugin download request: %w", err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := pluginDownloadClientForRequest().Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to download plugin: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to download plugin: HTTP %d", resp.StatusCode)
	}

	// Cap reads at 64MiB so a malicious or misconfigured upstream can't
	// exhaust memory by streaming an unbounded body.
	const maxPluginSize = 64 * 1024 * 1024
	var body io.Reader = http.MaxBytesReader(nil, resp.Body, maxPluginSize)
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(body)
		if err != nil {
			return "", fmt.Errorf("failed to initialize gzip decoder: %w", err)
		}
		defer gz.Close()
		body = gz
	}

	tempFile, err := os.CreateTemp(os.TempDir(), "bifrost-plugin-*"+extension)
	if err != nil {
		return "", fmt.Errorf("failed to create temporary file: %w", err)
	}
	tempPath := tempFile.Name()

	if _, err := io.Copy(tempFile, body); err != nil {
		tempFile.Close()
		os.Remove(tempPath)
		return "", fmt.Errorf("failed to write plugin to temporary file: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		os.Remove(tempPath)
		return "", fmt.Errorf("failed to close temporary file: %w", err)
	}

	if extension == ".so" {
		if err := os.Chmod(tempPath, 0755); err != nil {
			os.Remove(tempPath)
			return "", fmt.Errorf("failed to set executable permissions on plugin: %w", err)
		}
	}

	return tempPath, nil
}

// validatePluginURL performs the SSRF-relevant checks on the plugin URL
// inline, so CodeQL's intra-procedural go/request-forgery analyzer can see
// the sanitization on the path from the caller-supplied URL to the
// downstream http.NewRequest sink. This mirrors bifrost.ValidateExternalURL
// (which DownloadPlugin already calls), but the cross-package check isn't
// recognized by CodeQL's default taint model.
//
// The returned URL is reconstructed from the parsed components (literal
// allowlisted scheme + verified non-private host) so the value flowing
// out of this function is provably derived from the validated inputs
// rather than carrying the raw caller-supplied string forward.
func validatePluginURL(pluginURL string) (string, error) {
	parsed, err := url.Parse(pluginURL)
	if err != nil {
		return "", fmt.Errorf("invalid plugin URL: %w", err)
	}

	// Allowlist: only http/https. Comparison-with-literal severs taint.
	var scheme string
	switch parsed.Scheme {
	case "http":
		scheme = "http"
	case "https":
		scheme = "https"
	default:
		return "", fmt.Errorf("plugin URL scheme not allowed: %q", parsed.Scheme)
	}

	hostname := parsed.Hostname()
	if hostname == "" {
		return "", fmt.Errorf("plugin URL must include a hostname")
	}

	// Resolve the hostname and reject if any returned address is
	// loopback, private, link-local, multicast, or unspecified.
	// net.IP.IsPrivate/IsLoopback comparisons are CodeQL-recognized
	// SSRF sanitizers that break the request-forgery taint chain.
	ips, err := net.LookupIP(hostname)
	if err != nil {
		return "", fmt.Errorf("plugin URL host resolution failed: %w", err)
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
			ip.IsMulticast() || ip.IsUnspecified() {
			return "", fmt.Errorf("plugin URL resolves to disallowed address: %s", ip)
		}
	}

	// Rebuild the URL from the verified scheme + parsed host/path/query.
	// scheme is a literal here, parsed.Host has been resolved + checked.
	out := &url.URL{
		Scheme:   scheme,
		Host:     parsed.Host,
		Path:     parsed.EscapedPath(),
		RawQuery: parsed.RawQuery,
	}
	return out.String(), nil
}
