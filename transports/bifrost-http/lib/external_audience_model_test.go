package lib

import (
	"io"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
)

// The bodies below are ACTUAL responses measured on router2.trueward.ai on
// 2026-08-02 with the external party's own key, trimmed only of completion text
// and signature blobs. Same reasoning as prodLeakBody in the sibling test file:
// a hand-written fixture would have been written from the same understanding as
// the fix, so it could not catch a misunderstanding of the shape.
//
// This is the leak #466/#467/#468 all walked past. Those closed on a
// key-REMOVAL policy; `model` is a key that must SURVIVE with a different value,
// which that policy had no branch for.
const (
	// requested: {"model":"claude-haiku-4-5"} on /v1/messages
	prodBedrockMessagesBody = `{"id":"04520256-585b-4571-9bb1-2559d06beb91","type":"message","role":"assistant","content":[{"type":"text","text":"Hello!"}],"model":"global.anthropic.claude-haiku-4-5-20251001-v1:0","stop_reason":"end_turn","usage":{"input_tokens":42,"output_tokens":108}}`

	// requested: {"model":"claude-haiku-4-5"} on /v1/chat/completions
	prodBedrockChatBody = `{"id":"chatcmpl-abc","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Hello!"}}],"created":1785629137,"model":"global.anthropic.claude-haiku-4-5-20251001-v1:0","object":"chat.completion","usage":{"prompt_tokens":12,"total_tokens":30}}`

	// Vertex echoes the name we sent, so gemini already looks clean. Kept as a
	// fixture precisely BECAUSE it is the accidental case: the rewrite must be a
	// no-op here, byte for byte.
	prodVertexChatBody = `{"id":"1oluavfgPPz30M0P256jyA0","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Hello!"}}],"created":1785629142,"model":"gemini-3.6-flash","object":"chat.completion","usage":{"prompt_tokens":6,"total_tokens":286}}`
)

// modelLeakMarkers are the substrings whose presence in an external response IS
// the defect. Asserted individually so a failure names which half escaped.
var modelLeakMarkers = []string{
	"global.",     // OUR inference-profile scope — a routing decision of ours
	"anthropic.c", // Bedrock's wire-id namespace, which names AWS as the supplier
	"-v1:0",       // Bedrock's version suffix
	"20251001",    // the upstream snapshot date
}

func assertNoModelLeak(t *testing.T, got string) {
	t.Helper()
	for _, marker := range modelLeakMarkers {
		if strings.Contains(got, marker) {
			t.Errorf("external body still discloses %q:\n%s", marker, got)
		}
	}
}

func TestModelRewriteReplacesTheBedrockWireID(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"anthropic dialect", prodBedrockMessagesBody},
		{"openai dialect", prodBedrockChatBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ApplyBodyPolicy([]byte(tc.body), "claude-haiku-4-5")
			if !ok {
				t.Fatal("ApplyBodyPolicy reported failure on a well-formed body")
			}
			assertNoModelLeak(t, string(got))
			if !strings.Contains(string(got), `"model":"claude-haiku-4-5"`) {
				t.Errorf("model was not rewritten to the requested alias:\n%s", got)
			}
			// Over-sanitization guard: the rewrite must cost the caller nothing
			// else. Everything a client reads has to survive.
			for _, keep := range []string{"Hello!", "usage", "stop"} {
				if !strings.Contains(string(got), keep) {
					t.Errorf("rewrite destroyed %q, which the caller needs:\n%s", keep, got)
				}
			}
		})
	}
}

// The Azure form of the same bug: milder, because a dated snapshot name does not
// by itself name a supplier, but still a string the caller never asked for.
func TestModelRewriteReplacesTheAzureDeploymentSnapshot(t *testing.T) {
	got, ok := ApplyBodyPolicy([]byte(prodLeakBody), "gpt-5.6-luna")
	if !ok {
		t.Fatal("ApplyBodyPolicy reported failure on a well-formed body")
	}
	if strings.Contains(string(got), "gpt-5.6-luna-2026-07-09") {
		t.Errorf("the dated deployment snapshot survived:\n%s", got)
	}
	if !strings.Contains(string(got), `"model":"gpt-5.6-luna"`) {
		t.Errorf("model was not rewritten to the requested alias:\n%s", got)
	}
	// prodLeakBody carries BOTH defects. Removing extra_fields must still happen
	// when a model rewrite is also due — the two halves share one parse, so a
	// regression in either shows up here.
	assertNoLeak(t, string(got))
}

