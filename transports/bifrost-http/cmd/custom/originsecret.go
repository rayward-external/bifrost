package main

import (
	"crypto/subtle"

	"github.com/valyala/fasthttp"
)

const originSecretHeader = "x-rayward-cf-secret"

// healthProbePaths are exempt from origin-secret enforcement. Cloud Run
// liveness/readiness probes hit these endpoints directly on the replica (not
// through Cloudflare), so they never carry the origin-secret header — gating
// them 403s every probe and the revision never goes healthy.
// These endpoints expose no sensitive data (only liveness/dependency status).
var healthProbePaths = map[string]bool{
	"/health": true,
	"/livez":  true,
	"/readyz": true,
}

// requireOriginSecret enforces the Cloudflare origin trust boundary: it rejects
// any request whose x-rayward-cf-secret header (injected by the Cloudflare
// Transform Rule) does not match the configured secret. An empty secret
// disables enforcement, which is the expected state for local/dev runs.
//
// Health-probe endpoints are exempt — see healthProbePaths.
func requireOriginSecret(secret string) func(fasthttp.RequestHandler) fasthttp.RequestHandler {
	secretBytes := []byte(secret)
	return func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
		if secret == "" {
			return next
		}
		return func(ctx *fasthttp.RequestCtx) {
			if healthProbePaths[string(ctx.Path())] {
				next(ctx)
				return
			}
			provided := ctx.Request.Header.Peek(originSecretHeader)
			if subtle.ConstantTimeCompare(provided, secretBytes) != 1 {
				ctx.Error("forbidden", fasthttp.StatusForbidden)
				return
			}
			next(ctx)
		}
	}
}
