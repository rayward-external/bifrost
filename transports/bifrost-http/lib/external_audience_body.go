// RAYWARD FORK PATCH — minimal response-BODY exposure for external callers.
//
// # WHY THIS EXISTS, GIVEN THE HEADER PATCH ALREADY DOES
//
// handlers/external_audience_middleware.go strips the upstream vendor's headers
// out of the header block. Bifrost then republishes THE SAME HEADER SET in the
// response body, under `extra_fields.provider_response_headers`, where a header
// policy by construction cannot see it.
//
// Measured live on the external router, 2026-08-01, one ordinary chat request:
//
//	"extra_fields":{
//	  "routing_info":{"provider":"azure","model":"gpt-5.6-luna","key":"east"},
//	  "provider":"azure",
//	  "provider_response_headers":{
//	    "X-Ratelimit-Limit-Tokens":"5000000",   <- OUR purchased quota
//	    "X-Ms-Region":"East US 2",              <- OUR deployment region
//	    "Azureml-Model-Session":"d20260721160656-07a7b394",
//	    "Apim-Request-Id":"453ce5ea-5c71-497d-8aca-654e41de1446"}}
//
// The header patch's contract is that an external party "must not be able to
// learn what gateway software we run, that we proxy at all, who we proxy to, or
// what capacity we bought". Every one of those four is in that object, so the
// body route defeats the patch precisely rather than incidentally.
//
// # WHY THIS LIVES IN lib, AND WHY STREAMING IS WRAPPED SOMEWHERE ELSE
//
// The natural home was the existing middleware — one seam, every route, once per
// response. That works for a buffered body and is IMPOSSIBLE for a stream, for a
// reason worth writing down because it costs an afternoon to rediscover:
//
//	fasthttp's Response.SetBodyStream() calls ResetBody(), and ResetBody()
//	CLOSES the stream it is replacing.
//
// So a middleware cannot re-wrap a body stream a handler already installed —
// reading `ctx.Response.BodyStream()` and setting a wrapper around it closes the
// original first. For SSEStreamReader, Close() shuts closeCh, which tells the
// producer goroutine to stop, and the client receives an EMPTY body. The symptom
// (empty response) points nowhere near the cause (an API contract in the
// framework), which is why the note is here rather than in a commit message.
//
// Streaming is therefore wrapped where the stream is CREATED — the two
// `SetBodyStream(NewSSEStreamReader(), -1)` sites in integrations/router.go —
// and the middleware handles only the buffered case. That splits the seam in
// two, which is worse than one seam and better than a broken one.
//
// This package is the shared floor: `handlers` imports `integrations`, so
// `integrations` cannot import `handlers`, and only `lib` is below both.
//
// # STREAMING IS NOT AN EDGE CASE HERE — IT IS THE WORSE HALF
//
// Measured the same day: EVERY SSE chunk carries its own `extra_fields` with
// `routing_info`, so a streamed answer leaks the provider and our internal key
// alias once per token rather than once per request.
//
// # FAILURE DIRECTION, WHICH IS NOT THE SAME AS THE HEADER PATCH'S
//
// The header policy can fail safe by deleting: an unparseable header is still a
// name it can drop. A body must survive as valid JSON, so "delete everything
// unrecognized" is not available and the policy is necessarily a targeted
// removal of the known keys in strippedEnvelopeKeys.
//
// That makes the parse failure the interesting case, handled asymmetrically ON
// PURPOSE:
//
//   - Body does not parse as JSON -> pass through untouched. It is not a
//     Bifrost-marshaled response (plain-text 500s, upstream binary bodies), so it
//     cannot carry any of those keys.
//   - Body does not parse BUT the raw bytes carry one of them -> replace it
//     wholesale. Failing open there would ship the payload this file exists to
//     remove, and the combination should be unreachable.
package lib

import (
	"bytes"
	"io"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/bytedance/sonic/ast"
	"github.com/valyala/fasthttp"
)

