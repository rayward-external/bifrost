package handlers

import (
	"bufio"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

// Body-policy coverage that belongs at the MIDDLEWARE level: that the buffered
// scrub is actually wired into the request path, and that it survives real
// serialization. The scrub itself, and the SSE wrapper, are tested in
// lib/external_audience_body_test.go where they live.

// bodyLeakFixture is the ACTUAL body measured on router2.trueward.ai on
// 2026-08-01, trimmed only of the completion text.
const bodyLeakFixture = `{"id":"chatcmpl-E7yMPeIaFPu2h1iIksGRfiuEjpPkU","choices":[{"index":0,"message":{"role":"assistant","content":"Hello!"}}],"model":"gpt-5.6-luna-2026-07-09","object":"chat.completion","extra_fields":{"routing_info":{"provider":"azure","model":"gpt-5.6-luna","key":"east"},"provider":"azure","provider_response_headers":{"X-Ms-Region":"East US 2","X-Ratelimit-Limit-Tokens":"5000000"}}}`

var bodyLeakMarkers = []string{
	"extra_fields", "routing_info", "provider_response_headers",
	"5000000", "East US 2", "east", "azure",
}

func assertBodyHasNoLeak(t *testing.T, body string) {
	t.Helper()
	for _, marker := range bodyLeakMarkers {
		if strings.Contains(body, marker) {
			t.Errorf("external body still leaks %q\nbody: %s", marker, body)
		}
	}
}

func TestMiddlewareScrubsBufferedBody(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)

	ExternalAudienceHeaderMiddleware()(func(c *fasthttp.RequestCtx) {
		c.SetBodyString(bodyLeakFixture)
	})(ctx)

	got := string(ctx.Response.Body())
	assertBodyHasNoLeak(t, got)
	if !strings.Contains(got, "Hello!") {
		t.Errorf("scrub destroyed the completion: %s", got)
	}
}

func TestMiddlewareLeavesInternalBodyByteIdentical(t *testing.T) {
	// Absence of the marker means INTERNAL. Internal traffic never traverses the
	// external LBs, so its behaviour must be unchanged by this patch existing.
	ctx := &fasthttp.RequestCtx{}

	ExternalAudienceHeaderMiddleware()(func(c *fasthttp.RequestCtx) {
		c.SetBodyString(bodyLeakFixture)
	})(ctx)

	if string(ctx.Response.Body()) != bodyLeakFixture {
		t.Fatalf("internal response was modified:\n got: %s", ctx.Response.Body())
	}
}

// TestOnTheWireBufferedBodyIsScrubbedAndWellFramed drives a real fasthttp server
// over a real socket.
//
// In-process assertions on RequestCtx cannot see Content-Length — fasthttp fills
// it in at serialization — so a scrub that changed the body without updating the
// length would pass every in-process test and truncate or hang every real client.
func TestOnTheWireBufferedBodyIsScrubbedAndWellFramed(t *testing.T) {
	ln := fasthttputil.NewInmemoryListener()
	defer ln.Close()

	handler := ExternalAudienceHeaderMiddleware()(func(ctx *fasthttp.RequestCtx) {
		ctx.SetContentType("application/json")
		ctx.SetBodyString(bodyLeakFixture)
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

	body := string(response.Body())
	assertBodyHasNoLeak(t, body)
	if !strings.Contains(body, "Hello!") {
		t.Errorf("completion lost on the wire: %s", body)
	}
	if got, want := response.Header.ContentLength(), len(body); got != want {
		t.Errorf("Content-Length %d does not match delivered body length %d", got, want)
	}
}

// modelLeakFixture is the ACTUAL /v1/messages body measured on
// router2.trueward.ai on 2026-08-02, trimmed only of the completion text. The
// `model` value names AWS as our Claude supplier (`anthropic.<model>-v1:0` is
// Bedrock's wire-id format) and discloses our inference-profile scope
// (`global.`).
const modelLeakFixture = `{"id":"04520256-585b-4571-9bb1-2559d06beb91","type":"message","role":"assistant","content":[{"type":"text","text":"Hello!"}],"model":"global.anthropic.claude-haiku-4-5-20251001-v1:0","stop_reason":"end_turn"}`

// This is the test that would fail if lib.CaptureRequestedModel were dropped
// from the middleware. The lib tests call it directly, so they prove the rewrite
// WORKS while saying nothing about whether it is WIRED — the exact gap that let
// the leak survive three closed issues.
func TestMiddlewareRewritesModelToTheRequestedAlias(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)
	ctx.Request.SetBody([]byte(`{"model":"claude-haiku-4-5","max_tokens":16,"messages":[]}`))

	ExternalAudienceHeaderMiddleware()(func(c *fasthttp.RequestCtx) {
		c.SetBodyString(modelLeakFixture)
	})(ctx)

	got := string(ctx.Response.Body())
	for _, marker := range []string{"global.", "anthropic.c", "-v1:0", "20251001"} {
		if strings.Contains(got, marker) {
			t.Errorf("external body still discloses %q\nbody: %s", marker, got)
		}
	}
	if !strings.Contains(got, `"model":"claude-haiku-4-5"`) {
		t.Errorf("model was not rewritten to the requested alias: %s", got)
	}
	if !strings.Contains(got, "Hello!") {
		t.Errorf("rewrite destroyed the completion: %s", got)
	}
}

// The internal counterpart, asserted for the same reason as
// TestMiddlewareLeavesInternalBodyByteIdentical: internal traffic must keep FULL
// fidelity. Knowing which Bedrock profile actually served a request is how we
// debug routing, and an over-broad fix that scrubbed everywhere would destroy it
// while still passing every external assertion above.
func TestMiddlewareLeavesInternalModelByteIdentical(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetBody([]byte(`{"model":"claude-haiku-4-5","messages":[]}`))

	ExternalAudienceHeaderMiddleware()(func(c *fasthttp.RequestCtx) {
		c.SetBodyString(modelLeakFixture)
	})(ctx)

	if string(ctx.Response.Body()) != modelLeakFixture {
		t.Errorf("internal body was modified:\ngot  %s\nwant %s", ctx.Response.Body(), modelLeakFixture)
	}
}
