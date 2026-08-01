package lib

import (
	"io"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
)

// prodLeakBody is the ACTUAL body measured on router2.trueward.ai on
// 2026-08-01, trimmed only of the completion text. Using the real payload
// rather than a hand-written stub is deliberate: a synthetic fixture would have
// been written from the same understanding as the fix, so it could not catch a
// misunderstanding of the shape.
const prodLeakBody = `{"id":"chatcmpl-E7yMPeIaFPu2h1iIksGRfiuEjpPkU","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Hello!"}}],"created":1785569029,"model":"gpt-5.6-luna-2026-07-09","object":"chat.completion","usage":{"prompt_tokens":12,"total_tokens":30},"extra_fields":{"request_type":"chat_completion","routing_info":{"provider":"azure","model":"gpt-5.6-luna","key":"east"},"provider":"azure","latency":1496,"chunk_index":0,"provider_response_headers":{"Apim-Request-Id":"453ce5ea-5c71-497d-8aca-654e41de1446","X-Ms-Region":"East US 2","Azureml-Model-Session":"d20260721160656-07a7b394","X-Ratelimit-Limit-Tokens":"5000000"}}}`

// leakMarkers are the specific strings whose presence in an external response is
// the defect. Asserted individually so a failure names WHICH disclosure escaped
// rather than just "bodies differ".
var leakMarkers = []string{
	"extra_fields",
	"routing_info",
	"provider_response_headers",
	"5000000",         // our purchased Azure TPM
	"East US 2",       // our deployment region
	"east",            // our internal key alias
	"azure",           // the upstream we buy from
	"Apim-Request-Id", // the upstream's own request id
	"Azureml-Model-Session",
	"is_bifrost_error", // names the gateway software, in a field NAME
}

// prodErrorBody is the ACTUAL error body measured on router2.trueward.ai,
// 2026-08-01, from a request with a misspelled `role`. `extra_fields` was
// removed by #467; `is_bifrost_error` outlived it, because a field NAME does not
// look like a leak until you remember that names disclose as readily as values —
// the same reasoning that makes the header policy an allowlist.
const prodErrorBody = `{"is_bifrost_error":false,"status_code":400,"error":{"type":"invalid_request_error","code":"invalid_value","message":"Invalid value: 'wizard'. Supported values are: 'system', 'assistant', 'user', 'function', 'tool', and 'developer'.","param":"messages[0].role"}}`

func TestStripRemovesTheGatewaySoftwareName(t *testing.T) {
	got, ok := StripExtraFields([]byte(prodErrorBody))
	if !ok {
		t.Fatal("StripExtraFields reported failure on a well-formed error body")
	}
	assertNoLeak(t, string(got))

	// Everything ACTIONABLE must survive. Dropping the flag must not cost the
	// caller the reason their request was rejected — that would be the
	// over-sanitization failure this policy is otherwise careful to avoid.
	for _, keep := range []string{
		"invalid_request_error",
		"invalid_value",
		"Invalid value: 'wizard'",
		"messages[0].role",
		"400",
	} {
		if !strings.Contains(string(got), keep) {
			t.Errorf("scrub destroyed actionable error detail %q\nbody: %s", keep, got)
		}
	}
}

func TestStripRemovesEveryKeyFromOneBody(t *testing.T) {
	// A body carrying BOTH keys must lose both in a single pass. The fast-path
	// probe returns on its first hit, so a loop that stopped at the first Unset
	// would leave the second key in place and still report success.
	both := `{"is_bifrost_error":false,"extra_fields":{"routing_info":{"key":"east"}},"error":{"message":"x"}}`
	got, ok := StripExtraFields([]byte(both))
	if !ok {
		t.Fatal("unexpected failure")
	}
	assertNoLeak(t, string(got))
	if !strings.Contains(string(got), `"message":"x"`) {
		t.Errorf("payload lost: %s", got)
	}
}

func assertNoLeak(t *testing.T, body string) {
	t.Helper()
	for _, marker := range leakMarkers {
		if strings.Contains(body, marker) {
			t.Errorf("external body still leaks %q\nbody: %s", marker, body)
		}
	}
}

func TestStripExtraFieldsRemovesTheWholeObject(t *testing.T) {
	got, ok := StripExtraFields([]byte(prodLeakBody))
	if !ok {
		t.Fatal("StripExtraFields reported failure on a well-formed body")
	}
	assertNoLeak(t, string(got))

	// The completion itself must survive — a policy that empties the body would
	// pass the leak assertions above while breaking every caller.
	for _, keep := range []string{"chatcmpl-E7yMPeIaFPu2h1iIksGRfiuEjpPkU", "Hello!", "total_tokens"} {
		if !strings.Contains(string(got), keep) {
			t.Errorf("scrub destroyed caller-visible content %q\nbody: %s", keep, got)
		}
	}
}

