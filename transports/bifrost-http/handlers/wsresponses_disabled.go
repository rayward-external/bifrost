package handlers

import (
	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/transports/bifrost-http/integrations"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// ---------------------------------------------------------------------------
// FORK PATCH (rayward-external/bifrost) — Responses API WebSocket mode is
// disabled on this deployment. See rayward-internal/llm-gateway-infra#645 and
// .github/fork-patches.txt.
//
// Why the routes stay REGISTERED instead of simply not being registered:
// dropping the registration does NOT produce a clean rejection. Unmatched GETs
// fall through to the dashboard catch-all installed by UIHandler.RegisterRoutes
// ("GET /{filepath:*}"), and isAPIPath in ui.go only classifies a path as API
// when segment 0 is "v1"/"api" or segment 1 is "v1". So /openai/responses and
// /openai/openai/responses would be served the SPA shell — and measured against
// the live gateway on 2026-08-26, an HTTP/1.1 GET carrying WebSocket upgrade
// headers to any such catch-all path answers "101 Switching Protocols" with no
// Sec-WebSocket-Accept and then closes the connection:
//
//	$ curl -i --http1.1 -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
//	    -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
//	    https://<gateway>/openai/nope
//	HTTP/1.1 101 Switching Protocols       <- no sec-websocket-accept
//	curl: (52) Empty reply from server
//
// That accepted-then-died shape is exactly the symptom #645 asks us to remove,
// so an explicit handler is required. Keeping the registration also preserves
// the inference middleware chain on these paths.
//
// REMOVAL CONDITION: delete this file and restore the upstream body of
// (*WSResponsesHandler).RegisterRoutes once Responses-over-WebSocket works for
// the Azure, Bedrock and Fireworks legs, with integration coverage per family.
// ---------------------------------------------------------------------------

// responsesWSDisabledMessage is returned for every Responses WebSocket path.
// It names the supported alternative so a client does not have to guess.
const responsesWSDisabledMessage = "Responses API WebSocket mode is not supported on this gateway. Use HTTP POST on the same path instead."

// responsesWSDisabledPaths returns every path that (*WSResponsesHandler).RegisterRoutes
// would otherwise bind to the WebSocket upgrader. Derived from the same source
// as upstream's registration so the two cannot drift apart.
func responsesWSDisabledPaths() []string {
	return append([]string{"/v1/responses"}, integrations.OpenAIWSResponsesPaths("/openai")...)
}

// registerDisabledResponsesWSRoutes binds every Responses WebSocket path to a
// handler that refuses the request outright, so no such request can reach the
// dashboard catch-all.
func registerDisabledResponsesWSRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	handler := lib.ChainMiddlewares(handleResponsesWSDisabled, middlewares...)
	for _, path := range responsesWSDisabledPaths() {
		r.GET(path, handler)
	}
}

// handleResponsesWSDisabled refuses a Responses WebSocket request with a
// definitive JSON error. 404 matches what Router.NotFound already returns for
// an unmatched API GET, so the whole API surface answers consistently; the
// response is deliberately NOT a 101, so a client fails the handshake and can
// select HTTPS rather than seeing a session open and then die.
func handleResponsesWSDisabled(ctx *fasthttp.RequestCtx) {
	ctx.Response.Header.SetCanonical([]byte("Connection"), []byte("close"))
	SendError(ctx, fasthttp.StatusNotFound, responsesWSDisabledMessage)
}
