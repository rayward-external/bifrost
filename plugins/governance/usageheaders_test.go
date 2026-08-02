package governance

import (
	"math"
	"strconv"
	"testing"
)

func f(v float64) *float64 { return &v }

// A header a customer parses with an ordinary float parser must never carry a
// value that parses "successfully" into nonsense.
func TestFormatUsageAmountRejectsWhatWouldParseIntoNonsense(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got, ok := formatUsageAmount(bad); ok {
			// strconv.ParseFloat accepts "NaN", "+Inf" and "-Inf", so a client
			// does not error on these — it computes headroom against them and
			// shows the customer something meaningless.
			t.Errorf("formatUsageAmount(%v) = %q, want rejected", bad, got)
		}
	}
}

func TestFormatUsageAmountNeverUsesAnExponent(t *testing.T) {
	// A per-request cost of 1.14e-05 is entirely ordinary. %g would render it
	// with an exponent; the published contract reserves exponents for the cost
	// header alone, so limits and spends must not carry one.
	got, ok := formatUsageAmount(0.0000114)
	if !ok {
		t.Fatalf("formatUsageAmount rejected a legitimate small cost")
	}
	for _, c := range got {
		if c == 'e' || c == 'E' {
			t.Fatalf("formatUsageAmount(1.14e-05) = %q, contains an exponent", got)
		}
	}
	if parsed, err := strconv.ParseFloat(got, 64); err != nil || parsed != 0.0000114 {
		t.Fatalf("round-trip failed: %q -> %v, %v", got, parsed, err)
	}
}

func TestFormatUsageAmountTrimsTrailingZeros(t *testing.T) {
	// "50.000000" for a $50 cap is noise a customer has to read past.
	if got, _ := formatUsageAmount(50); got != "50" {
		t.Errorf("formatUsageAmount(50) = %q, want %q", got, "50")
	}
}

func TestBudgetWindowsRenderTheDocumentedFormat(t *testing.T) {
	got := formatBudgetWindows([]UsageBudgetWindow{
		{Duration: "1d", Limit: 50, Spent: 12.3},
		{Duration: "1w", Limit: 200, Spent: 40.1},
	})
	want := "1d;limit=50;spent=12.3, 1w;limit=200;spent=40.1"
	if got != want {
		t.Errorf("formatBudgetWindows() = %q, want %q", got, want)
	}
}

func TestBudgetWindowOrderIsPreserved(t *testing.T) {
	// Enforcement's evaluation order, not a sort. Reordering would be a silent
	// change to a published contract.
	got := formatBudgetWindows([]UsageBudgetWindow{
		{Duration: "1M", Limit: 500, Spent: 1},
		{Duration: "1d", Limit: 50, Spent: 2},
	})
	if want := "1M;limit=500;spent=1, 1d;limit=50;spent=2"; got != want {
		t.Errorf("formatBudgetWindows() = %q, want %q", got, want)
	}
}

func TestNothingToSayMeansNoHeaderRatherThanAZero(t *testing.T) {
	// An empty value would parse as a real cap of zero. Absence is the only
	// honest rendering of "no budget configured".
	if got := formatBudgetWindows(nil); got != "" {
		t.Errorf("formatBudgetWindows(nil) = %q, want empty", got)
	}
	if got := formatBudgetWindows([]UsageBudgetWindow{{Duration: "", Limit: 5, Spent: 1}}); got != "" {
		t.Errorf("a window with no duration should be skipped, got %q", got)
	}
	if got := formatBudgetWindows([]UsageBudgetWindow{{Duration: "1d", Limit: math.Inf(1), Spent: 1}}); got != "" {
		t.Errorf("an infinite cap must not be published, got %q", got)
	}
}

func TestAPartialWindowDoesNotSuppressAGoodOne(t *testing.T) {
	got := formatBudgetWindows([]UsageBudgetWindow{
		{Duration: "1d", Limit: math.NaN(), Spent: 1},
		{Duration: "1w", Limit: 200, Spent: 40},
	})
	if want := "1w;limit=200;spent=40"; got != want {
		t.Errorf("formatBudgetWindows() = %q, want %q", got, want)
	}
}