// The no-op case must be a genuine no-op: same bytes, not a re-marshal that
// happens to be equivalent. A re-marshal would reorder keys and rewrite number
// formatting on EVERY response, which is a compatibility risk taken for nothing.
func TestModelRewriteIsBytewiseNoopWhenAlreadyCorrect(t *testing.T) {
	body := []byte(prodVertexChatBody)
	got, ok := ApplyBodyPolicy(body, "gemini-3.6-flash")
	if !ok {
		t.Fatal("ApplyBodyPolicy reported failure on a well-formed body")
	}
	if string(got) != prodVertexChatBody {
		t.Errorf("body was rewritten when the model already matched:\ngot  %s\nwant %s", got, prodVertexChatBody)
	}
	if &got[0] != &body[0] {
		t.Error("ApplyBodyPolicy copied a body that needed no change")
	}
}

// No captured model means leave `model` alone. This is the pre-existing
// behaviour, and it is what every non-inference route gets.
func TestModelRewriteSkippedWithoutARequestedModel(t *testing.T) {
	got, ok := ApplyBodyPolicy([]byte(prodBedrockChatBody), "")
	if !ok {
		t.Fatal("ApplyBodyPolicy reported failure on a well-formed body")
	}
	if string(got) != prodBedrockChatBody {
		t.Errorf("model was rewritten with no requested model:\n%s", got)
	}
}

