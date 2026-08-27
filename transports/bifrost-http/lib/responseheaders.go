package lib

// Routed-identity response headers. Shared by the native `/v1` handlers and
// the drop-in integration routes: both surfaces emit the same `x-bifrost-*`
// header contract on successful responses, so the writer lives here rather
// than in either package.

import (
	"strconv"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/plugins/governance"
	"github.com/valyala/fasthttp"
)

// HTTP response header names for the routed identity. Set on every successful
// response from any integration so callers can recover the actual provider /
// model that handled the request — even when the integration converts the
// body to a provider-native shape (Anthropic, OpenAI, Bedrock) that has no
// place to surface Bifrost's `extra_fields`.
//
// Header values are derived from BifrostResponseExtraFields (populated by
// PopulateExtraFields at the end of every request path, including after
// fallback / routing-rule resolution). FallbackIndex is read from the
// BifrostContext because it isn't a field on the response struct.
//
// Naming follows the existing `x-bf-*` request-side convention (see
// `x-bf-vk`, `x-bf-key-id`, etc.).
const (
	HeaderBifrostProvider      = "x-bifrost-provider"
	HeaderBifrostOriginalModel = "x-bifrost-original-model"
	HeaderBifrostResolvedModel = "x-bifrost-resolved-model"
	HeaderBifrostFallbackIndex = "x-bifrost-fallback-index"
	HeaderBifrostRequestType   = "x-bifrost-request-type"
	// Cumulative milliseconds this request spent blocked on upstream sockets,
	// summed across every attempt and fallback. Subtract from the caller's own
	// elapsed time to get what Bifrost cost. Distinct from the per-attempt
	// latency in the response body's extra_fields, which only holds the last try.
	HeaderBifrostUpstreamLatency = "x-bifrost-upstream-latency-ms"
)

// Headers mirroring the non-deprecated ExtraFields.RoutingInfo fields 1:1.
// Emitted alongside the deprecated set above so header consumers get both
// generations, matching the JSON body contract on native routes.
const (
	HeaderBifrostRoutingInfoProvider                = "x-bifrost-routing-info-provider"
	HeaderBifrostRoutingInfoModel                   = "x-bifrost-routing-info-model"
	HeaderBifrostRoutingInfoKey                     = "x-bifrost-routing-info-key"
	HeaderBifrostRoutingInfoAliasModelID            = "x-bifrost-routing-info-alias-model-id"
	HeaderBifrostRoutingInfoAliasModelName          = "x-bifrost-routing-info-alias-model-name"
	HeaderBifrostRoutingInfoAliasModelFamily        = "x-bifrost-routing-info-alias-model-family"
	HeaderBifrostRoutingInfoIsFallback              = "x-bifrost-routing-info-is-fallback"
	HeaderBifrostRoutingInfoPrimaryProvider         = "x-bifrost-routing-info-primary-provider"
	HeaderBifrostRoutingInfoPrimaryModel            = "x-bifrost-routing-info-primary-model"
	HeaderBifrostRoutingInfoServerSideFallbackModel = "x-bifrost-routing-info-server-side-fallback-model"
)

// ApplyBifrostStreamResponseHeaders emits the routed-identity headers for a
// streaming response, before the first SSE write. Streams only carry
// ExtraFields on chunks — none exist at header-write time — so the identity
// comes from the RoutingInfo snapshot core stashes in the context at stream
// setup (BifrostContextKeyRoutingInfo). Absent snapshot (e.g. a plugin
// short-circuited the stream) emits only the request-type header.
func ApplyBifrostStreamResponseHeaders(ctx *fasthttp.RequestCtx, bifrostCtx *schemas.BifrostContext, requestType schemas.RequestType) {
	if bifrostCtx == nil {
		return
	}
	extra := schemas.BifrostResponseExtraFields{RequestType: requestType}
	if ri, ok := bifrostCtx.Value(schemas.BifrostContextKeyRoutingInfo).(schemas.RoutingInfo); ok {
		extra = ri.ToExtraFields(requestType)
	}
	ApplyBifrostResponseHeaders(ctx, bifrostCtx, extra)
}

