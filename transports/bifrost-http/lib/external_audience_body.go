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
// removal of the known keys in strippedTopLevelKeys.
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
	"github.com/valyala/fasthttp"
)

// AudienceRequestHeader is injected by the EXTERNAL load balancer's backend
// service. Deliberately vendor-neutral — AGENTS.md requires neutral header names
// in fork patches, so it must never contain the brand.
const AudienceRequestHeader = "x-gateway-audience"

// ExternalAudience is the only value that turns suppression on.
const ExternalAudience = "external"

// strippedTopLevelKeys are the top-level JSON keys removed from an external
// caller's response body. Each is about US rather than about the completion.
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
var strippedTopLevelKeys = []string{"extra_fields", "is_bifrost_error"}

// strippedKeyProbes are the raw-bytes tests used both as a fast path and to
// decide whether an UNPARSEABLE body is dangerous. Quoted deliberately —
// matching the bare word would trip on a model that writes one in prose.
var strippedKeyProbes = func() [][]byte {
	probes := make([][]byte, 0, len(strippedTopLevelKeys))
	for _, key := range strippedTopLevelKeys {
		probes = append(probes, []byte(`"`+key+`"`))
	}
	return probes
}()

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

// StripExtraFields removes the top-level extra_fields key from one JSON
// document, returning the input unchanged when there is nothing to do.
//
// Returns ok=false only for the dangerous case described in the file header: the
// document did not parse but its bytes contain the key.
func StripExtraFields(body []byte) (out []byte, ok bool) {
	if !bodyCarriesStrippedKey(body) {
		// Overwhelmingly the common path for SSE: most chunks are small and the
		// scan is cheaper than a parse. Also the correct answer for every body
		// that never had a stripped key.
		return body, true
	}

	root, err := sonic.Get(body)
	if err != nil {
		return nil, false
	}

	removedAny := false
	for _, key := range strippedTopLevelKeys {
		existed, unsetErr := root.Unset(key)
		if unsetErr != nil {
			return nil, false
		}
		removedAny = removedAny || existed
	}
	if !removedAny {
		// The bytes matched inside a nested value rather than at the top level.
		// Leave the document alone rather than guess at what it meant.
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
	scrubbed, ok := StripExtraFields(body)
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
	return &sseScrubbingReader{src: src}
}

// sseScrubbingReader rewrites `data:` payloads in an SSE stream as they pass.
//
// Line-buffered because a single Read from the upstream can split a JSON payload
// anywhere — including mid-token — so scrubbing per Read would corrupt the
// stream. Whole lines are the smallest unit that is always valid to parse.
type sseScrubbingReader struct {
	src     io.ReadCloser
	pending bytes.Buffer // complete, already-scrubbed bytes waiting to go out
	partial bytes.Buffer // bytes of a line not yet terminated by \n
	srcErr  error        // sticky: returned only once pending is drained
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
	// StripExtraFields finds no probe match and returns it unchanged.
	scrubbed, ok := StripExtraFields(payload)
	if !ok {
		scrubbed = bodyReplacementOnParseFailure
	}

	r.pending.Write(ssePayloadPrefix)
	r.pending.Write(scrubbed)
	r.pending.Write(terminator)
}
