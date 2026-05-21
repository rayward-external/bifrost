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
