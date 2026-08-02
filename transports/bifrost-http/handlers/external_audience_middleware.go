// RAYWARD FORK PATCH — minimal response-header exposure for external callers.
//
// # WHY THIS EXISTS
//
// An external party gets an API key and nothing else: they must not be able to
// learn what gateway software we run, that we proxy at all, who we proxy to, or
// what capacity we bought.
//
// This cannot be solved at the load balancer. GCP url-map
// `responseHeadersToRemove` caps at 50 names, and a single Bifrost response was
// measured emitting header sets that total 77 distinct names across providers
// (2026-07-30, live prod). Whichever 50 are listed, the rest reappear on a leg
// the selection did not anticipate.
//
// # WHY AN ALLOWLIST, WHERE THE LITELLM PATCH USES PREFIXES
//
// The companion patch in the LiteLLM fork suppresses by prefix, because LiteLLM
// re-publishes every upstream header under a `llm_provider-` prefix and its own
// under `x-litellm-`, so the leak has a shape. Bifrost has no such shape:
// applyBifrostResponseHeaders() in integrations/utils.go copies
// `extra.ProviderResponseHeaders` onto the response VERBATIM and UN-PREFIXED, so
// the emitted name set is whatever the upstream vendor chose to send. A denylist
// over that is a list of the names someone already happened to observe.
//
// Measured on the Anthropic leg, verbatim forwarding put all of these on the
// wire: `anthropic-organization-id`, twelve `anthropic-ratelimit-*` counters,
// Anthropic's own `request-id: req_011…`, `cf-ray`, `cf-cache-status`. On the
// Azure and Vertex legs the names differ again. So the policy is inverted: keep
// a fixed set of standard HTTP/CORS/security headers, drop everything else.
// A new provider, or a provider that starts sending a new header, is covered on
// the day it ships rather than on the day someone notices.
//
// # IF THIS PATCH IS DROPPED BY A REBASE
//
// External callers immediately see the upstream vendor's own headers verbatim —
// which names the vendor, our organization id there, and our purchased quota —
// plus `x-bifrost-*`, which names the gateway software.
// external_audience_middleware_test.go fails if any part of this goes missing,
// including the registration in server/server.go.
//
// # WHY A TRANSPORT MIDDLEWARE AND NOT A PATCH AT THE FORWARDING SITES
//
// There is no chokepoint. Provider headers are copied onto the response from
// more than a dozen call sites (applyBifrostResponseHeaders,
// forwardProviderHeaders, tryStreamLargeResponse, and the per-request-type
// branches in handlers/inference.go), and errors take paths that touch none of
// them. The outermost middleware sees the finished `ctx.Response.Header` for
// every route exactly once, so it is both the smallest and the only complete
// seam. One file plus one registration line to re-apply.
//
// This works for streaming too. handleStreaming() installs an SSE body stream
// with ctx.Response.SetBodyStream() and returns; fasthttp serializes the header
// block afterwards, so headers are still mutable when this middleware regains
// control — the same "runs once, before any body byte" property the ASGI
// http.response.start hook gives the LiteLLM patch.
//
// # SAFE FAILURE DIRECTION
//
// Absence of the audience header means INTERNAL, i.e. full headers. Only an
// explicit `external` value turns suppression on. If it were inverted, any
// failure to inject the header would silently un-hide everything on the external
// path. With this direction the worst a client can do by forging the header is
// suppress its own response headers, which harms nobody.

package handlers

