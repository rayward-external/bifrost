package governance

// Self-service usage reporting: what a key holder spent, on the response they
// already made.
//
// WHY THIS EXISTS
//
// A caller has no other way to see it. The admin surfaces are behind an
// operator sign-in, and the model/catalog routes are refused outright to an
// external audience, so a response header is the entire channel. The sibling
// gateway has published `x-usage-cost` / `-spend` / `-budget` for a while;
// Bifrost published nothing at all, on any hostname, which meant a key issued
// against this gateway had no way to answer "how much have I spent" short of
// asking a human.
//
// WHY THE VALUES ARE SNAPSHOTTED RATHER THAN READ AT WRITE TIME
//
// The transport writes response headers in two places and neither one can reach
// the store:
//
//   - buffered responses go through HTTPTransportPostHook, which owns
//     resp.Headers and is applied back to the wire (applyResponse=true);
//   - STREAMED responses do not call that hook at all. Their headers are
//     written before the first chunk, and the deferred post-hook that runs when
//     the stream ends is explicitly invoked with applyResponse=false because
//     "the response is already on the wire and mutating ctx.Response would
//     corrupt the chunked stream".
//
// So the stream path needs the numbers BEFORE the request runs. Enforcement
// already reads every one of them — CheckBudget compares budget.CurrentUsage
// against budget.EffectiveMaxLimit() for each applicable budget — so the
// snapshot records what enforcement actually acted on rather than issuing a
// second, disagreeing read. Same reasoning as the sibling gateway's, and the
// same failure mode if it is lost: the formatter stays correct and the header
// silently empties.
//
// WHY COST IS ABSENT ON A STREAM
//
// Structural, not an oversight, and true of both gateways for the same reason:
// a stream's response head is written before the call has a cost. Spend and
// budget are pre-request figures and survive; per-call cost does not exist yet.
// The customer-facing page documents this and tells the reader to derive a
// streamed call's cost from the movement in `x-usage-spend`.