// AudienceRequestHeader is injected by the EXTERNAL load balancer's backend
// service. Deliberately vendor-neutral — AGENTS.md requires neutral header names
// in fork patches, so it must never contain the brand.
const AudienceRequestHeader = "x-gateway-audience"

// ExternalAudience is the only value that turns suppression on.
const ExternalAudience = "external"

// strippedEnvelopeKeys are the JSON keys removed from an external caller's
// response body. Each is about US rather than about the completion.
//
//   - extra_fields    the routing metadata: provider, internal key alias,
//     purchased quota, deployment region (#467).
//   - is_bifrost_error NAMES THE GATEWAY SOFTWARE, in a field name, on every
//     error. A name discloses as readily as a value — the whole
//     reason the header policy is an allowlist rather than a
//     value filter. It is Bifrost-specific rather than part of
//     any dialect, so both the OpenAI and Anthropic SDKs ignore
//     it and dropping it externally breaks no standard client.
//     The `error` object beside it carries the actual status,
//     type, code and message, so nothing actionable is lost.
//
// NAMED "ENVELOPE" AND NOT "TOP LEVEL", because the previous name was a lie
// that shipped a leak. These are removed at every path in envelopePaths, not
// just the document root — see that variable for what a root-only Unset missed
// on /v1/responses.
var strippedEnvelopeKeys = []string{"extra_fields", "is_bifrost_error"}

// strippedKeyProbes are the raw-bytes tests used both as a fast path and to
// decide whether an UNPARSEABLE body is dangerous. Quoted deliberately —
// matching the bare word would trip on a model that writes one in prose.
//
// Depth-blind by construction, which is what makes it still correct now that
// the removal reaches nested envelopes: a substring scan does not care where in
// the document the key sits.
var strippedKeyProbes = func() [][]byte {
	probes := make([][]byte, 0, len(strippedEnvelopeKeys))
	for _, key := range strippedEnvelopeKeys {
		probes = append(probes, []byte(`"`+key+`"`))
	}
	return probes
}()

// looksLikeJSON reports whether the body opens like a JSON document. Used only
// to decide whether an UNPARSEABLE body is a truncated response of ours (fail
// closed) or something that was never JSON to begin with (pass through).
func looksLikeJSON(body []byte) bool {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
}

// envelopeAt resolves one path from envelopePaths to the object it names, or
// nil when the document has no such envelope.
//
// THE OBJECT TEST IS THE WHOLE GUARD, not a nil-safety formality. `response`
// and `message` are ordinary English words, and a body is free to carry either
// as a plain string, a number or an array — an upstream error saying
// `{"message":"upstream said no"}`, a caller's own echoed field. Those are not
// envelopes, and descending into one (or re-marshalling the document on account
// of one) would damage a body that discloses nothing.
//
// Shared by BOTH policies below, which is the point: one resolver means the
// strip and the model rewrite cannot come to disagree about what an envelope is.
func envelopeAt(root *ast.Node, path []string) *ast.Node {
	owner := root
	for _, segment := range path {
		next := owner.Get(segment)
		if next == nil || !next.Valid() || next.Type() != ast.V_OBJECT {
			return nil
		}
		owner = next
	}
	return owner
}

// stripKeysIn removes every strippedEnvelopeKeys entry from one already-resolved
// envelope, reporting whether it removed anything.
//
// The error is returned rather than swallowed: ApplyBodyPolicy fails the body
// CLOSED on it. A removal that reports an error is a removal we cannot claim
// happened, and shipping a body we could not scrub is the one outcome this file
// exists to prevent.
func stripKeysIn(owner *ast.Node) (bool, error) {
	changed := false
	for _, key := range strippedEnvelopeKeys {
		existed, err := owner.Unset(key)
		if err != nil {
			return false, err
		}
		changed = changed || existed
	}
	return changed, nil
}