// A `model` the MODEL wrote, in prose or in a nested object, is not ours to
// rewrite. Only the top-level field is the response's own claim about itself.
func TestModelRewriteLeavesNestedAndProseMentionsAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			"prose mentioning the key",
			`{"model":"global.anthropic.claude-haiku-4-5-20251001-v1:0","choices":[{"message":{"content":"set \"model\" in the request body"}}]}`,
		},
		{
			"nested object with its own model",
			`{"model":"global.anthropic.claude-haiku-4-5-20251001-v1:0","echo":{"model":"whatever-the-caller-sent"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ApplyBodyPolicy([]byte(tc.body), "claude-haiku-4-5")
			if !ok {
				t.Fatal("ApplyBodyPolicy reported failure on a well-formed body")
			}
			if !strings.Contains(string(got), `"model":"claude-haiku-4-5"`) {
				t.Errorf("top-level model was not rewritten:\n%s", got)
			}
			assertNoModelLeak(t, string(got))
			if strings.Contains(tc.body, "whatever-the-caller-sent") &&
				!strings.Contains(string(got), "whatever-the-caller-sent") {
				t.Errorf("a NESTED model was rewritten; only the top level is ours:\n%s", got)
			}
			if strings.Contains(tc.body, "in the request body") &&
				!strings.Contains(string(got), "in the request body") {
				t.Errorf("the completion text was mangled:\n%s", got)
			}
		})
	}
}

// A non-string model is not ours to reinterpret. Rewriting it would change the
// document's type, which is worse than the disclosure it cannot contain anyway.
func TestModelRewriteLeavesNonStringModelAlone(t *testing.T) {
	const body = `{"model":null,"choices":[]}`
	got, ok := ApplyBodyPolicy([]byte(body), "claude-haiku-4-5")
	if !ok {
		t.Fatal("ApplyBodyPolicy reported failure on a well-formed body")
	}
	if string(got) != body {
		t.Errorf("a non-string model was rewritten:\n%s", got)
	}
}

// FAILURE DIRECTION, and the asymmetry is deliberate. An unparseable body
// carrying a STRIPPED key is replaced wholesale — it would ship routing
// metadata. An unparseable body that merely mentions `model` is passed through:
// it is not a Bifrost-marshaled response, so the field a client reads is not in
// it, and nuking it would destroy bodies that leak nothing.
func TestModelRewriteDoesNotFailClosedOnUnparseableNonLeak(t *testing.T) {
	const plain = `upstream returned a plain-text 502 mentioning "model" somewhere`
	got, ok := ApplyBodyPolicy([]byte(plain), "claude-haiku-4-5")
	if !ok {
		t.Fatal("a body with no stripped key must not fail closed")
	}
	if string(got) != plain {
		t.Errorf("a non-JSON body was rewritten:\n%s", got)
	}
}

func TestStrippedKeyStillFailsClosedWhenUnparseable(t *testing.T) {
	if _, ok := ApplyBodyPolicy([]byte(`{"model":"x","extra_fields":{"routing_info":{"key":"east"`), "claude-haiku-4-5"); ok {
		t.Fatal("an unparseable body carrying routing metadata must fail closed")
	}
}

// STREAMING IS THE WORSE HALF, exactly as it was for #467: every chunk of an
// OpenAI-dialect stream carries its own `model`, so the wire id leaks once per
// chunk rather than once per response. Measured live: 4 of 10 chunks.
func TestSSEStreamRewritesModelInEveryChunk(t *testing.T) {
	const sse = `data: {"id":"1","choices":[{"delta":{"role":"assistant"}}],"model":"global.anthropic.claude-haiku-4-5-20251001-v1:0"}

data: {"id":"1","choices":[{"delta":{"content":"Hi"}}],"model":"global.anthropic.claude-haiku-4-5-20251001-v1:0"}

data: [DONE]

`
	ctx := externalCtx()
	ctx.Request.SetBody([]byte(`{"model":"claude-haiku-4-5","messages":[]}`))
	CaptureRequestedModel(ctx)

	wrapped := WrapSSEForExternalAudience(ctx, &nopReadCloser{Reader: strings.NewReader(sse)})
	out, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatalf("reading the wrapped stream: %v", err)
	}
	assertNoModelLeak(t, string(out))
	if n := strings.Count(string(out), `"model":"claude-haiku-4-5"`); n != 2 {
		t.Errorf("expected the alias in both data chunks, got %d:\n%s", n, out)
	}
	// SSE framing must survive intact, terminator included.
	if !strings.Contains(string(out), "data: [DONE]") {
		t.Errorf("the DONE sentinel was mangled:\n%s", out)
	}
}

// The same stream split at every possible byte boundary, including mid-token.
// The line buffer is what makes the rewrite safe; this is what proves it.
func TestSSEStreamRewritesModelWhenSplitByteAtATime(t *testing.T) {
	const sse = `data: {"choices":[],"model":"global.anthropic.claude-haiku-4-5-20251001-v1:0"}

`
	ctx := externalCtx()
	ctx.Request.SetBody([]byte(`{"model":"claude-haiku-4-5"}`))
	CaptureRequestedModel(ctx)

	wrapped := WrapSSEForExternalAudience(ctx, &nopReadCloser{Reader: &byteAtATimeReader{data: []byte(sse)}})
	out, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatalf("reading the wrapped stream: %v", err)
	}
	assertNoModelLeak(t, string(out))
	if !strings.Contains(string(out), `"model":"claude-haiku-4-5"`) {
		t.Errorf("model was not rewritten across split reads:\n%s", out)
	}
}

func TestCaptureRequestedModel(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"openai dialect", `{"model":"claude-haiku-4-5","messages":[]}`, "claude-haiku-4-5"},
		{"model not first", `{"messages":[],"model":"gpt-5.4-nano"}`, "gpt-5.4-nano"},
		{"no body", ``, ""},
		{"no model field", `{"messages":[]}`, ""},
		{"empty model", `{"model":""}`, ""},
		{"non-string model", `{"model":42}`, ""},
		{"not json", `not json at all, "model" included`, ""},
		{"model only nested", `{"echo":{"model":"claude-haiku-4-5"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &fasthttp.RequestCtx{}
			if tc.body != "" {
				ctx.Request.SetBody([]byte(tc.body))
			}
			CaptureRequestedModel(ctx)
			if got := RequestedModel(ctx); got != tc.want {
				t.Errorf("RequestedModel() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The buffered seam end to end, through the exported entry point the middleware
// actually calls — not just the pure function underneath it.
func TestApplyExternalBodyPolicyRewritesModel(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)
	ctx.Request.SetBody([]byte(`{"model":"claude-haiku-4-5","messages":[]}`))
	CaptureRequestedModel(ctx)
	ctx.Response.SetBody([]byte(prodBedrockMessagesBody))

	ApplyExternalBodyPolicy(ctx)

	got := string(ctx.Response.Body())
	assertNoModelLeak(t, got)
	if !strings.Contains(got, `"model":"claude-haiku-4-5"`) {
		t.Errorf("model was not rewritten on the buffered path:\n%s", got)
	}
}

// The ACTUAL Anthropic streaming envelope. `model` is NESTED under `message`,
// which the first version of this patch missed entirely: a streamed
// /v1/messages response disclosed Bedrock and our profile scope on every
// message_start while the buffered response on the same route was clean.
const prodAnthropicMessageStart = `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","content":[],"model":"global.anthropic.claude-haiku-4-5-20251001-v1:0","stop_reason":null,"usage":{"input_tokens":42,"output_tokens":1}}}`

// The same shape on /v1/responses events.
const prodResponsesCreated = `{"type":"response.created","response":{"id":"resp_01","model":"global.anthropic.claude-haiku-4-5-20251001-v1:0","status":"in_progress"}}`

func TestModelRewriteReachesTheNestedEnvelopes(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"anthropic message_start", prodAnthropicMessageStart},
		{"openai response.created", prodResponsesCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ApplyBodyPolicy([]byte(tc.body), "claude-haiku-4-5")
			if !ok {
				t.Fatal("ApplyBodyPolicy reported failure on a well-formed body")
			}
			assertNoModelLeak(t, string(got))
			if !strings.Contains(string(got), `"model":"claude-haiku-4-5"`) {
				t.Errorf("the nested model was not rewritten:\n%s", got)
			}
		})
	}
}

// A `model` a CALLER wrote must survive. The envelope list is an allowlist for
// exactly this reason — walking the document and rewriting every `model` would
// corrupt an answer to "show me a JSON body with a model field".
func TestModelRewriteLeavesCallerAuthoredEnvelopesAlone(t *testing.T) {
	const body = `{"model":"global.anthropic.claude-haiku-4-5-20251001-v1:0","choices":[{"message":{"role":"assistant","content":"{\"model\":\"gpt-4\"}"}}],"custom":{"model":"user-supplied"}}`
	got, ok := ApplyBodyPolicy([]byte(body), "claude-haiku-4-5")
	if !ok {
		t.Fatal("ApplyBodyPolicy reported failure on a well-formed body")
	}
	assertNoModelLeak(t, string(got))
	if !strings.Contains(string(got), "user-supplied") {
		t.Errorf("a non-envelope nested model was rewritten:\n%s", got)
	}
	if !strings.Contains(string(got), `gpt-4`) {
		t.Errorf("the completion text was mangled:\n%s", got)
	}
}

// The Anthropic stream end to end: EVERY frame, including the nested one.
func TestSSEStreamRewritesTheAnthropicMessageStart(t *testing.T) {
	sse := "event: message_start\ndata: " + prodAnthropicMessageStart + "\n\n" +
		`data: {"type":"content_block_delta","delta":{"text":"hi"}}` + "\n\n"
	ctx := externalCtx()
	ctx.Request.SetBody([]byte(`{"model":"claude-haiku-4-5","max_tokens":16,"messages":[]}`))

	wrapped := WrapSSEForExternalAudience(ctx, &nopReadCloser{Reader: strings.NewReader(sse)})
	out, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatalf("reading the wrapped stream: %v", err)
	}
	assertNoModelLeak(t, string(out))
	if !strings.Contains(string(out), `"model":"claude-haiku-4-5"`) {
		t.Errorf("message_start still carries the wire id:\n%s", out)
	}
	if !strings.Contains(string(out), "event: message_start") {
		t.Errorf("SSE framing was mangled:\n%s", out)
	}
}

// A NON-OBJECT document must not be re-marshalled. Widening the parse trigger to
// "any body mentioning model" newly routes these down the parse path, and
// re-serializing an array through sonic risks corrupting a body that leaked
// nothing.
func TestNonObjectDocumentsAreLeftByteIdentical(t *testing.T) {
	for _, body := range []string{
		`[{"model":"global.anthropic.claude-haiku-4-5-20251001-v1:0"}]`,
		`"model"`,
		`123`,
		`null`,
	} {
		got, ok := ApplyBodyPolicy([]byte(body), "claude-haiku-4-5")
		if !ok {
			t.Fatalf("ApplyBodyPolicy failed closed on a non-object body: %s", body)
		}
		if string(got) != body {
			t.Errorf("a non-object document was rewritten:\ngot  %s\nwant %s", got, body)
		}
	}
}

// FAIL CLOSED on a TRUNCATED response. A Bifrost-marshaled body is always valid
// JSON, so one that opens with `{` and does not parse is corrupted — and the
// bytes that did arrive can carry the wire id.
func TestTruncatedJSONCarryingTheWireIDFailsClosed(t *testing.T) {
	const truncated = `{"id":"msg_01","model":"global.anthropic.claude-haiku-4-5-20251001-v1:0","content":[`
	got, ok := ApplyBodyPolicy([]byte(truncated), "claude-haiku-4-5")
	if ok {
		t.Errorf("truncated JSON carrying the wire id passed through:\n%s", got)
	}
}

// ...but a body that was never JSON still passes through. Replacing it would
// destroy upstream error detail that discloses nothing.
func TestNonJSONBodyStillPassesThrough(t *testing.T) {
	const plain = `502 Bad Gateway: upstream said "model" unavailable`
	got, ok := ApplyBodyPolicy([]byte(plain), "claude-haiku-4-5")
	if !ok {
		t.Fatal("a non-JSON body must not fail closed")
	}
	if string(got) != plain {
		t.Errorf("a non-JSON body was rewritten:\n%s", got)
	}
}

// THE GZIP HOLE. The audience middleware is registered OUTERMOST and
// RequestDecompressionMiddleware is INNER, so an eager capture at the top of the
// middleware read COMPRESSED bytes, found no "model", and the response shipped
// the raw wire id. Lazy resolution observes the body after the inner middleware
// has replaced it.
func TestRequestedModelResolvesAfterAnInnerMiddlewareReplacesTheBody(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)
	// Stand in for gzip bytes: whatever the outer middleware would have seen,
	// it is not the JSON the handler eventually parses.
	ctx.Request.SetBody([]byte{0x1f, 0x8b, 0x08, 0x00, 0x00})

	if got := RequestedModel(ctx); got != "" {
		t.Fatalf("compressed body should yield no model, got %q", got)
	}

	// The decompression middleware runs and replaces the body...
	ctx2 := &fasthttp.RequestCtx{}
	ctx2.Request.Header.Set(AudienceRequestHeader, ExternalAudience)
	ctx2.Request.SetBody([]byte{0x1f, 0x8b, 0x08, 0x00, 0x00})
	ctx2.Request.SetBodyRaw([]byte(`{"model":"claude-haiku-4-5","messages":[]}`))

	// ...and because resolution is LAZY, the body policy sees the real model.
	if got := RequestedModel(ctx2); got != "claude-haiku-4-5" {
		t.Errorf("lazy resolution missed the decompressed model: got %q", got)
	}
}

func TestRequestedModelIsCachedAcrossCalls(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetBody([]byte(`{"model":"claude-haiku-4-5"}`))
	first := RequestedModel(ctx)
	// A streamed response calls this once per wrap; the body must not be
	// re-parsed per chunk, and a later body change must not shift mid-stream.
	ctx.Request.SetBody([]byte(`{"model":"something-else"}`))
	if second := RequestedModel(ctx); second != first {
		t.Errorf("resolution is not cached: %q then %q", first, second)
	}
}