func TestStripExtraFieldsIsAllocationFreeWhenAbsent(t *testing.T) {
	// The probe short-circuit keeps this policy off the hot path for every chunk
	// that never had the key. If it regresses to always parsing, streaming cost
	// per token goes up silently.
	body := []byte(`{"choices":[{"delta":{"content":"Hi"}}]}`)
	got, ok := StripExtraFields(body)
	if !ok {
		t.Fatal("unexpected failure")
	}
	if &got[0] != &body[0] {
		t.Error("StripExtraFields copied a body that needed no change")
	}
}

func TestStripExtraFieldsFailsClosedOnUnparseableLeak(t *testing.T) {
	// The one combination where passing through would ship exactly what this
	// policy removes.
	if _, ok := StripExtraFields([]byte(`{"extra_fields":{"routing_info":{"key":"east"`)); ok {
		t.Fatal("truncated body carrying the key was reported safe")
	}
}

func TestStripExtraFieldsLeavesNonJSONAlone(t *testing.T) {
	// Plain-text 500s and upstream binary bodies are not ours and cannot carry
	// the key — rewriting them would turn this policy into an outage.
	const plain = "upstream returned a plain text error"
	got, ok := StripExtraFields([]byte(plain))
	if !ok || string(got) != plain {
		t.Fatalf("non-JSON body was modified: %q ok=%v", got, ok)
	}
}

func TestStripExtraFieldsLeavesNestedMentionAlone(t *testing.T) {
	// The probe is a bytes match, so a nested occurrence reaches the parser.
	// Unset() reports it did not exist at the top level and the document is
	// returned unchanged rather than guessed at.
	const nested = `{"choices":[{"message":{"content":"the extra_fields key is documented here"}}]}`
	got, ok := StripExtraFields([]byte(nested))
	if !ok || string(got) != nested {
		t.Fatalf("nested mention was rewritten: %q", got)
	}
}

// nopReadCloser adapts a Reader so it can stand in for SSEStreamReader.
type nopReadCloser struct {
	io.Reader
	closed bool
}

func (n *nopReadCloser) Close() error { n.closed = true; return nil }

// byteAtATimeReader returns one byte per Read, so a JSON payload is split at
// every possible boundary — including mid-token and mid-escape.
type byteAtATimeReader struct {
	data []byte
	pos  int
}

func (r *byteAtATimeReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

func externalCtx() *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)
	// The wrapper only applies to genuine SSE, so the streaming tests below have
	// to declare it. Callers must set Content-Type BEFORE installing the stream;
	// every production site does.
	ctx.Response.Header.SetContentType(sseContentType)
	return ctx
}

// externalCtxWithContentType is the same, for the content-type gate tests.
func externalCtxWithContentType(contentType string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)
	ctx.Response.Header.SetContentType(contentType)
	return ctx
}

func TestWrapSSESkipsNonSSEContentTypes(t *testing.T) {
	// The wrapper is LINE-oriented, so applying it to a stream without newlines
	// buffers the whole response until EOF: a streaming endpoint silently turned
	// batch, plus unbounded memory. Found by review, not by the tests above —
	// they all used SSE, so the unconditional wrap looked correct.
	//
	// `application/vnd.amazon.eventstream` is the sharper case: it is BINARY
	// framing that handleStreaming really can emit, and line-scanning it is not
	// merely wasteful but meaningless.
	for _, contentType := range []string{
		"application/json",
		"application/vnd.amazon.eventstream",
		"application/octet-stream",
		"", // never set — must not be treated as SSE
	} {
		t.Run(contentType, func(t *testing.T) {
			src := &nopReadCloser{Reader: strings.NewReader(`{"extra_fields":{"routing_info":{"key":"east"}}}`)}
			got := WrapSSEForExternalAudience(externalCtxWithContentType(contentType), src)
			if got != io.ReadCloser(src) {
				t.Errorf("content-type %q was wrapped by the line-oriented SSE scrubber", contentType)
			}
		})
	}
}

func TestWrapSSEAppliesToSSEWithCharsetParameter(t *testing.T) {
	// Content-Type is frequently "text/event-stream; charset=utf-8". An exact
	// string comparison would skip it and silently disable the whole policy on
	// the paths that spell it that way.
	src := &nopReadCloser{Reader: strings.NewReader("data: {\"extra_fields\":{\"a\":1}}\n\n")}
	got := WrapSSEForExternalAudience(externalCtxWithContentType("text/event-stream; charset=utf-8"), src)
	if got == io.ReadCloser(src) {
		t.Fatal("SSE with a charset parameter was not wrapped")
	}
	out, _ := io.ReadAll(got)
	if strings.Contains(string(out), "extra_fields") {
		t.Errorf("not scrubbed: %s", out)
	}
}