// rewriteModelIn sets `model` to requestedModel inside an already-resolved
// envelope, reporting whether it changed anything.
//
// Only a STRING model is rewritten. StrictString errors on null/number/object
// rather than coercing, so those fall through untouched — rewriting one would
// change the document's TYPE, a compatibility break in exchange for nothing,
// since a non-string cannot carry the wire id.
func rewriteModelIn(owner *ast.Node, requestedModel string) bool {
	current := owner.Get(modelKey)
	if current == nil || !current.Valid() {
		return false
	}
	existing, err := current.StrictString()
	if err != nil || existing == requestedModel {
		return false
	}
	if _, setErr := owner.Set(modelKey, ast.NewString(requestedModel)); setErr != nil {
		return false
	}
	return true
}

// bodyCarriesStrippedKey reports whether any stripped key appears in the raw
// bytes. Used for the fast path and for the fail-closed decision.
func bodyCarriesStrippedKey(body []byte) bool {
	for _, probe := range strippedKeyProbes {
		if bytes.Contains(body, probe) {
			return true
		}
	}
	return false
}

// modelKey is the envelope field REWRITTEN rather than removed, which is why it
// is not in strippedEnvelopeKeys and needs its own machinery.
//
// Bifrost echoes the model string the UPSTREAM returned, not the one the caller
// asked for. On Bedrock that string is the wire id:
//
//	request  {"model":"claude-haiku-4-5"}
//	response {"model":"global.anthropic.claude-haiku-4-5-20251001-v1:0"}
//
// Two disclosures in one value. `anthropic.<model>-v1:0` is Bedrock's id format,
// naming AWS as the supplier — the same fact the header allowlist drops
// `x-amzn-`/`x-amz-` for, and the same fact the LiteLLM fork rewrites
// `msg_bdrk_` -> `msg_` to hide. And `global.` is our inference-profile scope,
// i.e. a routing decision of ours that is nobody else's business. Azure leaks a
// milder version of the same shape (`gpt-5.4-nano` -> `gpt-5.4-nano-2026-03-17`).
//
// Vertex happens to echo the name we sent, so Gemini looks clean. That is luck,
// not policy: nothing stops the next provider from behaving like Bedrock, so the
// rewrite is unconditional rather than a per-provider denylist.
//
// WHY REWRITE AND NOT DELETE, which is the whole reason this is separate:
// clients read `model`, and both the OpenAI and Anthropic dialects require it.
// Deleting it breaks conforming SDKs. The policy for everything else is removal,
// and removal is simply not available here — which is exactly how this leak
// survived #466, #467 and #468: strippedEnvelopeKeys had no branch for a key that
// must survive with a different value.
//
// Echoing the request's own model is also what the caller already expects: it is
// what LiteLLM does on the sibling router, and what a client that keys a response
// map off the model name needs in order to work at all.
const modelKey = "model"

// modelKeyProbe is the raw-bytes fast path for modelKey. Quoted for the same
// reason as strippedKeyProbes: the bare word appears in ordinary prose, and
// completions are full of ordinary prose.
var modelKeyProbe = []byte(`"` + modelKey + `"`)

// envelopePaths are the objects that describe the RESPONSE rather than holding
// something a caller wrote. The empty path is the document root.
//
// ONE LIST GOVERNS BOTH POLICIES — the key removal and the model rewrite — and
// that is a correctness decision, not tidiness. #487 is what two lists cost:
// the rewrite already knew the document root was not enough, and the removal
// still did `root.Unset(key)`, so the SAME frame that had its nested `model`
// correctly rewritten sailed through with its nested `extra_fields` intact.
// Two mechanisms over one shape drift, and the drift is silent.
//
// THE TOP LEVEL IS NOT ENOUGH, measured twice on two different keys:
//
//	#481  {"type":"message_start","message":{"id":"…","model":"global.anthropic.…"}}
//	#487  {"type":"response.created","response":{…,"extra_fields":{"routing_info":{…}}}}
//
// So a STREAMED /v1/messages response disclosed Bedrock and our
// inference-profile scope on every message_start, and a STREAMED /v1/responses
// response republished the #467 routing metadata under `response`, while the
// buffered response on each route was clean.
//
// An ALLOWLIST of envelope names, deliberately, rather than walking the document
// and acting on every `model` or `extra_fields` it finds. A completion can
// legitimately contain either — a caller asking "show me a JSON body with a
// model field" gets one back — and silently rewriting or deleting text the model
// wrote would corrupt the answer. These two names are response envelopes;
// nothing a caller authored lands there. Note the scope is deliberately narrow:
// `choices[].message` is NOT this list's `message`, which is the document-root
// one Anthropic's message_start uses.
var envelopePaths = [][]string{
	{},           // the document root: OpenAI chat/completions, Anthropic non-streaming
	{"message"},  // Anthropic message_start
	{"response"}, // OpenAI /v1/responses events
}

