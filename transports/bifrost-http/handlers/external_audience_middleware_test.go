// RAYWARD FORK PATCH — tests for the external-audience header allowlist.
//
// These exist to make the patch loud if a rebase drops it. Three properties are
// load-bearing and each has a test that fails on its own:
//
//  1. suppression is opt-IN (no header => full headers), so a broken injection at
//     the load balancer cannot silently un-hide anything internally;
//  2. the allowlist covers streaming and error responses, not just the plain
//     200 path — those are where header-stripping patches historically miss;
//  3. the middleware is registered OUTERMOST, or it observes a header set that
//     the middlewares above it are still going to add to.

package handlers

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

// vendorLeakHeaders are real names measured on live responses (2026-07-30), not
// invented ones. Every one reached a caller before this patch.
var vendorLeakHeaders = map[string]string{
	"anthropic-organization-id":              "b86e8a2d-ad5a-4d86-8432-d852a7c2fb39",
	"anthropic-ratelimit-tokens-limit":       "12000000",
	"anthropic-ratelimit-requests-remaining": "9999",
	"request-id":                             "req_011CdYdGULDUYXM2Z3DrVKkJ",
	"cf-ray":                                 "a23608cf5daa4556-DFW",
	"cf-cache-status":                        "DYNAMIC",
	"x-bifrost-provider":                     "anthropic",
	"x-bifrost-resolved-model":               "claude-opus-4-6",
	"x-bifrost-trace-id":                     "0349d2741d32491f92990ee0da59c797",
	"x-request-id":                           "ae95d54d-3345-43fb-b9d5-07e532f9cf8e",
	"traceresponse":                          "00-c14c5799c38d58195636511c9b5dc52b-5453861f4244d895-01",
	"x-amzn-bedrock-content-type":            "application/json",
	"x-amz-request-id":                       "bifrost",
	"azureml-model-session":                  "d123",
	"apim-request-id":                        "0f2f0e2c-1111-2222-3333-444455556666",
}

// keptHeaders must survive: a client that loses these is broken, and none of
// them says anything about who we are or who we buy from.
var keptHeaders = map[string]string{
	"Content-Type":                 "application/json",
	"Cache-Control":                "no-cache",
	"Vary":                         "Origin",
	"Retry-After":                  "30",
	"Access-Control-Allow-Origin":  "https://example.test",
	"Access-Control-Allow-Methods": "GET, POST, PUT, DELETE, PATCH, OPTIONS, HEAD",
	"X-Frame-Options":              "DENY",
	"X-Content-Type-Options":       "nosniff",
	"Strict-Transport-Security":    "max-age=31536000; includeSubDomains",
	"Referrer-Policy":              "strict-origin-when-cross-origin",
	"Content-Security-Policy":      "frame-ancestors 'none'",
	"Permissions-Policy":           "camera=(), microphone=(), geolocation=()",
	"X-Robots-Tag":                 "none",
}

// seedResponse writes the full leaky header set a real upstream produces.
func seedResponse(h *fasthttp.ResponseHeader) {
	for name, value := range keptHeaders {
		h.Set(name, value)
	}
	for name, value := range vendorLeakHeaders {
		h.Set(name, value)
	}
	h.Set("Set-Cookie", "GAESA=Cp4BMDAxNTQ4; expires=Sat, 29-Aug-2026 17:20:31 GMT; path=/")
}

func headerNames(h *fasthttp.ResponseHeader) map[string]string {
	got := map[string]string{}
	h.VisitAll(func(key, value []byte) {
		got[strings.ToLower(string(key))] = string(value)
	})
	return got
}

func TestExternalAudienceStripsVendorHeaders(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)

	handler := ExternalAudienceHeaderMiddleware()(func(ctx *fasthttp.RequestCtx) {
		seedResponse(&ctx.Response.Header)
	})
	handler(ctx)

	got := headerNames(&ctx.Response.Header)
	for name := range vendorLeakHeaders {
		if value, present := got[name]; present {
			t.Errorf("header %q leaked to an external caller with value %q", name, value)
		}
	}
	if _, present := got["set-cookie"]; present {
		t.Error("set-cookie leaked to an external caller")
	}
}