// ApplyBifrostErrorResponseHeaders emits the routed-identity headers on an
// error response. Errors carry the same identity as successes — core populates
// BifrostError.ExtraFields.RoutingInfo plus the deprecated triplet — so failed
// and fallback-exhausted requests can still report which provider ran and
// whether a fallback fired. This exists only to bridge the error's
// BifrostErrorExtraFields to the shared writer (a different type than the
// success path's BifrostResponseExtraFields); provider response headers are
// forwarded separately by the caller.
//
// Usage headers are deliberately NOT emitted here. The published contract says
// they ride on SUCCESSFUL responses and that a request rejected before it
// reaches a model carries none, and one error path structurally cannot produce
// them anyway — SendBifrostError has no BifrostContext to read a snapshot from,
// and it is called from dozens of sites. Emitting on the error paths that DO
// have a context would make the behaviour depend on which handler failed, which
// is worse than uniformly absent: a caller cannot tell "no budget configured"
// from "this particular error path happened to drop it".
func ApplyBifrostErrorResponseHeaders(ctx *fasthttp.RequestCtx, bifrostCtx *schemas.BifrostContext, extra schemas.BifrostErrorExtraFields) {
	applyBifrostResponseHeaders(ctx, bifrostCtx, schemas.BifrostResponseExtraFields{
		RequestType:            extra.RequestType,
		RoutingInfo:            extra.RoutingInfo,
		Provider:               extra.Provider,
		OriginalModelRequested: extra.OriginalModelRequested,
		ResolvedModelUsed:      extra.ResolvedModelUsed,
	}, false)
}

// ApplyBifrostResponseHeaders writes both the upstream provider response
// headers (forwarded verbatim) and the bifrost-level `x-bifrost-*` routing
// identity headers onto the fasthttp response. Empty fields are skipped so
// the headers never appear with a blank value. Safe to call when the caller
// didn't populate `extra` — the zero value for ExtraFields produces no
// headers.
func ApplyBifrostResponseHeaders(ctx *fasthttp.RequestCtx, bifrostCtx *schemas.BifrostContext, extra schemas.BifrostResponseExtraFields) {
	// "transport-response-headers" overhead phase: writing routed-identity and upstream
	// headers onto the fasthttp response. Runs inside the root http.request span on the
	// way out, on no phase span, so it would otherwise fold into "core". Nil-safe.
	if t, ok := ctx.UserValue(schemas.BifrostContextKeyTracer).(schemas.Tracer); ok && t != nil {
		if _, h := t.StartSpanID(ctx, "transport-response-headers", schemas.SpanKindInternal); h != nil {
			defer t.EndSpan(h, schemas.SpanStatusOk, "")
		}
	}
	for key, value := range extra.ProviderResponseHeaders {
		ctx.Response.Header.Set(key, value)
	}
	if emitUsage {
		ApplyUsageHeaders(ctx, bifrostCtx)
	}
	if extra.Provider != "" {
		ctx.Response.Header.Set(HeaderBifrostProvider, string(extra.Provider))
	}
	if extra.OriginalModelRequested != "" {
		ctx.Response.Header.Set(HeaderBifrostOriginalModel, extra.OriginalModelRequested)
	}
	if extra.ResolvedModelUsed != "" {
		ctx.Response.Header.Set(HeaderBifrostResolvedModel, extra.ResolvedModelUsed)
	}
	if extra.RequestType != "" {
		ctx.Response.Header.Set(HeaderBifrostRequestType, string(extra.RequestType))
	}
	ri := extra.RoutingInfo
	if ri.Provider != "" {
		ctx.Response.Header.Set(HeaderBifrostRoutingInfoProvider, string(ri.Provider))
	}
	if ri.Model != "" {
		ctx.Response.Header.Set(HeaderBifrostRoutingInfoModel, ri.Model)
	}
	if ri.Key != "" {
		ctx.Response.Header.Set(HeaderBifrostRoutingInfoKey, ri.Key)
	}
	if ri.ResolvedKeyAlias != nil {
		if ri.ResolvedKeyAlias.ModelID != "" {
			ctx.Response.Header.Set(HeaderBifrostRoutingInfoAliasModelID, ri.ResolvedKeyAlias.ModelID)
		}
		if ri.ResolvedKeyAlias.ModelName != nil && *ri.ResolvedKeyAlias.ModelName != "" {
			ctx.Response.Header.Set(HeaderBifrostRoutingInfoAliasModelName, *ri.ResolvedKeyAlias.ModelName)
		}
		if ri.ResolvedKeyAlias.ModelFamily != nil && *ri.ResolvedKeyAlias.ModelFamily != "" {
			ctx.Response.Header.Set(HeaderBifrostRoutingInfoAliasModelFamily, string(*ri.ResolvedKeyAlias.ModelFamily))
		}
	}
	// Booleans follow the fallback-index convention: absent = false.
	if ri.IsFallback {
		ctx.Response.Header.Set(HeaderBifrostRoutingInfoIsFallback, "true")
	}
	if ri.PrimaryProvider != nil && *ri.PrimaryProvider != "" {
		ctx.Response.Header.Set(HeaderBifrostRoutingInfoPrimaryProvider, string(*ri.PrimaryProvider))
	}
	if ri.PrimaryModel != nil && *ri.PrimaryModel != "" {
		ctx.Response.Header.Set(HeaderBifrostRoutingInfoPrimaryModel, *ri.PrimaryModel)
	}
	if ri.ServerSideFallbackModel != nil && *ri.ServerSideFallbackModel != "" {
		ctx.Response.Header.Set(HeaderBifrostRoutingInfoServerSideFallbackModel, *ri.ServerSideFallbackModel)
	}
	// Fallback index lives on the request context, not the response struct.
	// 0 = primary provider succeeded; non-zero = which fallback fired
	// (1-indexed). Only emit when non-zero so the absence of the header is
	// the unambiguous "no fallback fired" signal.
	if bifrostCtx != nil {
		if idx, ok := bifrostCtx.Value(schemas.BifrostContextKeyFallbackIndex).(int); ok && idx > 0 {
			ctx.Response.Header.Set(HeaderBifrostFallbackIndex, strconv.Itoa(idx))
		}
		if upstream, ok := schemas.GetUpstreamLatency(bifrostCtx); ok {
			ctx.Response.Header.Set(HeaderBifrostUpstreamLatency,
				strconv.FormatFloat(float64(upstream)/float64(time.Millisecond), 'f', 3, 64))
		}
	}
}

