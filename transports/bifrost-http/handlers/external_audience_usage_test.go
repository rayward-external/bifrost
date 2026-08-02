package handlers

// The caller's OWN usage figures must cross the external boundary — they are the
// one thing on the response that belongs to the caller rather than to us — but
// they must cross exactly once, and never under a name that identifies the
// gateway.

import (
	"testing"

	"github.com/maximhq/bifrost/plugins/governance"
	"github.com/valyala/fasthttp"
)

// seedUsage writes what the gateway actually emits: every figure twice, under a
// branded name and a neutral one.
func seedUsage(h *fasthttp.ResponseHeader) {
	h.Set(governance.HeaderUsageCost, "0.00031415")
	h.Set(governance.NeutralHeaderUsageCost, "0.00031415")
	h.Set(governance.HeaderUsageBudget, "1d;limit=50;spent=12.3")
	h.Set(governance.NeutralHeaderUsageBudget, "1d;limit=50;spent=12.3")
}

func externalCtxWith(seed func(*fasthttp.ResponseHeader)) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(AudienceRequestHeader, ExternalAudience)
	handler := ExternalAudienceHeaderMiddleware()(func(ctx *fasthttp.RequestCtx) {
		seed(&ctx.Response.Header)
	})
	handler(ctx)
	return ctx
}

func TestExternalCallerKeepsTheirOwnUsageFigures(t *testing.T) {
	ctx := externalCtxWith(seedUsage)

	if got := string(ctx.Response.Header.Peek(governance.NeutralHeaderUsageCost)); got != "0.00031415" {
		t.Errorf("%s = %q, want the caller's own cost", governance.NeutralHeaderUsageCost, got)
	}
	if got := string(ctx.Response.Header.Peek(governance.NeutralHeaderUsageBudget)); got != "1d;limit=50;spent=12.3" {
		t.Errorf("%s = %q, want the caller's own budget", governance.NeutralHeaderUsageBudget, got)
	}
}

func TestExternalCallerNeverSeesTheBrandedNames(t *testing.T) {
	ctx := externalCtxWith(seedUsage)

	for _, rename := range governance.UsageHeaderRenames {
		if got := ctx.Response.Header.Peek(rename[0]); len(got) > 0 {
			t.Errorf("branded header %q reached an external caller with value %q; the NAME is the disclosure",
				rename[0], got)
		}
	}
}

// The property the capture-delete-write ordering exists for.
//
// Duplicate response headers are commonly comma-joined by clients, turning a
// number into "999, 0.004" — which both corrupts the figure and lets whoever
// supplied the other copy obscure or spoof the caller's real spend.
func TestExactlyOneCopyOfEachUsageHeaderSurvives(t *testing.T) {
	ctx := externalCtxWith(func(h *fasthttp.ResponseHeader) {
		seedUsage(h)
		// A provider that starts sending its own x-usage-cost.
		// applyBifrostResponseHeaders copies upstream response headers onto the
		// response verbatim and un-prefixed, so this is a real shape, not a
		// contrived one. It must not survive beside ours.
		h.Add(governance.NeutralHeaderUsageCost, "999")
		h.Add(governance.NeutralHeaderUsageBudget, "999")
	})

	for _, name := range []string{governance.NeutralHeaderUsageCost, governance.NeutralHeaderUsageBudget} {
		var values []string
		ctx.Response.Header.VisitAll(func(k, v []byte) {
			if string(k) == name || string(k) == canonical(name) {
				values = append(values, string(v))
			}
		})
		if len(values) != 1 {
			t.Errorf("%s appeared %d times (%q); a client will comma-join them", name, len(values), values)
		}
		if len(values) == 1 && values[0] == "999" {
			t.Errorf("%s = %q — the upstream's copy won over the gateway's", name, values[0])
		}
	}
}

// The invariant the test above rests on. Allowlisting a neutral name would let
// an upstream copy survive on its own merits, and no amount of careful ordering
// in applyExternalHeaderPolicy would help.
func TestNeutralUsageNamesAreNotAllowlisted(t *testing.T) {
	for _, rename := range governance.UsageHeaderRenames {
		if _, ok := externalAllowedHeaders[rename[1]]; ok {
			t.Errorf("%q is in externalAllowedHeaders, so an upstream copy now survives beside ours", rename[1])
		}
		if _, ok := externalAllowedHeaders[rename[0]]; ok {
			t.Errorf("branded name %q is allowlisted; it names the gateway", rename[0])
		}
	}
}

// A response with no usage figures must not gain empty ones. An empty budget
// header parses as a real cap of zero.
func TestNoUsageHeadersAreInventedWhenGovernanceRecordedNothing(t *testing.T) {
	ctx := externalCtxWith(func(h *fasthttp.ResponseHeader) {
		h.Set("content-type", "application/json")
	})

	for _, rename := range governance.UsageHeaderRenames {
		if got := ctx.Response.Header.Peek(rename[1]); len(got) > 0 {
			t.Errorf("%s = %q was invented from nothing", rename[1], got)
		}
	}
}

// Internal callers get both families: the neutral contract so one client works
// against every hostname, and the branded names existing tooling already reads.
func TestInternalAudienceKeepsBothNameFamilies(t *testing.T) {
	ctx := &fasthttp.RequestCtx{} // no audience header => internal
	handler := ExternalAudienceHeaderMiddleware()(func(ctx *fasthttp.RequestCtx) {
		seedUsage(&ctx.Response.Header)
	})
	handler(ctx)

	for _, rename := range governance.UsageHeaderRenames {
		branded := string(ctx.Response.Header.Peek(rename[0]))
		neutral := string(ctx.Response.Header.Peek(rename[1]))
		if branded == "" && neutral == "" {
			continue // not seeded (spend); nothing to assert
		}
		if branded != neutral {
			t.Errorf("internal caller: %s=%q disagrees with %s=%q", rename[0], branded, rename[1], neutral)
		}
	}
}

// fasthttp canonicalises header names on write; Peek normalises but VisitAll
// reports the stored form, so a case-insensitive compare is needed there.
func canonical(name string) string {
	var h fasthttp.ResponseHeader
	h.Set(name, "x")
	var stored string
	h.VisitAll(func(k, _ []byte) {
		if len(stored) == 0 {
			stored = string(k)
		}
	})
	return stored
}