func TestExternalAudienceKeepsStandardHeaders(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)

	handler := ExternalAudienceHeaderMiddleware()(func(ctx *fasthttp.RequestCtx) {
		seedResponse(&ctx.Response.Header)
	})
	handler(ctx)

	got := headerNames(&ctx.Response.Header)
	for name, want := range keptHeaders {
		value, present := got[strings.ToLower(name)]
		if !present {
			t.Errorf("header %q was dropped; an external caller needs it", name)
			continue
		}
		if value != want {
			t.Errorf("header %q = %q, want %q", name, value, want)
		}
	}
}

// The safety direction: absent marker means internal, which means untouched.
// If this ever inverts, a failure to inject the header at the load balancer
// stops being visible internally and starts being invisible externally.
func TestInternalAudienceIsUntouched(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		set   bool
	}{
		{name: "no audience header"},
		{name: "empty value", value: "", set: true},
		{name: "internal", value: "internal", set: true},
		{name: "not a token match", value: "externally", set: true},
		{name: "substring of a longer token", value: "not-external-either", set: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &fasthttp.RequestCtx{}
			if tc.set {
				ctx.Request.Header.Set(AudienceRequestHeader, tc.value)
			}

			handler := ExternalAudienceHeaderMiddleware()(func(ctx *fasthttp.RequestCtx) {
				seedResponse(&ctx.Response.Header)
			})
			handler(ctx)

			got := headerNames(&ctx.Response.Header)
			for name, want := range vendorLeakHeaders {
				if got[name] != want {
					t.Errorf("internal response lost %q (=%q); suppression must be opt-in", name, got[name])
				}
			}
		})
	}
}

// GCP custom_request_headers ADDS rather than replaces, so a client that sends
// its own audience header produces two entries or one comma-joined value. Both
// must still read as external — otherwise a caller could opt itself out.
func TestExternalAudienceDetection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		want   bool
	}{
		{name: "exact", values: []string{"external"}, want: true},
		{name: "uppercase", values: []string{"External"}, want: true},
		{name: "comma joined after a forged value", values: []string{"internal,external"}, want: true},
		{name: "comma space joined", values: []string{"internal, external"}, want: true},
		{name: "duplicate entries, forged first", values: []string{"internal", "external"}, want: true},
		{name: "duplicate entries, forged second", values: []string{"external", "internal"}, want: true},
		{name: "forged only", values: []string{"internal"}, want: false},
		{name: "prefix is not a token", values: []string{"external-preview"}, want: false},
		{name: "absent", values: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &fasthttp.RequestCtx{}
			for _, value := range tc.values {
				ctx.Request.Header.Add(AudienceRequestHeader, value)
			}
			if got := isExternalAudience(ctx); got != tc.want {
				t.Errorf("isExternalAudience(%q) = %v, want %v", tc.values, got, tc.want)
			}
		})
	}
}

func TestExternalAudienceStripsMultipleCookies(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)

	handler := ExternalAudienceHeaderMiddleware()(func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Add("Set-Cookie", "a=1; path=/")
		ctx.Response.Header.Add("Set-Cookie", "b=2; path=/")
		ctx.Response.Header.Add("Set-Cookie", "c=3; path=/")
	})
	handler(ctx)

	if raw := ctx.Response.Header.Peek("Set-Cookie"); len(raw) != 0 {
		t.Errorf("Set-Cookie survived: %q", raw)
	}
}

// An error response takes a different path through the router and touches none
// of the provider-header forwarding sites — but the upstream's headers are
// already on ctx.Response by then.
func TestExternalAudienceStripsOnErrorResponses(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)

	handler := ExternalAudienceHeaderMiddleware()(func(ctx *fasthttp.RequestCtx) {
		seedResponse(&ctx.Response.Header)
		ctx.SetStatusCode(fasthttp.StatusTooManyRequests)
		ctx.SetBodyString(`{"error":{"message":"rate limited"}}`)
	})
	handler(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", ctx.Response.StatusCode())
	}
	got := headerNames(&ctx.Response.Header)
	if _, present := got["anthropic-organization-id"]; present {
		t.Error("error response leaked anthropic-organization-id")
	}
	if got["retry-after"] != "30" {
		t.Errorf("error response lost retry-after (=%q); the caller needs it to back off", got["retry-after"])
	}
}