// ApplyUsageHeaders emits the caller's own spend position, if governance
// recorded one for this request.
//
// Exported because ApplyBifrostResponseHeaders is NOT the single choke point it
// looked like. Large-response mode returns from streamLargeResponseIfActive
// before the shared writer runs, so that path has to call this itself — a
// successful response that silently lost its usage headers is exactly the
// failure this feature is about.
//
// Written here rather than in the governance plugin's HTTPTransportPostHook
// because this is the ONE writer all three response shapes pass through —
// buffered, streamed and error. That hook owns resp.Headers on the buffered
// path but is never invoked for a stream, so using it would need a second
// emitter and two places to keep in step.
//
// The cost/no-cost split falls out of ordering rather than a flag, which is
// what makes it correct by construction on every route at once:
//
//   - BUFFERED: core has already run PostLLMHook, which put this call's cost on
//     the snapshot, so Cost is set and ships.
//   - STREAMED: these headers are written before the first chunk, so PostLLMHook
//     has not run and Cost is still nil. The header is simply absent — which is
//     the honest answer, since the call does not yet have a cost.
//
// Spend and budget come from a pre-request snapshot and are present on both.
//
// AUDIENCE: this deliberately writes both the branded and the neutral name. The
// external-audience middleware renames the branded value into the neutral slot
// and drops everything outside its allowlist, so an external caller ends up with
// exactly one copy under the neutral name; an internal caller keeps both.
func ApplyUsageHeaders(ctx *fasthttp.RequestCtx, bifrostCtx *schemas.BifrostContext) {
	snapshot := governance.UsageSnapshotFromContext(bifrostCtx)
	if snapshot == nil {
		return
	}
	for name, value := range snapshot.Headers(snapshot.Cost != nil) {
		ctx.Response.Header.Set(name, value)
	}
}