// Both name families, always. Which one survives is the audience middleware's
// decision; this package must not pre-empt it, because the branded name is the
// rename SOURCE and dropping it would leave an external caller with nothing.
func TestHeadersEmitBothTheBrandedAndNeutralName(t *testing.T) {
	s := &UsageSnapshot{
		Cost:    f(0.00031415),
		Budgets: []UsageBudgetWindow{{Duration: "1d", Limit: 50, Spent: 12.3}},
	}
	got := s.Headers(true)

	for _, pair := range [][2]string{
		{HeaderUsageCost, NeutralHeaderUsageCost},
		{HeaderUsageBudget, NeutralHeaderUsageBudget},
	} {
		branded, neutral := got[pair[0]], got[pair[1]]
		if branded == "" {
			t.Errorf("%s missing; the external rename has nothing to read", pair[0])
		}
		if branded != neutral {
			t.Errorf("%s=%q disagrees with %s=%q; the audience would change the VALUE, not just the name",
				pair[0], branded, pair[1], neutral)
		}
	}
}

// Structural, and true of both gateways: a stream's response head is written
// before the call has a cost.
func TestCostIsOmittedOnTheStreamingPath(t *testing.T) {
	s := &UsageSnapshot{
		Cost:    f(0.5),
		Budgets: []UsageBudgetWindow{{Duration: "1d", Limit: 50, Spent: 12.3}},
	}
	got := s.Headers(false)
	if _, ok := got[HeaderUsageCost]; ok {
		t.Error("branded cost header present on a stream")
	}
	if _, ok := got[NeutralHeaderUsageCost]; ok {
		t.Error("neutral cost header present on a stream")
	}
	// Budget is a pre-request figure and must survive.
	if got[NeutralHeaderUsageBudget] == "" {
		t.Error("budget dropped on a stream; it does not depend on the response")
	}
}

// Zero is a real value for every one of these. "This call was free" and "no cost
// was recorded" are different statements and the customer-facing page tells
// readers to treat them differently, so the distinction must survive to the wire.
func TestAZeroCostIsPublishedButAnAbsentOneIsNot(t *testing.T) {
	withZero := (&UsageSnapshot{Cost: f(0)}).Headers(true)
	if got, ok := withZero[NeutralHeaderUsageCost]; !ok || got != "0" {
		t.Errorf("a zero cost must publish as %q, got %q (present=%v)", "0", got, ok)
	}

	withNone := (&UsageSnapshot{}).Headers(true)
	if _, ok := withNone[NeutralHeaderUsageCost]; ok {
		t.Error("an unrecorded cost must leave the header ABSENT, not publish 0")
	}
}

func TestANilSnapshotSaysNothingRatherThanPanicking(t *testing.T) {
	var s *UsageSnapshot
	if got := s.Headers(true); len(got) != 0 {
		t.Errorf("nil snapshot produced %v", got)
	}
}

// Spend is wired but never populated on this gateway: nothing here records a
// LIFETIME total, and the header is documented as a figure that only ever goes
// up. This pins the reasoning so a future change has to be deliberate.
func TestSpendShipsOnlyWhenSomethingActuallyRecordedIt(t *testing.T) {
	if got := (&UsageSnapshot{}).Headers(true); got[NeutralHeaderUsageSpend] != "" {
		t.Errorf("spend published with nothing behind it: %q", got[NeutralHeaderUsageSpend])
	}
	if got := (&UsageSnapshot{Spend: f(71.27)}).Headers(true); got[NeutralHeaderUsageSpend] != "71.27" {
		t.Errorf("a recorded spend must ship, got %q", got[NeutralHeaderUsageSpend])
	}
}

// The rename table drives the middleware. A branded name in it that nothing
// emits produces no error — just a header that is silently always absent.
func TestEveryRenameSourceIsAHeaderThisPackageActuallyEmits(t *testing.T) {
	emitted := (&UsageSnapshot{
		Cost:    f(1),
		Spend:   f(2),
		Budgets: []UsageBudgetWindow{{Duration: "1d", Limit: 50, Spent: 1}},
	}).Headers(true)

	if len(UsageHeaderRenames) == 0 {
		t.Fatal("the rename table is empty; no usage header reaches an external caller")
	}
	for _, rename := range UsageHeaderRenames {
		if _, ok := emitted[rename[0]]; !ok {
			t.Errorf("rename source %q is never emitted, so %q is permanently absent externally",
				rename[0], rename[1])
		}
		if _, ok := emitted[rename[1]]; !ok {
			t.Errorf("rename target %q is not emitted at source, so internal callers lose it", rename[1])
		}
	}
}