// A panic must not be the one path that ships a full header set outward.
func TestExternalAudienceStripsWhenHandlerPanics(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)

	handler := ExternalAudienceHeaderMiddleware()(func(ctx *fasthttp.RequestCtx) {
		seedResponse(&ctx.Response.Header)
		panic("upstream blew up")
	})

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected the panic to propagate to fasthttp's recovery")
			}
		}()
		handler(ctx)
	}()

	if _, present := headerNames(&ctx.Response.Header)["anthropic-organization-id"]; present {
		t.Error("a panicking handler leaked anthropic-organization-id")
	}
}

// The CORS middleware answers preflight without calling next. Being outermost
// means this still applies.
func TestExternalAudienceStripsOnEarlyReturn(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("OPTIONS")
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)

	inner := func(ctx *fasthttp.RequestCtx) {
		t.Error("preflight must not reach the inner handler")
	}
	preflight := func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
		return func(ctx *fasthttp.RequestCtx) {
			ctx.Response.Header.Set("Access-Control-Allow-Origin", "https://example.test")
			ctx.Response.Header.Set("x-bifrost-trace-id", "abc123")
			ctx.SetStatusCode(fasthttp.StatusOK)
		}
	}
	handler := ExternalAudienceHeaderMiddleware()(preflight(inner))
	handler(ctx)

	got := headerNames(&ctx.Response.Header)
	if _, present := got["x-bifrost-trace-id"]; present {
		t.Error("preflight response leaked x-bifrost-trace-id")
	}
	if got["access-control-allow-origin"] != "https://example.test" {
		t.Error("preflight lost its CORS header")
	}
}

// Streaming is the case a header patch most easily misses: the handler returns
// with only a body stream installed, and the header block is serialized later.
// This drives a real fasthttp server over an in-memory listener and reads the
// bytes off the wire, so it fails if the deferred rewrite lands too late.
func TestExternalAudienceStripsOnStreamingResponse(t *testing.T) {
	ln := fasthttputil.NewInmemoryListener()
	defer ln.Close()

	handler := ExternalAudienceHeaderMiddleware()(func(ctx *fasthttp.RequestCtx) {
		seedResponse(&ctx.Response.Header)
		ctx.SetContentType("text/event-stream")
		ctx.Response.SetBodyStreamWriter(func(w *bufio.Writer) {
			for _, chunk := range []string{"data: {\"a\":1}\n\n", "data: [DONE]\n\n"} {
				w.WriteString(chunk) //nolint:errcheck
				w.Flush()            //nolint:errcheck
			}
		})
	})

	server := &fasthttp.Server{Handler: handler}
	go server.Serve(ln)     //nolint:errcheck
	defer server.Shutdown() //nolint:errcheck

	conn, err := ln.Dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	request := "GET /v1/chat/completions HTTP/1.1\r\nHost: router.example\r\n" +
		AudienceRequestHeader + ": " + ExternalAudience + "\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}

	var response fasthttp.Response
	if err := response.Read(bufio.NewReader(conn)); err != nil {
		t.Fatalf("read: %v", err)
	}

	got := headerNames(&response.Header)
	for name := range vendorLeakHeaders {
		if _, present := got[name]; present {
			t.Errorf("streaming response leaked %q", name)
		}
	}
	if !strings.HasPrefix(got["content-type"], "text/event-stream") {
		t.Errorf("streaming content-type = %q, want text/event-stream", got["content-type"])
	}
	if body := string(response.Body()); !strings.Contains(body, "[DONE]") {
		t.Errorf("streaming body was mangled: %q", body)
	}
	var _ net.Conn = conn
}