// bodyReplacementOnParseFailure is what an external caller gets when a body both
// fails to parse and looks like it carries routing metadata. Deliberately
// content-free: this is the branch where we know something is wrong and have
// decided that saying nothing beats saying too much.
var bodyReplacementOnParseFailure = []byte(`{"error":{"message":"internal error","type":"internal_error"}}`)

// ssePayloadPrefix is the only SSE field whose value is JSON. `event:`, `id:`
// and `retry:` lines carry no object and are forwarded byte-for-byte.
var ssePayloadPrefix = []byte("data: ")

// IsExternalAudience reports whether the audience header explicitly says
// "external".
//
// GCP custom_request_headers ADDS a header rather than replacing it, so when a
// client also sends one the value can arrive duplicated (two entries) or
// comma-joined into one. Every occurrence is checked, and every token within it.
func IsExternalAudience(ctx *fasthttp.RequestCtx) bool {
	for _, raw := range ctx.Request.Header.PeekAll(AudienceRequestHeader) {
		for _, token := range strings.FieldsFunc(string(raw), func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t'
		}) {
			if strings.EqualFold(token, ExternalAudience) {
				return true
			}
		}
	}
	return false
}

// requestedModelUserValue is the RequestCtx key holding the model the caller
// asked for, so both body policies can echo it back.
//
// Captured ONCE, before the handler runs, rather than read from ctx.Request at
// scrub time. The two policies observe the request at different moments — the
// buffered one from a defer after the handler returns, the SSE one from inside
// it — and a handler is free to rewrite ctx.Request in between. One read at a
// known-good moment beats two reads at moments we would have to keep verifying.
const requestedModelUserValue = "rayward_external_requested_model"

// CaptureRequestedModel resolves and caches the request's model on ctx.
//
// Exported so a caller can force resolution at a chosen moment; RequestedModel
// calls it on demand, which is how every production path reaches it.
func CaptureRequestedModel(ctx *fasthttp.RequestCtx) {
	// Only a REAL answer is stored — see RequestedModel for why an empty one
	// must never reach the cache.
	if model := parseRequestedModel(ctx.Request.Body()); model != "" {
		ctx.SetUserValue(requestedModelUserValue, model)
	}
}

// RequestedModel returns the model the caller asked for, or "" — which means
// "leave the response's `model` alone".
//
// RESOLVED LAZILY, AND THAT IS THE FIX FOR A REAL LEAK, not a style choice.
//
// The first version captured eagerly at the top of the audience middleware,
// which is registered OUTERMOST. RequestDecompressionMiddleware is INNER, and it
// is what replaces the body via ctx.Request.SetBodyRaw. So for a request sent
// with `Content-Encoding: gzip` — an ordinary SDK option — the eager read saw
// COMPRESSED BYTES, found no `"model"`, captured nothing, and the response
// shipped the raw Bedrock wire id. Every test and the leak-check send
// uncompressed bodies, so all of them reported green.
//
// Resolving on demand instead means both consumers observe the body AFTER the
// inner middleware has decompressed it: the SSE wrapper runs inside the handler,
// and the buffered policy runs in a defer after it. The result is cached on ctx
// so a streamed response parses the request body once, not once per chunk.
// ONLY A NON-EMPTY RESULT IS EVER CACHED, and that is a fix for a leak that
// reached production, not a micro-optimisation.
//
// Lazy resolution alone was not enough. Anything resolving BEFORE the inner
// decompression middleware — the eager capture the audience middleware used to
// do — stored "" and every later call returned that empty string from cache. The
// gzip request then got no rewrite at all, so `Content-Encoding: gzip` still
// shipped `global.anthropic.claude-haiku-4-5-20251001-v1:0` while every
// uncompressed path measured clean. Caching only a real answer makes this
// correct regardless of WHEN it is first called, rather than relying on every
// caller knowing the middleware ordering.
//
// The cost is that a request with genuinely no model re-parses per call. That is
// a byte scan failing on the first probe, and only on routes carrying no model.
func RequestedModel(ctx *fasthttp.RequestCtx) string {
	if cached, ok := ctx.UserValue(requestedModelUserValue).(string); ok && cached != "" {
		return cached
	}
	model := parseRequestedModel(ctx.Request.Body())
	if model != "" {
		ctx.SetUserValue(requestedModelUserValue, model)
	}
	return model
}

