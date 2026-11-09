package handlers

// Fork-only test (not in upstream): the relay data plane's stream goes through
// lib.WrapSSEForExternalAudience like every other response stream. The repo-wide
// guard in lib/external_audience_stream_guard_test.go only proves the call is
// present; this proves it is wired to the bytes the caller actually reads.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayStreamScrubsExtraFieldsForExternalAudience(t *testing.T) {
	const chunk = `{"id":"c1","delta":"hi","extra_fields":{"routing_info":{"key":"k1"},"provider":"p1"}}`
	upstreamBody := "data: " + chunk + "\n\n" + "data: " + chunk + "\n\n" + "data: [DONE]\n\n"

	for _, tc := range []struct {
		name        string
		contentType string
		external    bool
		wantScrub   bool
	}{
		{"external caller, SSE reply is scrubbed", "text/event-stream; charset=utf-8", true, true},
		{"internal caller, SSE reply is untouched", "text/event-stream; charset=utf-8", false, false},
		{"external caller, non-SSE reply is untouched", "application/octet-stream", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, upstreamBody)
			}))
			defer upstream.Close()
			base := serveRelayGatewayHTTP(t, streamingRelay(t, "aud", upstream.URL))

			req, err := http.NewRequest(http.MethodPost, base+"/relay/aud/s", strings.NewReader(`{}`))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			if tc.external {
				req.Header.Set(lib.AudienceRequestHeader, lib.ExternalAudience)
			}
			resp, err := (&http.Client{}).Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			text := strings.ReplaceAll(string(body), ": heartbeat\n", "")

			if !tc.wantScrub {
				assert.Equal(t, upstreamBody, text, "the upstream's bytes arrive unchanged")
				return
			}
			assert.NotContains(t, text, "extra_fields", "an external caller must not receive extra_fields on a relayed SSE stream")
			assert.Equal(t, 2, strings.Count(text, `"delta":"hi"`), "every chunk's payload survives the scrub")
			assert.Contains(t, text, "data: [DONE]\n\n", "the stream terminator survives the scrub")
		})
	}
}