// Composed against the real SecurityHeadersMiddleware rather than a stub, so the
// allowlist is checked against the actual header names and values that ship.
//
// Deliberately NOT named "is outermost": it does not prove that. Every
// middleware in today's chain sets its headers BEFORE calling next, so this
// passes with the ordering reversed. Ordering is asserted where it is actually
// decidable — against the source, in TestExternalAudienceMiddlewareIsRegistered
// — and its consequence is covered by TestExternalAudienceSeesPostHocHeaders.
func TestExternalAudienceStripsAcrossTheComposedChain(t *testing.T) {
	inner := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("x-bifrost-provider", "anthropic")
	}
	chain := ExternalAudienceHeaderMiddleware()(SecurityHeadersMiddleware()(inner))

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)
	ctx.Request.Header.Set("X-Forwarded-Proto", "https")
	chain(ctx)

	got := headerNames(&ctx.Response.Header)
	if _, present := got["x-bifrost-provider"]; present {
		t.Error("x-bifrost-provider survived the real chain")
	}
	if got["strict-transport-security"] == "" {
		t.Error("HSTS was dropped; the allowlist must keep the security headers")
	}
}

// Why outermost matters, stated as a behaviour rather than a convention: a
// middleware that touches headers AFTER next returns is invisible to anything
// nested inside it. None does today — which is exactly why the requirement
// would otherwise be untested until the day one does.
func TestExternalAudienceSeesPostHocHeaders(t *testing.T) {
	postHoc := func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
		return func(ctx *fasthttp.RequestCtx) {
			next(ctx)
			ctx.Response.Header.Set("x-bifrost-fallback-index", "2")
		}
	}
	inner := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "application/json")
	}

	outermost := ExternalAudienceHeaderMiddleware()(postHoc(inner))
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)
	outermost(ctx)
	if _, present := headerNames(&ctx.Response.Header)["x-bifrost-fallback-index"]; present {
		t.Error("a post-hoc header escaped the policy even from the outermost position")
	}

	// The same chain nested one level in demonstrates the failure it prevents.
	nested := postHoc(ExternalAudienceHeaderMiddleware()(inner))
	ctx = &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)
	nested(ctx)
	if _, present := headerNames(&ctx.Response.Header)["x-bifrost-fallback-index"]; !present {
		t.Error("nested placement was expected to miss the post-hoc header; " +
			"if it no longer does, this test has stopped demonstrating anything")
	}
}

// /v1/responses is a GET route that upgrades (wsresponses.go:79), and
// fasthttp/websocket writes the handshake onto ctx.Response.Header before
// hijacking (server_fasthttp.go:172-183) — so the deferred rewrite CAN reach a
// 101 and strip it into an unusable response. Found by an independent review.
func TestExternalAudienceKeepsTheWebSocketHandshake(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)

	handshake := map[string]string{
		"Upgrade":                  "websocket",
		"Sec-WebSocket-Accept":     "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=",
		"Sec-WebSocket-Protocol":   "openai-realtime",
		"Sec-WebSocket-Extensions": "permessage-deflate; server_no_context_takeover",
		"Sec-Websocket-Version":    "13",
	}
	handler := ExternalAudienceHeaderMiddleware()(func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusSwitchingProtocols)
		ctx.Response.Header.Set("Connection", "Upgrade")
		for name, value := range handshake {
			ctx.Response.Header.Set(name, value)
		}
		// A real upgrade also carries the routing identity, which must still go.
		ctx.Response.Header.Set("x-bifrost-provider", "openai")
	})
	handler(ctx)

	got := headerNames(&ctx.Response.Header)
	for name, want := range handshake {
		if got[strings.ToLower(name)] != want {
			t.Errorf("101 lost %q (=%q, want %q); standards-compliant clients reject the connection",
				name, got[strings.ToLower(name)], want)
		}
	}
	if got["connection"] != "Upgrade" {
		t.Errorf("101 lost Connection: Upgrade (=%q)", got["connection"])
	}
	if _, present := got["x-bifrost-provider"]; present {
		t.Error("the upgrade response still leaked x-bifrost-provider")
	}
}