// parseRequestedModel reads the top-level `model` out of a request body, or ""
// for a body that is absent, is not JSON, or names no model — a GET, a malformed
// request, or a route that takes no model.
//
// Errors are swallowed on purpose. This is best-effort: with no model the
// policies leave `model` alone, which is the pre-existing behaviour rather than
// a new failure. A request whose body we cannot parse is one the handler is
// about to reject anyway.
func parseRequestedModel(body []byte) string {
	if len(body) == 0 || !bytes.Contains(body, modelKeyProbe) {
		return ""
	}
	node, err := sonic.Get(body, modelKey)
	if err != nil {
		return ""
	}
	// StrictString, not String: String() COERCES, so a numeric `"model": 42`
	// would be captured as "42" and then written back into the response as a
	// string the caller never sent. Caught by TestCaptureRequestedModel.
	model, err := node.StrictString()
	if err != nil {
		return ""
	}
	return model
}

// StripExtraFields applies the body policy with no model rewrite.
//
// Kept as its own name because the stripping half is independently meaningful
// and independently tested: a body policy that only removed keys is exactly what
// this was before the model rewrite, and the tests for that half should not have
// to thread a model through to say what they mean.
func StripExtraFields(body []byte) (out []byte, ok bool) {
	return ApplyBodyPolicy(body, "")
}

// ApplyBodyPolicy removes the stripped keys from one JSON document and rewrites
// its `model` to requestedModel, returning the input unchanged when there is
// nothing to do. An empty requestedModel skips the rewrite.
//
// Both act at the envelopePaths allowlist — the document root plus the two
// nested response envelopes — never on a recursive walk.
//
// Returns ok=false only for the dangerous case described in the file header: the
// document did not parse but its bytes contain a STRIPPED key. An unparseable
// body that merely mentions `model` is passed through — it is not a
// Bifrost-marshaled response, so the field a client would read is not in it, and
// failing closed there would replace bodies that leak nothing.
func ApplyBodyPolicy(body []byte, requestedModel string) (out []byte, ok bool) {
	carriesStrippedKey := bodyCarriesStrippedKey(body)
	mayCarryModel := requestedModel != "" && bytes.Contains(body, modelKeyProbe)
	if !carriesStrippedKey && !mayCarryModel {
		// Overwhelmingly the common path for SSE: most chunks are small and the
		// scan is cheaper than a parse. Also the correct answer for every body
		// that never had a stripped key and never named a model.
		return body, true
	}

	root, err := sonic.Get(body)
	if err != nil {
		// FAIL CLOSED on anything that LOOKS like JSON. A Bifrost-marshaled
		// response is always valid JSON, so an unparseable body that still opens
		// with `{` or `[` is a truncated or corrupted one — and a truncated
		// response can carry the wire id in the bytes that did arrive. Passing
		// it through would ship the disclosure this file exists to remove.
		//
		// Genuinely non-JSON bodies (plain-text upstream 5xx, binary) pass
		// through untouched: they are not ours, they cannot carry a top-level
		// `model` a client would read, and replacing them would destroy error
		// detail that leaks nothing.
		if carriesStrippedKey || looksLikeJSON(body) {
			return nil, false
		}
		return body, true
	}

	// Non-OBJECT documents are left entirely alone. Unset/Set are meaningless on
	// an array or scalar, and re-marshalling one through sonic is a real
	// corruption risk: widening the parse trigger from the two stripped keys to
	// "any body mentioning model" newly routes such bodies down this path, so
	// the check has to exist even though the old code never needed it.
	if root.Type() != ast.V_OBJECT {
		return body, true
	}

	// ONE PASS OVER ONE ALLOWLIST, applying both policies to each envelope it
	// resolves. Walking the envelopes once rather than once per policy is what
	// makes it structurally impossible for the strip to reach a shallower set of
	// objects than the rewrite — the divergence that was #487.
	changedAny := false
	for _, envelope := range envelopePaths {
		owner := envelopeAt(&root, envelope)
		if owner == nil {
			continue
		}

		// Gated on the raw-bytes probe: if the key is nowhere in the document it
		// is not in this envelope either, and the Unset pair is pure cost on
		// every streamed chunk.
		if carriesStrippedKey {
			stripped, stripErr := stripKeysIn(owner)
			if stripErr != nil {
				return nil, false
			}
			changedAny = changedAny || stripped
		}

		if mayCarryModel && rewriteModelIn(owner, requestedModel) {
			changedAny = true
		}
	}

	if !changedAny {
		// The bytes matched inside a nested value rather than at the top level,
		// or the model already said what the caller asked for. Leave the document
		// alone rather than guess at what it meant.
		return body, true
	}

	rewritten, err := root.MarshalJSON()
	if err != nil {
		return nil, false
	}
	return rewritten, true
}