import (
	"strconv"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// ContextKeyUsageSnapshot carries the UsageSnapshot from the governance plugin
// to the HTTP transport. Deliberately NOT one of the reserved keys in
// core/schemas: reserved keys are core-owned and need setReservedValue to
// survive the restricted-writes guard, whereas this is plugin-owned and written
// with a plain SetValue on the pre-request path.
const ContextKeyUsageSnapshot schemas.BifrostContextKey = "governance-usage-snapshot"

// Response header names. BOTH families are written, and the pair is what makes
// one contract work for two audiences that must not see the same thing.
//
//	branded  x-bifrost-usage-*  names the gateway, so it is for internal callers
//	                            only — and it is the rename SOURCE for external
//	                            ones, since a branded name never crosses the
//	                            boundary itself
//	neutral  x-usage-*          the documented contract, discloses nothing
//
// On the internal hostname the audience middleware does not run, so a caller
// receives both. On the external one the middleware renames the branded value
// into the neutral name and deletes everything outside its allowlist — which
// includes the branded original AND the neutral copy written here.
//
// That the neutral copy dies externally is load-bearing, not waste. The neutral
// names are deliberately absent from the allowlist so the rename is the only
// thing that can mint one, and that is what guarantees an external caller sees
// exactly ONE value per name. applyBifrostResponseHeaders copies the upstream's
// own response headers onto the response verbatim and un-prefixed, so a
// provider that starts sending `x-usage-cost` would otherwise sit beside ours
// and be comma-joined by the client into something non-numeric. Allowlist a
// neutral name and that protection is gone.
const (
	HeaderUsageCost   = "x-bifrost-usage-cost"
	HeaderUsageSpend  = "x-bifrost-usage-spend"
	HeaderUsageBudget = "x-bifrost-usage-budget"

	NeutralHeaderUsageCost   = "x-usage-cost"
	NeutralHeaderUsageSpend  = "x-usage-spend"
	NeutralHeaderUsageBudget = "x-usage-budget"
)

// UsageHeaderRenames maps each branded name to the neutral name an external
// caller receives. Consumed by the external-audience middleware; declared here
// so the two cannot drift apart silently — a rename pointing at a header
// nothing emits produces no error, just a permanently absent header.
var UsageHeaderRenames = [][2]string{
	{HeaderUsageCost, NeutralHeaderUsageCost},
	{HeaderUsageSpend, NeutralHeaderUsageSpend},
	{HeaderUsageBudget, NeutralHeaderUsageBudget},
}

// UsageBudgetWindow is one enforced budget window, as enforcement saw it.
type UsageBudgetWindow struct {
	// Duration is the window's reset period verbatim from config ("1d", "1M").
	// Published as-is: it is the customer's own configured window name, and
	// normalising it here would disagree with what they were told they bought.
	Duration string
	Limit    float64
	Spent    float64
}

// UsageSnapshot is what a caller may be told about their own consumption.
//
// Pointers, not bare floats, because zero is a legitimate value for every one
// of these and "no cost recorded" must stay distinguishable from "cost was 0".
// A dropped header reads as "no information"; a `0` reads as "this was free",
// and the customer-facing page tells readers to treat those differently.
type UsageSnapshot struct {
	Cost    *float64
	Spend   *float64
	Budgets []UsageBudgetWindow
}

// UsageSnapshotFromContext returns the snapshot the governance plugin stashed,
// or nil when governance did not run (no virtual key, or usage tracking
// explicitly skipped). Callers must treat nil as "say nothing" rather than
// as zero.
func UsageSnapshotFromContext(ctx *schemas.BifrostContext) *UsageSnapshot {
	if ctx == nil {
		return nil
	}
	snapshot, _ := ctx.Value(ContextKeyUsageSnapshot).(*UsageSnapshot)
	return snapshot
}

// Headers renders the snapshot under BOTH the branded and neutral names, since
// which one survives is the audience middleware's decision and not this
// package's (see the header-name block above).
//
// includeCost is false on the streaming path, where the head is written before
// the call has a cost. Every field is independently omissible: a key with no
// budget configured still gets whatever else it has, and a snapshot with
// nothing to say yields an empty map rather than headers carrying placeholders.
//
// Spend is currently never present on this gateway — nothing here records a
// LIFETIME total. Budgets track a windowed CurrentUsage that the sweep resets
// every cycle, and the documented meaning of the spend header is a figure that
// only ever goes up. Publishing a resetting number under that name would be
// worse than publishing none, so the field stays wired and stays nil until a
// real cumulative figure exists to put in it.
func (s *UsageSnapshot) Headers(includeCost bool) map[string]string {
	out := make(map[string]string, 6)
	if s == nil {
		return out
	}
	set := func(branded, neutral, value string) {
		out[branded] = value
		out[neutral] = value
	}
	if includeCost && s.Cost != nil {
		if v, ok := formatUsageAmount(*s.Cost); ok {
			set(HeaderUsageCost, NeutralHeaderUsageCost, v)
		}
	}
	if s.Spend != nil {
		if v, ok := formatUsageAmount(*s.Spend); ok {
			set(HeaderUsageSpend, NeutralHeaderUsageSpend, v)
		}
	}
	if v := formatBudgetWindows(s.Budgets); v != "" {
		set(HeaderUsageBudget, NeutralHeaderUsageBudget, v)
	}
	return out
}

// formatBudgetWindows renders the windows as the published contract:
//
//	1d;limit=50;spent=12.3, 1w;limit=200;spent=40.1
//
// A comma-separated list of windows, each a name plus `;`-separated key=value
// parameters. `spent` rides alongside `limit` because the separate lifetime
// spend figure is NOT scoped to any window and cannot be subtracted from a
// windowed cap — headroom has to be derivable from this one header.
//
// Returns "" when there is nothing to say, which keeps the header ABSENT. An
// empty value would parse as a real cap of zero.
//
// SAME-DURATION WINDOWS ARE COLLAPSED TO THE MOST BINDING ONE. The window name
// is a DURATION, and duration is not unique here: budgets hang off several
// entities at once (virtual key, team, customer, model scope), so two of them
// can easily both reset daily. Emitting both produced a header with two `1d`
// entries — and the published format is a list keyed by window name, which the
// parser in our own customer documentation turns into a dict. The duplicate key
// silently overwrote one cap with the other.
//
// Measured live on 2026-08-02, before this fix, from a real external key:
//
//	1d;limit=1000;spent=0.0066, 1M;limit=10000;..., 1w;limit=5000;..., 1d;limit=5000;spent=0.087
//
// Run through the documented parser that reports $4999.89 of daily headroom
// against a real binding cap of $999.99 — the LOOSER duplicate won because it
// came last, so the error is always in the dangerous direction.
//
// Collapsing rather than renaming is deliberate. Disambiguating the entries
// would mean publishing WHICH entity each budget belongs to, i.e. our internal
// governance hierarchy, to the party it constrains. The caller does not need
// that: enforcement stops them when the first cap runs out, so for a given
// duration the only figure that can change their behaviour is the one with the
// least headroom. Least REMAINING, not the smallest limit — a large, nearly
// exhausted cap binds before a small, untouched one.
func formatBudgetWindows(windows []UsageBudgetWindow) string {
	type entry struct {
		limit, spent string
		remaining    float64
	}
	binding := make(map[string]entry, len(windows))
	order := make([]string, 0, len(windows))

	for _, w := range windows {
		if w.Duration == "" {
			continue
		}
		limit, ok := formatUsageAmount(w.Limit)
		if !ok {
			continue
		}
		spent, ok := formatUsageAmount(w.Spent)
		if !ok {
			continue
		}
		candidate := entry{limit: limit, spent: spent, remaining: w.Limit - w.Spent}
		existing, seen := binding[w.Duration]
		if !seen {
			binding[w.Duration] = candidate
			order = append(order, w.Duration)
			continue
		}
		if candidate.remaining < existing.remaining {
			binding[w.Duration] = candidate
		}
	}

	// First-appearance order, which is enforcement's evaluation order. A sort
	// would be a silent change to a published contract.
	rendered := make([]string, 0, len(order))
	for _, duration := range order {
		e := binding[duration]
		rendered = append(rendered, duration+";limit="+e.limit+";spent="+e.spent)
	}
	return strings.Join(rendered, ", ")
}

// formatUsageAmount renders a dollar amount for a header a customer parses with
// an ordinary float parser.
//
// 'f' with -1 precision, NOT 'g': %g switches to an exponent for small numbers,
// and a per-request cost of 1.14e-05 is entirely ordinary here. Exponents are
// legal floats, but the published contract says only the cost header may carry
// one, so limits and spends must not. -1 precision then trims the trailing
// zeros that would otherwise render a $50 cap as "50.000000".
//
// The bool is false for values that must never be published rather than
// published wrongly: +Inf renders as "+Inf" and NaN as "NaN", both of which a
// client's float parser accepts, yielding a cap it will then compute nonsense
// headroom against. Rejecting them keeps the header absent instead.
func formatUsageAmount(v float64) (string, bool) {
	if v != v { // NaN — the only value not equal to itself.
		return "", false
	}
	// Catches +Inf and -Inf together. A comparison against math.Inf(1) alone
	// would let -Inf through, and a negative "cap" is worse than none.
	if v > 1e308 || v < -1e308 {
		return "", false
	}
	return strconv.FormatFloat(v, 'f', -1, 64), true
}
