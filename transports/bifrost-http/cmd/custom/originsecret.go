package main

import (
	"crypto/subtle"

	"github.com/valyala/fasthttp"
)

const originSecretHeader = "x-rayward-cf-secret"

// requireOriginSecret enforces the Cloudflare origin trust boundary: it rejects
// any request whose x-rayward-cf-secret header (injected by the Cloudflare
// Transform Rule) does not match the configured secret. An empty secret
// disables enforcement, which is the expected state for local/dev runs.
func requireOriginSecret(secret string) func(fasthttp.RequestHandler) fasthttp.RequestHandler {
	secretBytes := []byte(secret)
	return func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
		if secret == "" {
			return next
		}
		return func(ctx *fasthttp.RequestCtx) {
			provided := ctx.Request.Header.Peek(originSecretHeader)
			if subtle.ConstantTimeCompare(provided, secretBytes) != 1 {
				ctx.Error("forbidden", fasthttp.StatusForbidden)
				return
			}
			next(ctx)
		}
	}
}