// ApplyExternalBodyPolicy removes extra_fields from a BUFFERED response body.
//
// Streams are deliberately untouched here — see the file header. They are
// wrapped at creation by WrapSSEForExternalAudience instead, because
// SetBodyStream would close the very stream this would be trying to read.
func ApplyExternalBodyPolicy(ctx *fasthttp.RequestCtx) {
	if ctx.Response.BodyStream() != nil {
		return
	}

	body := ctx.Response.Body()
	if len(body) == 0 {
		return
	}
	scrubbed, ok := ApplyBodyPolicy(body, RequestedModel(ctx))
	if !ok {
		ctx.Response.SetBody(bodyReplacementOnParseFailure)
		return
	}
	if !bytes.Equal(scrubbed, body) {
		ctx.Response.SetBody(scrubbed)
	}
}

// sseContentType is the ONLY content type this wrapper may be applied to.
const sseContentType = "text/event-stream"

// WrapSSEForExternalAudience returns src unchanged unless the caller is external
// AND the response is genuinely SSE.
//
// Call it at the point the stream is CREATED and pass the result straight to
// SetBodyStream. Wrapping an already-installed stream does not work; the file
// header explains why.
//
// # THE CONTENT-TYPE TEST IS LOAD-BEARING, NOT DEFENSIVE
//
// The wrapper is LINE-oriented: it holds bytes until it sees a newline. That is
// correct for SSE, where every frame is newline-delimited and small, and wrong
// for everything else, in two distinct ways that a review caught:
//
//   - A passthrough stream returning `application/json` has no newlines until
//     EOF, so the whole response would buffer in memory and the client would get
//     nothing incrementally — a streaming endpoint silently turned batch.
//   - handleStreaming can emit `application/vnd.amazon.eventstream`, a BINARY
//     framing. Line-scanning it is meaningless and any rewrite would corrupt it.
//
// Callers must therefore set Content-Type BEFORE installing the stream. Every
// current call site does; the guard test in integrations enforces that new ones
// route through this function, and this test is what makes it safe to call from
// a site whose content type is not statically known.
func WrapSSEForExternalAudience(ctx *fasthttp.RequestCtx, src io.ReadCloser) io.ReadCloser {
	if !IsExternalAudience(ctx) {
		return src
	}
	if !bytes.Contains(ctx.Response.Header.ContentType(), []byte(sseContentType)) {
		return src
	}
	// Resolved HERE rather than per chunk: ctx is the request's, the reader
	// outlives the handler that owns it, and reading ctx from the producer
	// goroutine on every frame would be a data race for a value that cannot
	// change mid-stream.
	return &sseScrubbingReader{src: src, requestedModel: RequestedModel(ctx)}
}

