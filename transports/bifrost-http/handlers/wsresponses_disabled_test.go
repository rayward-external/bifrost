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
func assertRefused(t *testing.T, path string, resp *fasthttp.Response) {
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
	if !strings.Contains(body, "WebSocket mode is not supported") {
		t.Fatalf("%s: body = %q, want the disabled-mode message", path, body)
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
				assertRefused(t, path, serveUpgradeGET(r, path))
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

// TestResponsesWebSocketDisabledLeavesRealtimeAlone guards the blast radius:
// #645 is about /v1/responses only, and the Realtime WebSocket surface must
// keep upgrading.
func TestResponsesWebSocketDisabledLeavesRealtimeAlone(t *testing.T) {
	for _, path := range responsesWSDisabledPaths() {
		if strings.Contains(path, "realtime") {
			t.Fatalf("responsesWSDisabledPaths must not disable a realtime path, got %s", path)
		}
	}
	if !isInferenceWSEndpoint("/v1/realtime") {
		t.Fatal("/v1/realtime must remain an inference WebSocket endpoint")
	}
}
