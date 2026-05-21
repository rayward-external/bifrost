package main

import (
	"testing"

	"github.com/valyala/fasthttp"
)

func TestOriginSecret_MissingHeaderRejected(t *testing.T) {
	nextCalled := false
	next := func(ctx *fasthttp.RequestCtx) {
		nextCalled = true
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := requireOriginSecret("expected-secret")(next)

	ctx := &fasthttp.RequestCtx{}
	handler(ctx)

	if nextCalled {
		t.Fatal("next handler must not be called when the origin-secret header is missing")
	}
	if ctx.Response.StatusCode() != fasthttp.StatusForbidden {
		t.Fatalf("want status 403, got %d", ctx.Response.StatusCode())
	}
}

func TestOriginSecret_CorrectHeaderPasses(t *testing.T) {
	nextCalled := false
	next := func(ctx *fasthttp.RequestCtx) {
		nextCalled = true
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := requireOriginSecret("expected-secret")(next)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("x-rayward-cf-secret", "expected-secret")
	handler(ctx)

	if !nextCalled {
		t.Fatal("next handler must be called when the origin-secret header matches")
	}
	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("want status 200, got %d", ctx.Response.StatusCode())
	}
}

func TestOriginSecret_WrongHeaderRejected(t *testing.T) {
	nextCalled := false
	next := func(ctx *fasthttp.RequestCtx) {
		nextCalled = true
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := requireOriginSecret("expected-secret")(next)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("x-rayward-cf-secret", "wrong-secret")
	handler(ctx)

	if nextCalled {
		t.Fatal("next handler must not be called when the origin-secret header is wrong")
	}
	if ctx.Response.StatusCode() != fasthttp.StatusForbidden {
		t.Fatalf("want status 403, got %d", ctx.Response.StatusCode())
	}
}

func TestOriginSecret_EmptySecretDisablesEnforcement(t *testing.T) {
	nextCalled := false
	next := func(ctx *fasthttp.RequestCtx) {
		nextCalled = true
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := requireOriginSecret("")(next)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("x-rayward-cf-secret", "anything")
	handler(ctx)

	if !nextCalled {
		t.Fatal("next handler must be called when no secret is configured (enforcement disabled)")
	}
	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("want status 200, got %d", ctx.Response.StatusCode())
	}
}

// TestOriginSecret_HealthPathsExempt verifies the platform health-probe
// endpoints bypass origin-secret enforcement. ACA liveness/readiness probes
// hit these directly on the replica with no Cloudflare header; gating them
// 403s every probe and the revision never becomes healthy.
func TestOriginSecret_HealthPathsExempt(t *testing.T) {
	for _, path := range []string{"/health", "/livez", "/readyz"} {
		nextCalled := false
		next := func(ctx *fasthttp.RequestCtx) {
			nextCalled = true
			ctx.SetStatusCode(fasthttp.StatusOK)
		}

		handler := requireOriginSecret("expected-secret")(next)

		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetRequestURI(path)
		// No x-rayward-cf-secret header — mirrors an internal ACA probe.
		handler(ctx)

		if !nextCalled {
			t.Fatalf("%s: probe must pass without the origin-secret header", path)
		}
		if ctx.Response.StatusCode() != fasthttp.StatusOK {
			t.Fatalf("%s: want status 200, got %d", path, ctx.Response.StatusCode())
		}
	}
}

// TestOriginSecret_NonHealthPathStillGated guards against the exemption being
// too broad — a normal path with no header must still be rejected.
func TestOriginSecret_NonHealthPathStillGated(t *testing.T) {
	nextCalled := false
	next := func(ctx *fasthttp.RequestCtx) {
		nextCalled = true
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := requireOriginSecret("expected-secret")(next)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI("/v1/chat/completions")
	handler(ctx)

	if nextCalled {
		t.Fatal("non-health path must still require the origin-secret header")
	}
	if ctx.Response.StatusCode() != fasthttp.StatusForbidden {
		t.Fatalf("want status 403, got %d", ctx.Response.StatusCode())
	}
}