// sseScrubbingReader rewrites `data:` payloads in an SSE stream as they pass.
//
// Line-buffered because a single Read from the upstream can split a JSON payload
// anywhere — including mid-token — so scrubbing per Read would corrupt the
// stream. Whole lines are the smallest unit that is always valid to parse.
type sseScrubbingReader struct {
	src io.ReadCloser
	// requestedModel is snapshotted at construction — see WrapSSEForExternalAudience.
	// Every chunk of an OpenAI-dialect stream carries its own `model`, so without
	// this the wire id leaks once per chunk, the same shape as the #467
	// extra_fields leak this reader was built for.
	requestedModel string
	pending        bytes.Buffer // complete, already-scrubbed bytes waiting to go out
	partial        bytes.Buffer // bytes of a line not yet terminated by \n
	srcErr         error        // sticky: returned only once pending is drained
}

// Close releases the stream this reader wrapped.
//
// NOT optional plumbing. fasthttp closes a response body stream through an
// io.Closer type assertion; a wrapper that does not implement it leaves
// SSEStreamReader's closeCh open, so the producer goroutine is never told the
// client went away and leaks for the lifetime of the process.
func (r *sseScrubbingReader) Close() error {
	return r.src.Close()
}

func (r *sseScrubbingReader) Read(p []byte) (int, error) {
	for r.pending.Len() == 0 {
		if r.srcErr != nil {
			// Flush a final unterminated line before reporting the error, so a
			// stream that ends without a trailing newline is not truncated.
			if r.partial.Len() > 0 {
				r.emit(r.partial.Bytes())
				r.partial.Reset()
				continue
			}
			return 0, r.srcErr
		}

		buf := make([]byte, 4096)
		n, err := r.src.Read(buf)
		if n > 0 {
			r.partial.Write(buf[:n])
			r.drainCompleteLines()
		}
		if err != nil {
			r.srcErr = err
		}
		// A read that returned neither bytes nor an error would spin. SSE
		// producers block instead, but the guard costs nothing and a busy loop
		// in a streaming path is expensive to diagnose.
		if n == 0 && err == nil && r.pending.Len() == 0 {
			continue
		}
	}
	return r.pending.Read(p)
}

// drainCompleteLines moves every newline-terminated line out of partial and into
// pending, scrubbing the ones that carry JSON.
func (r *sseScrubbingReader) drainCompleteLines() {
	for {
		buffered := r.partial.Bytes()
		idx := bytes.IndexByte(buffered, '\n')
		if idx < 0 {
			return
		}
		line := make([]byte, idx+1)
		copy(line, buffered[:idx+1])
		r.partial.Next(idx + 1)
		r.emit(line)
	}
}

// emit scrubs one line if it is a `data:` payload and appends it to pending.
//
// The terminator is preserved exactly: SSE framing depends on it, and a `data:`
// line can end with \n or \r\n.
func (r *sseScrubbingReader) emit(line []byte) {
	if !bytes.HasPrefix(line, ssePayloadPrefix) {
		r.pending.Write(line)
		return
	}

	payload := line[len(ssePayloadPrefix):]
	var terminator []byte
	for len(payload) > 0 {
		last := payload[len(payload)-1]
		if last != '\n' && last != '\r' {
			break
		}
		terminator = append([]byte{last}, terminator...)
		payload = payload[:len(payload)-1]
	}

	// `data: [DONE]` and any other non-JSON sentinel falls through untouched:
	// ApplyBodyPolicy finds no probe match and returns it unchanged.
	scrubbed, ok := ApplyBodyPolicy(payload, r.requestedModel)
	if !ok {
		scrubbed = bodyReplacementOnParseFailure
	}

	r.pending.Write(ssePayloadPrefix)
	r.pending.Write(scrubbed)
	r.pending.Write(terminator)
}
