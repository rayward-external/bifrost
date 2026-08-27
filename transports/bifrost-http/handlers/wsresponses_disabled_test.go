package handlers

import (
	"strings"
	"testing"

	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/transports/bifrost-http/integrations"
	"github.com/valyala/fasthttp"
)

// FORK PATCH TEST (rayward-internal/llm-gateway-infra#645). See
// wsresponses_disabled.go. This is the tripwire: an upstream sync that restores
// the vanilla (*WSResponsesHandler).RegisterRoutes body, or that drops
// wsresponses_disabled.go's registration, turns this red instead of silently
// re-enabling a broken transport.

// spaShellBody is what the test's stand-in for the dashboard catch-all returns.
// Reaching it is the failure this patch exists to prevent: on the live gateway
// the catch-all's keep-alive 200 is rewritten by the Google front end into a
// "101 Switching Protocols" with no Sec-WebSocket-Accept, then dropped (see
// wsresponses_disabled.go).
const spaShellBody = "SPA-SHELL"

// newResponsesWSTestRouter builds a router with the Responses WebSocket paths
// registered by register, followed by a catch-all standing in for
// UIHandler.RegisterRoutes' "GET /{filepath:*}" — the same order the server uses.
func newResponsesWSTestRouter(register func(*router.Router)) *router.Router {
	r := router.New()
	register(r)
	r.GET("/{filepath:*}", func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetContentType("text/html; charset=utf-8")
		ctx.SetBodyString(spaShellBody)
	})
	return r
}

// serveUpgradeGET drives one GET request carrying a full set of WebSocket
// upgrade headers through the router and returns the response.
func serveUpgradeGET(r *router.Router, path string) *fasthttp.Response {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fasthttp.MethodGet)
	ctx.Request.SetRequestURI(path)
	ctx.Request.Header.Set("Connection", "Upgrade")
	ctx.Request.Header.Set("Upgrade", "websocket")
	ctx.Request.Header.Set("Sec-WebSocket-Version", "13")
	ctx.Request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	r.Handler(ctx)
	return &ctx.Response
}

// assertRefused checks that a response is a definitive non-WebSocket rejection
// and not the SPA shell.
func assertRefused(t *testing.T, path string, resp *fasthttp.Response, wantMessage string) {
	t.Helper()

	body := string(resp.Body())
	if strings.Contains(body, spaShellBody) {
		t.Fatalf("%s fell through to the dashboard catch-all; it must be refused explicitly", path)
	}
	if got := resp.StatusCode(); got == fasthttp.StatusSwitchingProtocols {
		t.Fatalf("%s: upgrade was accepted (101); the WebSocket transport must not open", path)
	}
	if got := resp.StatusCode(); got != fasthttp.StatusNotFound {
		t.Fatalf("%s: status = %d, want %d (body: %q)", path, got, fasthttp.StatusNotFound, body)
	}
	if ct := string(resp.Header.ContentType()); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("%s: content-type = %q, want application/json", path, ct)
	}
	if !strings.Contains(body, wantMessage) {
		t.Fatalf("%s: body = %q, want %q", path, body, wantMessage)
	}
}

// TestResponsesWebSocketRoutesAreDisabled is the behavioral guard: every path
// upstream would bind to the upgrader must instead answer a definitive JSON
// rejection, whether the routes are registered directly or through
// (*WSResponsesHandler).RegisterRoutes.
func TestResponsesWebSocketRoutesAreDisabled(t *testing.T) {
	registrars := map[string]func(*router.Router){
		"registerDisabledResponsesWSRoutes": func(r *router.Router) {
			registerDisabledResponsesWSRoutes(r)
		},
		// The delegation itself. A sync that restores upstream's RegisterRoutes
		// body binds h.handleUpgrade here and this case stops returning our 404.
		"WSResponsesHandler.RegisterRoutes": func(r *router.Router) {
			(&WSResponsesHandler{}).RegisterRoutes(r)
		},
	}

	for name, register := range registrars {
		t.Run(name, func(t *testing.T) {
			r := newResponsesWSTestRouter(register)
			for _, path := range responsesWSDisabledPaths() {
				assertRefused(t, path, serveUpgradeGET(r, path), "Responses API WebSocket mode is not supported")
			}
		})
	}
}

// TestResponsesWSDisabledPathsCoverUpstreamRegistration pins the path list to
// upstream's own source. If a sync adds a Responses WebSocket path to
// OpenAIWSResponsesPaths, this fails rather than leaving the new path to fall
// through to the catch-all.
func TestResponsesWSDisabledPathsCoverUpstreamRegistration(t *testing.T) {
	covered := make(map[string]bool)
	for _, path := range responsesWSDisabledPaths() {
		covered[path] = true
	}

	if !covered["/v1/responses"] {
		t.Fatal("/v1/responses is not covered by responsesWSDisabledPaths")
	}
	for _, path := range integrations.OpenAIWSResponsesPaths("/openai") {
		if !covered[path] {
			t.Fatalf("OpenAI integration path %s is not covered by responsesWSDisabledPaths", path)
		}
	}
}

// TestRealtimeWebSocketRoutesAreDisabled is the realtime tripwire
// (rayward-internal/llm-gateway-infra#646). No realtime-capable model is served
// on this deployment, so upstream's handler upgraded and then died: measured
// live, 12 of 14 upgrades came back 502 websocket_handshake_failed while the
// origin logged 101, and the 2 that reached the client as 101 were already dead.
func TestRealtimeWebSocketRoutesAreDisabled(t *testing.T) {
	registrars := map[string]func(*router.Router){
		"registerDisabledRealtimeWSRoutes": func(r *router.Router) {
			registerDisabledRealtimeWSRoutes(r)
		},
		// The delegation itself. A sync that restores upstream's RegisterRoutes
		// body binds h.handleUpgrade here and this case stops returning our 404.
		"WSRealtimeHandler.RegisterRoutes": func(r *router.Router) {
			(&WSRealtimeHandler{}).RegisterRoutes(r)
		},
	}

	for name, register := range registrars {
		t.Run(name, func(t *testing.T) {
			r := newResponsesWSTestRouter(register)
			for _, path := range realtimeWSDisabledPaths() {
				assertRefused(t, path, serveUpgradeGET(r, path), "Realtime API WebSocket mode is not supported")
			}
		})
	}
}

// TestRealtimeWSDisabledPathsCoverUpstreamRegistration pins the realtime path
// list to upstream's own source, so a sync that adds a path cannot leave it
// falling through to the dashboard catch-all.
func TestRealtimeWSDisabledPathsCoverUpstreamRegistration(t *testing.T) {
	covered := make(map[string]bool)
	for _, path := range realtimeWSDisabledPaths() {
		covered[path] = true
	}

	if !covered["/v1/realtime"] {
		t.Fatal("/v1/realtime is not covered by realtimeWSDisabledPaths")
	}
	for _, path := range integrations.OpenAIRealtimePaths("/openai") {
		if !covered[path] {
			t.Fatalf("OpenAI realtime path %s is not covered by realtimeWSDisabledPaths", path)
		}
	}
}

// TestDisabledWSPathSetsDoNotOverlap keeps the two refusal messages meaningful:
// a caller hitting a responses path must not be told about realtime models.
func TestDisabledWSPathSetsDoNotOverlap(t *testing.T) {
	responses := make(map[string]bool)
	for _, path := range responsesWSDisabledPaths() {
		responses[path] = true
	}
	for _, path := range realtimeWSDisabledPaths() {
		if responses[path] {
			t.Fatalf("%s appears in both the responses and realtime disabled path sets", path)
		}
	}
}
