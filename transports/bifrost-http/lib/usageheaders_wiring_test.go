package lib

// Wiring guards for the usage headers.
//
// Every defect these cover is a SILENTLY ABSENT header on a route that was
// never exercised — which is how all four got past the first round of tests:
// those drove the writer directly and proved it writes, saying nothing about
// whether the routes reach it.
//
// The lesson worth keeping: "ApplyBifrostResponseHeaders is the one shared
// writer for buffered, streamed and error responses" was the assumption the
// whole change rested on, and it was false. Two response shapes route around
// it. These assert the call sites, because a missing one raises no error and
// changes no test.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/plugins/governance"
	"github.com/valyala/fasthttp"
)

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(body)
}

// The published contract says usage headers ride on SUCCESSFUL responses and
// that a rejected request carries none. One error path structurally cannot
// produce them (SendBifrostError has no BifrostContext), so emitting on the
// paths that do have one would make the behaviour depend on which handler
// failed — leaving a caller unable to tell "no budget configured" from "this
// error path dropped it".
func TestErrorResponsesCarryNoUsageHeaders(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	bifrostCtx := schemas.NewBifrostContext(context.Background(), time.Now())
	cost := 0.25
	bifrostCtx.SetValue(governance.ContextKeyUsageSnapshot, &governance.UsageSnapshot{
		Cost:    &cost,
		Budgets: []governance.UsageBudgetWindow{{Duration: "1d", Limit: 50, Spent: 12.3}},
	})

	ApplyBifrostErrorResponseHeaders(ctx, bifrostCtx, schemas.BifrostErrorExtraFields{
		Provider: "azure",
	})

	for _, name := range []string{
		governance.NeutralHeaderUsageCost,
		governance.NeutralHeaderUsageBudget,
		governance.HeaderUsageCost,
		governance.HeaderUsageBudget,
	} {
		if got := ctx.Response.Header.Peek(name); len(got) > 0 {
			t.Errorf("%s = %q on an error response; the contract says successful responses only",
				name, got)
		}
	}
	// The routed-identity headers must still be there — this is a scoped
	// suppression of the usage family, not of the whole writer.
	if got := ctx.Response.Header.Peek(HeaderBifrostProvider); len(got) == 0 {
		t.Error("routed-identity headers were suppressed too; only the usage family should be")
	}
}

// A successful response through the normal writer keeps them.
func TestSuccessfulResponsesCarryUsageHeaders(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	bifrostCtx := schemas.NewBifrostContext(context.Background(), time.Now())
	cost := 0.25
	bifrostCtx.SetValue(governance.ContextKeyUsageSnapshot, &governance.UsageSnapshot{Cost: &cost})

	ApplyBifrostResponseHeaders(ctx, bifrostCtx, schemas.BifrostResponseExtraFields{Provider: "azure"})

	if got := string(ctx.Response.Header.Peek(governance.NeutralHeaderUsageCost)); got != "0.25" {
		t.Errorf("%s = %q, want %q", governance.NeutralHeaderUsageCost, got, "0.25")
	}
}

// Large-response mode returns from streamLargeResponseIfActive BEFORE the shared
// writer runs, so it must apply the usage headers itself. Asserted on the call
// site: a successful large response that lost its headers looks identical to one
// whose key has no budget.
func TestLargeResponseModeAppliesUsageHeadersItself(t *testing.T) {
	src := repoFile(t, "transports/bifrost-http/handlers/utils.go")

	idx := strings.Index(src, "func streamLargeResponseIfActive")
	if idx < 0 {
		t.Fatal("streamLargeResponseIfActive is gone; re-check whether the bypass still exists")
	}
	body := src[idx:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}

	apply := strings.Index(body, "lib.ApplyUsageHeaders(")
	stream := strings.Index(body, "lib.StreamLargeResponseBody(")
	if apply < 0 {
		t.Fatal("streamLargeResponseIfActive no longer applies usage headers; every successful " +
			"large response silently loses them")
	}
	if stream < 0 {
		t.Fatal("StreamLargeResponseBody call is gone; this guard needs rewriting")
	}
	if apply > stream {
		t.Error("usage headers are applied AFTER the body stream is installed; fasthttp will not " +
			"accept header writes at that point, so they never reach the client")
	}
}

// The large-payload branch of EvaluateGovernanceRequest RETURNS early, so the
// snapshot call at the end of the function is unreachable from it.
func TestLargePayloadBranchRecordsItsOwnSnapshot(t *testing.T) {
	src := repoFile(t, "plugins/governance/main.go")

	idx := strings.Index(src, "BifrostContextKeyLargePayloadMetadata")
	if idx < 0 {
		t.Fatal("the large-payload branch is gone; re-check whether it still returns early")
	}
	branch := src[idx:]
	if end := strings.Index(branch, "\n\tif hasRoutingRules {"); end > 0 {
		branch = branch[:end]
	}
	if !strings.Contains(branch, "p.recordUsageSnapshot(") {
		t.Error("the large-payload branch returns without recording a usage snapshot, so a " +
			"governed large-payload request emits no budget headers at all")
	}
}

// loadBalanceProvider mutates the request, so a provider read before it can be
// empty for a VK with weighted providers and a bare model. The snapshot would
// then collect different budgets than the request is billed against — the header
// disagreeing with enforcement is the one failure this snapshot exists to avoid.
func TestSnapshotReadsTheProviderAfterLoadBalancing(t *testing.T) {
	src := repoFile(t, "plugins/governance/main.go")

	lb := strings.Index(src, "p.loadBalanceProvider(")
	if lb < 0 {
		t.Fatal("loadBalanceProvider call is gone; this guard needs rewriting")
	}
	// The final snapshot call — the one guarded here — is the last in the file.
	snap := strings.LastIndex(src, "p.recordUsageSnapshot(")
	if snap < 0 {
		t.Fatal("recordUsageSnapshot is never called")
	}
	if snap < lb {
		t.Error("the usage snapshot is recorded BEFORE loadBalanceProvider mutates the request; " +
			"the provider it captures can be empty and the budgets will not match billing")
	}

	// Asserted as an ADJACENT pair, not "a GetRequestFields call appears somewhere
	// in between" — there are other calls to it in this span, so the loose version
	// stayed green when the re-read was removed. Mutation-tested.
	tail := src[lb:]
	reread := "billedProvider, billedModel, _ := req.GetRequestFields()"
	call := "p.recordUsageSnapshot(ctx, virtualKey, billedProvider, billedModel)"
	i := strings.Index(tail, reread)
	if i < 0 {
		t.Fatal("the request fields are not re-read after load balancing, so the snapshot uses " +
			"the pre-routing provider and collects budgets the request will not be billed against")
	}
	between := tail[i+len(reread):]
	j := strings.Index(between, call)
	if j < 0 {
		t.Fatal("the re-read values are not the ones handed to recordUsageSnapshot")
	}
	if strings.TrimSpace(between[:j]) != "" {
		t.Errorf("something now sits between the re-read and the snapshot call (%q); if it mutates "+
			"the request the captured provider is stale again", strings.TrimSpace(between[:j]))
	}
}
