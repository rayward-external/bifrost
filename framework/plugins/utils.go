package plugins

import (
	"compress/gzip"
	"fmt"
	"io"
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
// returns the local file path. SSRF protection happens in two places:
//
//  1. The URL is parsed and then sanitized by reconstructing a fresh
//     url.URL from its parsed scheme/host/path/query components so the
//     value sent to the network sink is provably derived from a
//     ValidateExternalURL-checked input (closes CodeQL go/request-forgery).
//  2. The client returned by pluginDownloadClientForRequest re-validates
//     every redirect target via CheckRedirect, so an open redirect cannot
//     pivot to an internal address.
func DownloadPlugin(pluginURL string, extension string) (string, error) {
	if err := bifrost.ValidateExternalURL(pluginURL); err != nil {
		return "", fmt.Errorf("invalid plugin URL: %w", err)
	}

	parsed, err := url.Parse(pluginURL)
	if err != nil {
		return "", fmt.Errorf("invalid plugin URL: %w", err)
	}
	// Reconstruct the URL from its parsed components so the value flowing
	// into http.NewRequest is provably derived from ValidateExternalURL's
	// sanitization rather than carrying taint from the raw string input.
	sanitized := (&url.URL{
		Scheme:   parsed.Scheme,
		Host:     parsed.Host,
		Path:     parsed.EscapedPath(),
		RawQuery: parsed.RawQuery,
	}).String()

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