// The headers an independent review asked for that are refused ON PURPOSE: each
// names us or a vendor, and every route emitting one is denied at the external
// load balancer. Pinned so re-adding one is a deliberate act.
func TestExternalAudienceStillRefusesTheIdentifyingProtocolHeaders(t *testing.T) {
	refused := map[string]string{
		"X-Goog-Upload-URL":    "https://generativelanguage.googleapis.com/upload/v1beta/files/abc",
		"X-Goog-Upload-Status": "active",
		"WWW-Authenticate":     `Bearer resource_metadata="https://router2.example/.well-known/oauth-protected-resource"`,
		"Location":             "https://router2.example/oauth/consent?state=abc",
	}
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)

	handler := ExternalAudienceHeaderMiddleware()(func(ctx *fasthttp.RequestCtx) {
		for name, value := range refused {
			ctx.Response.Header.Set(name, value)
		}
	})
	handler(ctx)

	got := headerNames(&ctx.Response.Header)
	for name := range refused {
		if value, present := got[strings.ToLower(name)]; present {
			t.Errorf("%q survived (=%q). It names Google, or carries our own hostname "+
				"in its value; the routes that emit it are denied at the LB "+
				"(/genai/upload/v1beta/files 403, /mcp 403, /v1/mcp 404). Allowing it "+
				"to unbreak that flow would be the leak.", name, value)
		}
	}
}

// A rebase that drops the registration line leaves every test above passing and
// the gateway wide open, so the wiring itself is asserted.
func TestExternalAudienceMiddlewareIsRegistered(t *testing.T) {
	path := filepath.Join("..", "server", "server.go")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	const want = "handlers.ExternalAudienceHeaderMiddleware()(handlers.SecurityHeadersMiddleware()("
	if !strings.Contains(string(source), want) {
		t.Fatalf("%s no longer wraps the handler chain in ExternalAudienceHeaderMiddleware "+
			"as the outermost middleware; external callers get every response header. "+
			"Expected to find %q", path, want)
	}
}

// The allowlist is the whole mechanism, so its contents are pinned. Adding a
// name here should be a deliberate act with a reason, not a merge artifact.
func TestExternalAllowlistContents(t *testing.T) {
	want := []string{
		"access-control-allow-credentials", "access-control-allow-headers",
		"access-control-allow-methods", "access-control-allow-origin",
		"access-control-expose-headers", "access-control-max-age",
		"allow", "cache-control", "connection", "content-encoding",
		"content-length", "content-security-policy", "content-type", "date",
		"permissions-policy", "referrer-policy", "retry-after",
		"sec-websocket-accept", "sec-websocket-extensions",
		"sec-websocket-protocol", "sec-websocket-version", "server",
		"strict-transport-security", "transfer-encoding", "upgrade", "vary",
		"x-content-type-options", "x-frame-options", "x-robots-tag",
	}
	if len(externalAllowedHeaders) != len(want) {
		t.Errorf("allowlist has %d entries, want %d", len(externalAllowedHeaders), len(want))
	}
	for _, name := range want {
		if _, ok := externalAllowedHeaders[name]; !ok {
			t.Errorf("allowlist is missing %q", name)
		}
	}
	for name := range externalAllowedHeaders {
		if name != strings.ToLower(name) {
			t.Errorf("allowlist entry %q must be lowercase; lookups normalize", name)
		}
	}
}

// Guards the neutral-name rule in AGENTS.md: the marker is checked into a fork
// whose source is readable, so a branded value would put the brand in the one
// place this whole external domain exists to keep it out of.
func TestAudienceHeaderNameIsNeutral(t *testing.T) {
	for _, value := range []string{AudienceRequestHeader, ExternalAudience} {
		if strings.Contains(strings.ToLower(value), "rayward") {
			t.Errorf("%q must not contain the brand name", value)
		}
	}
	var _ schemas.BifrostHTTPMiddleware = ExternalAudienceHeaderMiddleware()
}
