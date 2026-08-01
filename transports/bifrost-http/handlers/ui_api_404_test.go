package handlers

import (
	"embed"
	"encoding/json"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
)

// An unmatched path under an API namespace must NOT fall through to the SPA.
//
// RegisterRoutes installs `GET /{filepath:*}` as a global catch-all, registered
// after every real route, so it swallows unmatched GETs before Router.NotFound
// (which already emits a correct JSON 404) can ever fire. The result observed
// live on the internal gateway:
//
//	GET /v1/models/claude-haiku-4-5 -> 200 text/html (3657 bytes, the dashboard shell)
//
// An OpenAI SDK calling client.models.retrieve() gets HTTP 200 with an HTML
// document and dies in JSON decoding, instead of receiving a clean 404.
//
// The UI must keep its SPA fallback for genuine UI routes — that is what makes
// client-side routing work — so this is scoped to API namespaces only.
func TestIsAPIPath(t *testing.T) {
	apiPaths := []string{
		// Inference API.
		"/v1/models/claude-haiku-4-5",
		"/v1/definitely-not-a-route",
		"/v1/chat/completions/extra",
		"/v1",
		"/v1/",
		// Provider-pinned routes: /{provider}/v1/...
		"/openai/v1/models/foo",
		"/anthropic/v1/nope",
		"/vertex/v1/",
		// Admin API.
		"/api/not-a-route",
		"/api",
		"/api/",
	}
	for _, p := range apiPaths {
		t.Run("api"+p, func(t *testing.T) {
			if !isAPIPath(p) {
				t.Errorf("isAPIPath(%q) = false, want true — this path would serve SPA HTML with 200", p)
			}
		})
	}

	// These must keep the SPA fallback. Breaking any of them breaks the admin UI,
	// which is a far worse outcome than the bug being fixed.
	uiPaths := []string{
		"/",
		"/logs",
		"/providers",
		"/config/providers",
		"/virtual-keys",
		"/index.html",
		"/_next/static/chunk.js",
		"/favicon.ico",
		// Near-misses that must NOT be treated as API paths.
		"/v1beta",
		"/v10/foo",
		"/apiary",
		"/versions",
		"/provider/v2/thing",
		"/ui/v1x/page",
	}
	for _, p := range uiPaths {
		t.Run("ui"+p, func(t *testing.T) {
			if isAPIPath(p) {
				t.Errorf("isAPIPath(%q) = true, want false — this would break the admin UI", p)
			}
		})
	}
}

// Seam test: isAPIPath being correct is worthless if serveDashboard never calls
// it. This drives the actual handler, so it fails if the predicate is right but
// the wiring is missing — the exact way a per-stage-only guard goes vacuous.
func TestServeDashboardReturnsJSON404ForAPIPaths(t *testing.T) {
	h := NewUIHandler(embed.FS{})

	t.Run("APIPathGetsJSON404NotHTML", func(t *testing.T) {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetRequestURI("/v1/models/claude-haiku-4-5")
		ctx.Request.Header.SetMethod("GET")

		h.serveDashboard(ctx)

		if got := ctx.Response.StatusCode(); got != fasthttp.StatusNotFound {
			t.Fatalf("status = %d, want 404 (200 + HTML is what broke SDK models.retrieve())", got)
		}

		body := string(ctx.Response.Body())
		if strings.Contains(strings.ToLower(body), "<!doctype") || strings.Contains(strings.ToLower(body), "<html") {
			t.Fatalf("body is HTML, want JSON: %.120s", body)
		}

		// Must be decodable by an SDK expecting an error envelope.
		var payload map[string]any
		if err := json.Unmarshal(ctx.Response.Body(), &payload); err != nil {
			t.Fatalf("body is not valid JSON (%v): %.120s", err, body)
		}
		if _, ok := payload["error"]; !ok {
			t.Fatalf("JSON body has no error field: %.120s", body)
		}
	})

	// The SPA fallback must survive for real UI routes. If this ever starts
	// returning 404, client-side routing in the dashboard is broken.
	t.Run("UIPathIsNotShortCircuited", func(t *testing.T) {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetRequestURI("/logs")
		ctx.Request.Header.SetMethod("GET")

		h.serveDashboard(ctx)

		// With an empty embed.FS there is no index.html to serve, so the exact
		// status is not the point — only that it did NOT take the API-404 branch,
		// which is identifiable by its JSON error envelope.
		var payload map[string]any
		if json.Unmarshal(ctx.Response.Body(), &payload) == nil {
			if _, isErrEnvelope := payload["error"]; isErrEnvelope {
				t.Fatal("UI route took the API-404 branch; SPA routing would be broken")
			}
		}
	})
}