func TestWrapSSEScrubsEveryChunk(t *testing.T) {
	// Two chunks, each carrying its own extra_fields — the measured shape. A
	// test with one chunk would pass against a fix that only scrubbed the first.
	chunk := `{"id":"c1","choices":[{"delta":{"content":"Hi"}}],"extra_fields":{"routing_info":{"provider":"azure","key":"east"},"provider":"azure","chunk_index":N}}`
	sse := "data: " + strings.Replace(chunk, "N", "0", 1) + "\n\n" +
		"data: " + strings.Replace(chunk, "N", "1", 1) + "\n\n" +
		"data: [DONE]\n\n"

	wrapped := WrapSSEForExternalAudience(externalCtx(), &nopReadCloser{Reader: strings.NewReader(sse)})
	out, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	got := string(out)
	assertNoLeak(t, got)
	if n := strings.Count(got, "data: "); n != 3 {
		t.Errorf("SSE framing damaged, want 3 data lines, got %d:\n%s", n, got)
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Errorf("[DONE] sentinel lost:\n%s", got)
	}
	if !strings.Contains(got, `"content":"Hi"`) {
		t.Errorf("scrub destroyed the delta payload:\n%s", got)
	}
}

func TestWrapSSESurvivesSplitReads(t *testing.T) {
	// The failure this guards against is silent corruption, not a leak: scrubbing
	// per Read instead of per line would try to parse a fragment of JSON.
	sse := `data: {"id":"c1","extra_fields":{"routing_info":{"key":"east"}},"choices":[{"delta":{"content":"Hi"}}]}` + "\n\n"

	wrapped := WrapSSEForExternalAudience(externalCtx(), &nopReadCloser{Reader: &byteAtATimeReader{data: []byte(sse)}})
	out, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	got := string(out)
	assertNoLeak(t, got)
	if !strings.Contains(got, `"content":"Hi"`) {
		t.Errorf("payload corrupted by split reads:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Errorf("SSE terminator lost:\n%q", got)
	}
}

func TestWrapSSEFlushesUnterminatedFinalLine(t *testing.T) {
	const sse = `data: {"a":1}`
	wrapped := WrapSSEForExternalAudience(externalCtx(), &nopReadCloser{Reader: strings.NewReader(sse)})
	out, _ := io.ReadAll(wrapped)
	if string(out) != sse {
		t.Fatalf("unterminated final line mangled:\n got: %q\nwant: %q", out, sse)
	}
}

func TestWrapSSELeavesCleanStreamByteIdentical(t *testing.T) {
	const sse = "event: ping\ndata: {\"a\":1}\n\nid: 42\ndata: [DONE]\n\n"
	wrapped := WrapSSEForExternalAudience(externalCtx(), &nopReadCloser{Reader: strings.NewReader(sse)})
	out, _ := io.ReadAll(wrapped)
	if string(out) != sse {
		t.Fatalf("clean SSE was rewritten:\n got: %q\nwant: %q", out, sse)
	}
}

func TestWrapSSEIsIdentityForInternalCallers(t *testing.T) {
	// Absence of the marker means INTERNAL, and internal behaviour must be
	// byte-identical to before this patch existed — including NOT paying for a
	// wrapper on every streamed token.
	src := &nopReadCloser{Reader: strings.NewReader("data: {}\n\n")}
	if got := WrapSSEForExternalAudience(&fasthttp.RequestCtx{}, src); got != io.ReadCloser(src) {
		t.Fatal("internal stream was wrapped")
	}
}

func TestWrapSSEPropagatesClose(t *testing.T) {
	// fasthttp closes a body stream through an io.Closer assertion. If the
	// wrapper swallows it, SSEStreamReader's closeCh stays open and its producer
	// goroutine never learns the client went away.
	src := &nopReadCloser{Reader: strings.NewReader("data: {}\n\n")}
	wrapped := WrapSSEForExternalAudience(externalCtx(), src)
	if err := wrapped.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !src.closed {
		t.Fatal("Close was not propagated to the wrapped stream")
	}
}

func TestIsExternalAudienceTokenising(t *testing.T) {
	// GCP custom_request_headers ADDS rather than replaces, so a client-supplied
	// value can arrive alongside ours, duplicated or comma-joined.
	for _, tc := range []struct {
		name   string
		values []string
		want   bool
	}{
		{"exact", []string{"external"}, true},
		{"uppercase", []string{"EXTERNAL"}, true},
		{"comma joined after a forged value", []string{"internal,external"}, true},
		{"duplicate entries, forged first", []string{"internal", "external"}, true},
		{"forged only", []string{"internal"}, false},
		{"prefix is not a token", []string{"externally"}, false},
		{"absent", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &fasthttp.RequestCtx{}
			for _, v := range tc.values {
				ctx.Request.Header.Add(AudienceRequestHeader, v)
			}
			if got := IsExternalAudience(ctx); got != tc.want {
				t.Errorf("IsExternalAudience(%v) = %v, want %v", tc.values, got, tc.want)
			}
		})
	}
}

func TestApplyExternalBodyPolicyLeavesStreamsToTheWrapper(t *testing.T) {
	// The buffered policy must not touch a stream: reading it here would consume
	// bytes the transport still needs, and SetBodyStream would close it.
	ctx := externalCtx()
	src := &nopReadCloser{Reader: strings.NewReader("data: {}\n\n")}
	ctx.Response.SetBodyStream(src, -1)

	ApplyExternalBodyPolicy(ctx)

	if src.closed {
		t.Fatal("buffered policy closed a live stream")
	}
	if ctx.Response.BodyStream() != io.Reader(src) {
		t.Fatal("buffered policy replaced the stream")
	}
}