import (
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// AudienceRequestHeader is injected by the EXTERNAL load balancer's backend
// service. Deliberately vendor-neutral — AGENTS.md requires neutral header names
// in fork patches, so it must never contain the brand.
//
// Defined in lib and re-exported here. The companion BODY policy has to live in
// lib (`handlers` imports `integrations`, so `integrations` cannot import
// `handlers`, and the SSE wrap has to happen inside `integrations`), and one
// marker string across two policies must have exactly one definition — a
// second copy that drifts would leave the header policy on and the body policy
// silently off.
const AudienceRequestHeader = lib.AudienceRequestHeader

// ExternalAudience is the only value that turns suppression on.
const ExternalAudience = lib.ExternalAudience

// externalAllowedHeaders is the complete set of response headers an external
// caller receives. Everything absent from this map is deleted.
//
// Membership test: does a standard HTTP client need it, and does it say nothing
// about who we are or who we buy from? Names are lowercase; lookups normalize.
var externalAllowedHeaders = map[string]struct{}{
	// Message framing. fasthttp owns most of these, but they surface through
	// VisitAll and deleting them would corrupt the response.
	"content-type":      {},
	"content-length":    {},
	"content-encoding":  {},
	"transfer-encoding": {},
	"connection":        {},
	"date":              {},
	"vary":              {},
	"cache-control":     {},
	"allow":             {},

	// Backpressure. Unlike the vendor `*-ratelimit-*` families this carries no
	// quota figure — just "come back later" — so it stays.
	"retry-after": {},

	// CORS. Browser clients break without these, and Bifrost's values are
	// generic: the origin echoed back, a fixed method list, and a fixed list of
	// SDK REQUEST header names. Bifrost sets no
	// access-control-expose-headers, so nothing here enumerates our own headers.
	"access-control-allow-origin":      {},
	"access-control-allow-methods":     {},
	"access-control-allow-headers":     {},
	"access-control-allow-credentials": {},
	"access-control-expose-headers":    {},
	"access-control-max-age":           {},

	// WebSocket handshake. `/v1/responses` is registered as a GET route that
	// upgrades (wsresponses.go), and fasthttp/websocket sets these on
	// ctx.Response.Header BEFORE calling ctx.Hijack (server_fasthttp.go:172-183),
	// so they are inside this policy's reach and a 101 without them is rejected
	// by any standards-compliant client. Pure protocol: `Sec-WebSocket-Accept` is
	// a hash of the client's own nonce and `Sec-WebSocket-Protocol` echoes the
	// subprotocol the client asked for, so none of them says anything about us.
	// `Connection: Upgrade` rides the framing entry above.
	"upgrade":                  {},
	"sec-websocket-accept":     {},
	"sec-websocket-protocol":   {},
	"sec-websocket-extensions": {},
	"sec-websocket-version":    {},

	// SecurityHeadersMiddleware. Generic hardening, identical on any host.
	"x-frame-options":           {},
	"x-content-type-options":    {},
	"referrer-policy":           {},
	"content-security-policy":   {},
	"permissions-policy":        {},
	"strict-transport-security": {},
	"x-robots-tag":              {},

	// NOT a real exception — an honest one. fasthttp fills in a default
	// `Server: fasthttp` at serialization time whenever the header is unset
	// (server.getServerName), so deleting it here changes the wire not at all.
	// Removing `server` is the load balancer's job; listing it as allowed keeps
	// this map an accurate description of what actually goes out.
	"server": {},
}

// Deliberately NOT allowed, because each has been mistaken for harmless:
//
//   - x-request-id — Bifrost mints a UUID for it, but the provider-header copy
//     runs afterwards with Set(), so an upstream that sends its own x-request-id
//     (OpenAI does) overwrites ours and we would forward THEIR id.
//   - x-bifrost-* / x-bifrost-trace-id — names the gateway software.
//   - traceresponse — our W3C trace id.
//   - set-cookie — the value has been observed naming the upstream domain.
//   - x-amzn-bedrock-content-type, x-amz-request-id — name the vendor. No
//     Bedrock-shaped route is published externally, but the allowlist means we
//     do not have to keep that true to stay safe.
//
// Three more were raised by an independent review (2026-07-31) as breaking
// supported protocols, and are excluded ON PURPOSE. Each names us or a vendor,
// and every route that emits one is refused by the external load balancer —
// verified live, not assumed: /genai/upload/v1beta/files -> 403, /mcp -> 403,
// /v1/mcp -> 404.
//
//   - x-goog-upload-url / x-goog-upload-status — the GenAI resumable-upload
//     control pair. The header NAME says Google, which is the disclosure itself,
//     so allowlisting them to keep that flow working would defeat the point.
//   - www-authenticate — MCP OAuth discovery sets it to
//     resource_metadata="https://<our host>/…", i.e. our hostname in the value.
//   - location — the OAuth consent/callback redirect carries our host the same
//     way. Nothing on the published surface returns a 3xx.
//
// If any of those routes is ever published externally, the fix is to allow the
// header AND neutralize what it carries — not to allow it alone.

// isExternalAudience reports whether the audience header explicitly says
// "external".
//
// GCP custom_request_headers ADDS a header rather than replacing it, so when a
// client also sends one the value can arrive duplicated (two entries) or
// comma-joined into one. Every occurrence is checked, and every token within it.
func isExternalAudience(ctx *fasthttp.RequestCtx) bool {
	// Delegates so the header and body policies can never disagree about who is
	// external. The detection tests below still exercise this name.
	return lib.IsExternalAudience(ctx)
}

// applyExternalHeaderPolicy deletes every response header outside the allowlist.
//
// Two passes on purpose: fasthttp documents mutation during VisitAll as
// undefined behavior, so the names are collected first and deleted after.
func applyExternalHeaderPolicy(h *fasthttp.ResponseHeader) {
	var doomed []string
	h.VisitAll(func(key, _ []byte) {
		name := string(key)
		if _, ok := externalAllowedHeaders[strings.ToLower(name)]; ok {
			return
		}
		doomed = append(doomed, name)
	})
	for _, name := range doomed {
		// Del handles fasthttp's special-cased fields, including Set-Cookie,
		// which it clears wholesale rather than one cookie at a time.
		h.Del(name)
	}
}

// ExternalAudienceHeaderMiddleware applies the external-audience response
// policies for requests that arrived on an external frontend: headers are
// stripped to the allowlist above, and `extra_fields` is removed from BUFFERED
// bodies by lib.ApplyExternalBodyPolicy.
//
// STREAMED bodies are NOT covered here and cannot be: fasthttp's SetBodyStream
// closes the stream it replaces, so re-wrapping one from a middleware kills the
// producer and delivers an empty body. Streams are wrapped at creation, in
// integrations/router.go, via lib.WrapSSEForExternalAudience. Full reasoning in
// lib/external_audience_body.go.
//
// The name is historical — it predates the body policy and is kept because
// renaming would churn its registration and nine test call sites for no
// behavioural gain, and this fork's first job is surviving upstream rebases.
//
// Must be registered OUTERMOST in server.go so it observes the final header set
// produced by every inner middleware, CORS and the security headers included.
func ExternalAudienceHeaderMiddleware() schemas.BifrostHTTPMiddleware {
	return func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
		return func(ctx *fasthttp.RequestCtx) {
			if !isExternalAudience(ctx) {
				next(ctx)
				return
			}
			// NO EAGER CaptureRequestedModel HERE, and the omission is load-bearing.
			//
			// This middleware is registered OUTERMOST, so it runs BEFORE
			// RequestDecompressionMiddleware. On a `Content-Encoding: gzip`
			// request the body is still compressed bytes at this point, so an
			// eager capture finds no `"model"` and caches the EMPTY STRING —
			// which lib.RequestedModel then returned from cache for the rest of
			// the request, defeating lazy resolution entirely.
			//
			// Measured live on router2 AFTER the first fix deployed: an ordinary
			// gzipped /v1/messages request still returned
			// `global.anthropic.claude-haiku-4-5-20251001-v1:0` while every
			// uncompressed path was clean.
			//
			// lib.RequestedModel resolves on demand instead, so both body
			// policies observe the body AFTER the inner middleware decompressed
			// it. Do not "optimise" by hoisting a capture back up here.
			//
			// Deferred, not sequential: a panic below must not be the one path
			// that ships a full header set to an external caller. Runs after the
			// handler has finished writing headers — including the CORS preflight
			// branch, which returns without calling next, and the streaming
			// branch, which returns with the body stream installed but the header
			// block not yet serialized.
			//
			// LIFO: the body policy runs FIRST, so the header policy observes the
			// Content-Length that SetBody recomputed rather than the pre-scrub one.
			defer applyExternalHeaderPolicy(&ctx.Response.Header)
			defer lib.ApplyExternalBodyPolicy(ctx)
			next(ctx)
		}
	}
}
