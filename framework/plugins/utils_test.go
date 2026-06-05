package plugins

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fakePluginBytes = "fake-plugin-binary-content"

// withPluginDownloadTestServer spins up an httptest.Server and swaps the
// package-private pluginDownloadClientForRequest hook so DownloadPlugin uses
// a per-test http.Client whose Transport routes the synthetic
// "http://example.com" host (which passes ValidateExternalURL) to the local
// test server. The production client is never mutated.
func withPluginDownloadTestServer(t *testing.T, handler http.Handler) string {
	t.Helper()

	server := httptest.NewServer(handler)
	testClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &rewriteTransport{
			target: server.Listener.Addr().String(),
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if err := bifrost.ValidateExternalURL(req.URL.String(), false); err != nil {
				return err
			}
			return nil
		},
	}

	previous := pluginDownloadClientForRequest
	pluginDownloadClientForRequest = func() *http.Client { return testClient }

	t.Cleanup(func() {
		pluginDownloadClientForRequest = previous
		server.Close()
	})

	return "http://example.com"
}

// rewriteTransport rewrites the Host of every outbound request to the local
// httptest server's listener address so requests for the synthetic
// "example.com" host (which passes ValidateExternalURL) reach the test
// server. The original host header is preserved so handlers see "example.com".
type rewriteTransport struct {
	target string
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.URL.Host = rt.target
	cloned.URL.Scheme = "http"
	cloned.Host = req.URL.Host
	return http.DefaultTransport.RoundTrip(cloned)
}

func TestDownloadPlugin_DirectDownload(t *testing.T) {
	baseURL := withPluginDownloadTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(fakePluginBytes))
	}))

	path, err := DownloadPlugin(baseURL+"/download", ".so")
	require.NoError(t, err)
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, fakePluginBytes, string(data))
}

func TestDownloadPlugin_FollowsRedirect(t *testing.T) {
	baseURL := withPluginDownloadTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "http://example.com/final", http.StatusFound)
		case "/final":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(fakePluginBytes))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	path, err := DownloadPlugin(baseURL+"/redirect", ".so")
	require.NoError(t, err)
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, fakePluginBytes, string(data))
}

func TestDownloadPlugin_TooManyRedirects(t *testing.T) {
	baseURL := withPluginDownloadTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com/loop", http.StatusFound)
	}))

	_, err := DownloadPlugin(baseURL+"/loop", ".so")
	require.Error(t, err)
	// net/http reports "stopped after N redirects" when the per-client
	// redirect cap is exceeded.
	msg := strings.ToLower(err.Error())
	assert.True(t,
		strings.Contains(msg, "stopped after") || strings.Contains(msg, "too many redirects"),
		"expected redirect-cap error, got: %v", err)
}

func TestDownloadPlugin_NonOKStatus(t *testing.T) {
	baseURL := withPluginDownloadTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	_, err := DownloadPlugin(baseURL+"/missing", ".so")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func TestDownloadPlugin_FileExtensionPreserved(t *testing.T) {
	baseURL := withPluginDownloadTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(fakePluginBytes))
	}))

	path, err := DownloadPlugin(baseURL+"/download", ".so")
	require.NoError(t, err)
	defer os.Remove(path)

	assert.Contains(t, path, ".so")
}
