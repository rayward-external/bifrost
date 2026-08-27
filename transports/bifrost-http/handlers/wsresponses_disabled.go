package handlers

import (
	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/transports/bifrost-http/integrations"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// ---------------------------------------------------------------------------
// FORK PATCH (rayward-external/bifrost) — Realtime API WebSocket mode is
// disabled on this deployment. See rayward-internal/llm-gateway-infra#646 and
// .github/fork-patches.txt.
//
// (Responses API WebSocket mode was disabled here too, by PR #289
// (llm-gateway-infra#645), because every non-Vertex leg bypassed governance
// routing and dialed the literal "openai" provider straight off the bare
// model string. That was fixed by backporting maximhq/bifrost#5339 into
// wsresponses.go (llm-gateway-infra#650) and the Responses half of this file
// was removed 2026-08-27 — see (*WSResponsesHandler).RegisterRoutes, which is
// back to the vanilla upstream body. Only Realtime remains disabled below.)
//
// Why the routes stay REGISTERED instead of simply not being registered:
// dropping the registration does NOT produce a clean rejection. Unmatched GETs
// fall through to the dashboard catch-all installed by UIHandler.RegisterRoutes
// ("GET /{filepath:*}"), and isAPIPath in ui.go only classifies a path as API
// when segment 0 is "v1"/"api" or segment 1 is "v1". So /openai/realtime would
// be answered by the SPA handler with a keep-alive 200 text/html — and on the
// production path that 200 never reaches the client. Measured against the
// live gateway on 2026-08-26 (3/3), an HTTP/1.1 GET carrying WebSocket
// upgrade headers to any catch-all path comes back rewritten as a bogus
// upgrade and then dropped:
//
//	$ curl -i --http1.1 -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
//	    -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
//	    https://<gateway>/openai/nope
//	HTTP/1.1 101 Switching Protocols       <- no sec-websocket-accept
//	curl: (52) Empty reply from server
//
// Bifrost is NOT what writes that 101: fasthttp v1.71.0 + router v1.5.4 answer
// the identical request with a plain "200 OK" locally. The rewrite happens in
// the Google front end between Bifrost and the client (Cloud Run ingress /
// global LB). Every response observed to pass through it intact carries
// "Connection: close" (Router.NotFound's JSON 404 on /v1/*, and LiteLLM's 403
// on its own Cloud Run service); the keep-alive 200 is the one rewritten.
// Which of the two properties triggers the rewrite cannot be separated from
// outside, so the refusing handler mirrors the shape known to pass: a JSON
// 404 with an explicit "Connection: close".
//
// That accepted-then-died shape is exactly the symptom #646 asks us to remove,
// so an explicit handler is required. Keeping the registration also preserves
// the inference middleware chain on these paths.
//
// REMOVAL CONDITION: delete this file and restore the upstream body of
// (*WSRealtimeHandler).RegisterRoutes once a realtime-capable model is served
// on this deployment, with integration coverage proving the handshake holds.
// ---------------------------------------------------------------------------

// realtimeWSDisabledMessage is returned for every Realtime WebSocket path.
// There is no HTTP alternative for Realtime, so it says what is actually true:
// no realtime-capable model is served here (see llm-gateway-infra#646).
const realtimeWSDisabledMessage = "Realtime API WebSocket mode is not supported on this gateway: no realtime-capable model is served."

// realtimeWSDisabledPaths returns every path that (*WSRealtimeHandler).RegisterRoutes
// would otherwise bind to the WebSocket upgrader. Derived from the same source
// as upstream's registration so the two cannot drift apart.
func realtimeWSDisabledPaths() []string {
	return append([]string{"/v1/realtime"}, integrations.OpenAIRealtimePaths("/openai")...)
}

// registerDisabledRealtimeWSRoutes binds every Realtime WebSocket path to a
// handler that refuses the request outright, so no such request can reach the
// dashboard catch-all.
func registerDisabledRealtimeWSRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	registerRefusingWSRoutes(r, realtimeWSDisabledPaths(), realtimeWSDisabledMessage, middlewares...)
}

// registerRefusingWSRoutes binds paths to a handler that refuses the request
// outright, so no such request can reach the dashboard catch-all.
func registerRefusingWSRoutes(r *router.Router, paths []string, message string, middlewares ...schemas.BifrostHTTPMiddleware) {
	handler := lib.ChainMiddlewares(func(ctx *fasthttp.RequestCtx) {
		refuseWSUpgrade(ctx, message)
	}, middlewares...)
	for _, path := range paths {
		r.GET(path, handler)
	}
}

// refuseWSUpgrade writes the refusal. The explicit "Connection: close" is
// load-bearing, not cosmetic: measured against the live gateway, a keep-alive
// non-101 reply to an upgrade request is rewritten by the Google front end into
// a bogus "101 Switching Protocols" with no Sec-WebSocket-Accept and then
// dropped, while every reply carrying "Connection: close" passes through intact.
func refuseWSUpgrade(ctx *fasthttp.RequestCtx, message string) {
	ctx.Response.Header.SetCanonical([]byte("Connection"), []byte("close"))
	SendError(ctx, fasthttp.StatusNotFound, message)
}
