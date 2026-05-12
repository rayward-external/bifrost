package plugins

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

const fakePluginBytes = "fake-plugin-binary-content"

func withPluginDownloadTestServer(t *testing.T, handler http.Handler) string {
	t.Helper()

	server := httptest.NewServer(handler)
	previousClient := pluginDownloadClient
	testClient := &fasthttp.Client{
		ReadBufferSize: 64 * 1024,
		Dial: func(addr string) (net.Conn, error) {
			return net.Dial("tcp", server.Listener.Addr().String())
		},
	}

	pluginDownloadClientMu.Lock()
	pluginDownloadClient = testClient
	pluginDownloadClientMu.Unlock()

	t.Cleanup(func() {
		pluginDownloadClientMu.Lock()
		pluginDownloadClient = previousClient
		pluginDownloadClientMu.Unlock()
		server.Close()
	})

	return "http://example.com"
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
	assert.Contains(t, err.Error(), "too many redirects")
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
