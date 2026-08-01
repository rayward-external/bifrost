package integrations

import (
	"testing"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/providers/anthropic"
	"github.com/tidwall/gjson"
	"github.com/valyala/fasthttp"
)

// newMaxTokensCtx builds a request context for the max_tokens validator: path
// drives the endpoint gate, body drives the absent-vs-zero distinction.
func newMaxTokensCtx(path, body string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI(path)
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetBodyString(body)
	return ctx
}

// TestRejectAnthropicMessagesInvalidMaxTokens covers the contract that
// /v1/messages requires max_tokens >= 1, and — just as importantly — that the
// sibling endpoints sharing the "/v1/messages/{path:*}" route are NOT held to it.
func TestRejectAnthropicMessagesInvalidMaxTokens(t *testing.T) {
	for _, tc := range []struct {
		name        string
		path        string
		body        string
		wantHandled bool
		wantMessage string
	}{
		{
			name:        "missing max_tokens is rejected",
			path:        "/v1/messages",
			body:        `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`,
			wantHandled: true,
			wantMessage: "max_tokens: Field required",
		},
		{
			// The prefixed mount must enforce identically. extractExactPath()
			// strips "/anthropic/" and appends the query string, so a validator
			// built on it would silently skip every prefixed request.
			name:        "prefixed mount is rejected too",
			path:        "/anthropic/v1/messages",
			body:        `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`,
			wantHandled: true,
			wantMessage: "max_tokens: Field required",
		},
		{
			name:        "query string does not defeat the endpoint gate",
			path:        "/v1/messages?beta=true",
			body:        `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`,
			wantHandled: true,
			wantMessage: "max_tokens: Field required",
		},
		{
			// Present-but-zero is a different rejection than absent, and
			// AnthropicMessageRequest.MaxTokens is a plain int, so both arrive as 0.
			name:        "explicit zero reports the range error, not the required error",
			path:        "/v1/messages",
			body:        `{"model":"claude-opus-5","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`,
			wantHandled: true,
			wantMessage: "max_tokens: Input should be greater than or equal to 1",
		},
		{
			name:        "negative max_tokens is rejected",
			path:        "/v1/messages",
			body:        `{"model":"claude-opus-5","max_tokens":-1,"messages":[{"role":"user","content":"hi"}]}`,
			wantHandled: true,
			wantMessage: "max_tokens: Input should be greater than or equal to 1",
		},
		{
			name:        "valid max_tokens passes through",
			path:        "/v1/messages",
			body:        `{"model":"claude-opus-5","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`,
			wantHandled: false,
		},
		{
			name:        "count_tokens is exempt",
			path:        "/v1/messages/count_tokens",
			body:        `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`,
			wantHandled: false,
		},
		{
			name:        "prefixed count_tokens is exempt",
			path:        "/anthropic/v1/messages/count_tokens",
			body:        `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`,
			wantHandled: false,
		},
		{
			name:        "batch create is exempt",
			path:        "/v1/messages/batches",
			body:        `{"requests":[]}`,
			wantHandled: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newMaxTokensCtx(tc.path, tc.body)

			req := &anthropic.AnthropicMessageRequest{}
			if err := sonic.Unmarshal([]byte(tc.body), req); err != nil {
				t.Fatalf("unmarshal request body: %v", err)
			}

			handled, err := rejectAnthropicMessagesInvalidMaxTokens(ctx, nil, req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if handled != tc.wantHandled {
				t.Fatalf("handled = %v, want %v", handled, tc.wantHandled)
			}
			if !tc.wantHandled {
				return
			}

			if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadRequest {
				t.Errorf("status = %d, want %d", got, fasthttp.StatusBadRequest)
			}
			body := string(ctx.Response.Body())
			if got := gjson.Get(body, "type").String(); got != "error" {
				t.Errorf("type = %q, want %q", got, "error")
			}
			if got := gjson.Get(body, "error.type").String(); got != "invalid_request_error" {
				t.Errorf("error.type = %q, want %q — the envelope must match api.anthropic.com", got, "invalid_request_error")
			}
			if got := gjson.Get(body, "error.message").String(); got != tc.wantMessage {
				t.Errorf("error.message = %q, want %q", got, tc.wantMessage)
			}
		})
	}
}

// TestAnthropicMessagesRouteWiresMaxTokensValidation guards the seam, not the
// logic: every test above calls the validator directly, so all of them would
// still pass if the ShortCircuit were never attached to the route and the
// contract were dead in production. This asserts it is actually reachable, on
// every registered messages path and under the prefixed mount.
func TestAnthropicMessagesRouteWiresMaxTokensValidation(t *testing.T) {
	for _, prefix := range []string{"", "/anthropic"} {
		routes := createAnthropicMessagesRouteConfig(prefix, &testLogger{})
		if len(routes) == 0 {
			t.Fatalf("prefix %q: no messages routes registered", prefix)
		}
		for _, route := range routes {
			if route.ShortCircuit == nil {
				t.Fatalf("prefix %q: route %s has no ShortCircuit — the max_tokens contract is unenforced", prefix, route.Path)
			}

			ctx := newMaxTokensCtx(prefix+"/v1/messages",
				`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`)
			handled, err := route.ShortCircuit(ctx, nil, &anthropic.AnthropicMessageRequest{})
			if err != nil {
				t.Fatalf("prefix %q: %v", prefix, err)
			}
			if !handled || ctx.Response.StatusCode() != fasthttp.StatusBadRequest {
				t.Errorf("prefix %q route %s: handled=%v status=%d, want handled=true status=400",
					prefix, route.Path, handled, ctx.Response.StatusCode())
			}
		}
	}
}

// TestRejectAnthropicMessagesInvalidMaxTokensIgnoresOtherRequestTypes ensures the
// validator never claims a request it does not understand.
func TestRejectAnthropicMessagesInvalidMaxTokensIgnoresOtherRequestTypes(t *testing.T) {
	ctx := newMaxTokensCtx("/v1/messages", `{}`)

	handled, err := rejectAnthropicMessagesInvalidMaxTokens(ctx, nil, &anthropic.AnthropicTextRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Fatal("handled a non-AnthropicMessageRequest; the validator must pass those through")
	}
}
