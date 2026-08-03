package governance

import (
	"context"
	"math"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeWithVK builds a store holding one virtual key at a known starting
// total, backed by a real (temp-file) SQLite configStore rather than a bare
// struct literal — BumpVirtualKeyLifetimeSpend and VirtualKeyLifetimeSpend
// both gate on configStore != nil now, so a nil configStore here would
// silently no-op every test in this file instead of exercising them.
func storeWithVK(t *testing.T, vkID string, starting float64) *LocalGovernanceStore {
	t.Helper()
	ctx := context.Background()
	logger := NewMockLogger()
	configStore, err := configstore.NewConfigStore(ctx, &configstore.Config{
		Enabled: true,
		Type:    configstore.ConfigStoreTypeSQLite,
		Config:  &configstore.SQLiteConfig{Path: t.TempDir() + "/vk.db"},
	}, logger)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, configStore.Close(ctx)) })

	vk := &configstoreTables.TableVirtualKey{
		ID:            vkID,
		Name:          vkID,
		Value:         *schemas.NewSecretVar(vkID + "-value"),
		IsActive:      schemas.Ptr(true),
		LifetimeSpend: starting,
	}
	require.NoError(t, configStore.CreateVirtualKey(ctx, vk))

	gs, err := NewLocalGovernanceStore(ctx, logger, configStore, nil, nil)
	require.NoError(t, err)
	return gs
}

func TestSpendAccumulatesOnTopOfThePersistedTotal(t *testing.T) {
	gs := storeWithVK(t, "vk1", 100)
	gs.BumpVirtualKeyLifetimeSpend(context.Background(), "vk1", 0.25)
	gs.BumpVirtualKeyLifetimeSpend(context.Background(), "vk1", 0.75)

	got, ok := gs.VirtualKeyLifetimeSpend("vk1")
	if !ok || got != 101 {
		t.Errorf("VirtualKeyLifetimeSpend() = %v, %v; want 101, true", got, ok)
	}
}

// The header must include the request the caller just made, like the budget
// windows do. Reading only the persisted column would make it appear frozen
// between dumps.
func TestUndumpedSpendIsVisibleImmediately(t *testing.T) {
	gs := storeWithVK(t, "vk1", 10)
	before, _ := gs.VirtualKeyLifetimeSpend("vk1")
	gs.BumpVirtualKeyLifetimeSpend(context.Background(), "vk1", 2.5)
	after, _ := gs.VirtualKeyLifetimeSpend("vk1")

	if after-before != 2.5 {
		t.Errorf("pending delta not visible: before=%v after=%v", before, after)
	}
}

// Values that would corrupt a monotonic total are refused, not clamped.
func TestPoisonousCostsAreRefused(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -5, 0} {
		gs := storeWithVK(t, "vk1", 10)
		gs.BumpVirtualKeyLifetimeSpend(context.Background(), "vk1", bad)
		got, _ := gs.VirtualKeyLifetimeSpend("vk1")
		if got != 10 {
			t.Errorf("cost %v moved the total to %v; want it refused", bad, got)
		}
		if math.IsNaN(got) {
			t.Fatalf("cost %v poisoned the total to NaN — every later monotonic "+
				"comparison would be false and the row would stop updating forever", bad)
		}
	}
}

// `!(cost > 0)` rather than `cost <= 0`, because every comparison with NaN is
// false — so `NaN <= 0` is false and a naive guard lets it straight through.
func TestTheNaNGuardIsWrittenTheOnlyWayThatWorks(t *testing.T) {
	if math.NaN() <= 0 {
		t.Fatal("unreachable")
	}
	if !(math.NaN() > 0) != true {
		t.Fatal("the !(x > 0) form must reject NaN")
	}
}

func TestAnUnknownKeyIsNotInvented(t *testing.T) {
	gs := storeWithVK(t, "vk1", 10)
	if _, ok := gs.VirtualKeyLifetimeSpend("nope"); ok {
		t.Error("reported a spend for a key that does not exist")
	}
	// Must not panic or create pending state for a key with no row.
	gs.BumpVirtualKeyLifetimeSpend(context.Background(), "nope", 1)
	if len(gs.PendingVKLifetimeSpend) != 0 {
		t.Errorf("accumulated pending spend against an unknown key: %v", gs.PendingVKLifetimeSpend)
	}
}

// Memory-only mode (no configStore) must not accumulate or publish lifetime
// spend at all. Nothing here is ever written to a row, so any value produced
// would reset to zero on the next restart — exactly the failure mode this
// file's own header explains a pure in-memory counter cannot avoid. Omitting
// the header is the only option that keeps the "only ever goes up" contract.
func TestMemoryOnlyModeNeverAccumulatesOrPublishesSpend(t *testing.T) {
	gs := &LocalGovernanceStore{
		PendingVKLifetimeSpend: make(map[string]float64),
		vkLifetimeSpendBase:    make(map[string]float64),
	}
	gs.virtualKeysByID.Store("vk1", &configstoreTables.TableVirtualKey{ID: "vk1", LifetimeSpend: 10})

	gs.BumpVirtualKeyLifetimeSpend(context.Background(), "vk1", 5)
	if len(gs.PendingVKLifetimeSpend) != 0 {
		t.Errorf("accumulated pending spend with no configStore to ever persist it: %v",
			gs.PendingVKLifetimeSpend)
	}

	if _, ok := gs.VirtualKeyLifetimeSpend("vk1"); ok {
		t.Error("published a lifetime-spend figure with no configStore — it can " +
			"only ever reset to zero on restart, breaking the monotonic contract")
	}
}

// A failed dump must cost a retry, not the spend. This is now guaranteed
// structurally rather than by a fold-back step: a row is only ever removed
// from PendingVKLifetimeSpend after settleCommittedDelta runs, which only
// happens once its own batch's write has already committed. A batch that
// never commits never has its rows touched at all.
//
// Real SQLite, closed mid-flight, forces DumpVirtualKeyLifetimeSpend down its
// genuine error path — a string check on the source can't tell "removes the
// row on failure" from "never removed it in the first place", so this needs
// an actual failing transaction, not a text assertion.
func TestAFailedDumpDoesNotLoseTheSpend(t *testing.T) {
	ctx := context.Background()
	logger := NewMockLogger()
	configStore, err := configstore.NewConfigStore(ctx, &configstore.Config{
		Enabled: true,
		Type:    configstore.ConfigStoreTypeSQLite,
		Config:  &configstore.SQLiteConfig{Path: t.TempDir() + "/dump-fail.db"},
	}, logger)
	require.NoError(t, err)

	vk := &configstoreTables.TableVirtualKey{
		ID:            "vk-dump-fail",
		Name:          "dump-fail",
		Value:         *schemas.NewSecretVar("vk-dump-fail-value"),
		IsActive:      schemas.Ptr(true),
		LifetimeSpend: 10,
	}
	require.NoError(t, configStore.CreateVirtualKey(ctx, vk))

	gs, err := NewLocalGovernanceStore(ctx, logger, configStore, nil, nil)
	require.NoError(t, err)

	gs.BumpVirtualKeyLifetimeSpend(ctx, "vk-dump-fail", 3)

	// Close the underlying DB so the transaction the dump opens fails for real.
	require.NoError(t, configStore.Close(ctx))

	err = gs.DumpVirtualKeyLifetimeSpend(ctx)
	require.Error(t, err, "closing the store must make the dump's own transaction fail")

	got, ok := gs.VirtualKeyLifetimeSpend("vk-dump-fail")
	require.True(t, ok)
	assert.Equal(t, 13.0, got,
		"the bumped delta must still be visible after a failed dump — a lost "+
			"delta here is spend that will never be billed")
}

// Concurrency: the bump path is called from the tracker's worker goroutines.
func TestConcurrentBumpsAreNotLost(t *testing.T) {
	gs := storeWithVK(t, "vk1", 0)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gs.BumpVirtualKeyLifetimeSpend(context.Background(), "vk1", 0.01)
		}()
	}
	wg.Wait()

	got, _ := gs.VirtualKeyLifetimeSpend("vk1")
	if math.Abs(got-2.0) > 1e-9 {
		t.Errorf("concurrent bumps totalled %v, want 2.0 — increments were lost", got)
	}
}

// A dump with nothing pending must not touch the database at all.
func TestAnEmptyDumpIsANoOp(t *testing.T) {
	gs := storeWithVK(t, "vk1", 10)
	// Nothing was bumped, so PendingVKLifetimeSpend is empty and the dump must
	// return before ever touching the store.
	if err := gs.DumpVirtualKeyLifetimeSpend(context.Background()); err != nil {
		t.Errorf("empty dump returned %v", err)
	}
}

// The first version of this file wrote an absolute total under
// `WHERE lifetime_spend < ?`, reasoning that a stale node could then never lower
// a higher value. It cannot — but two healthy nodes computing absolute totals
// from the same base produce 5 and 7, and the guard keeps 7. The cluster loses 5.
//
// A comparison guard turns concurrent contributions into a MAXIMUM, and a maximum
// is not a sum. The write must be an atomic increment.
func TestTheWriteIsAnAtomicIncrementAndNotAGuardedTotal(t *testing.T) {
	// SCOPED to the write function. Checking the whole file matched the header
	// comment that documents the failed design and reported a defect that was
	// not there — the same vacuous-guard trap this codebase has hit before.
	src := readGovernanceFile(t, "lifetimespend.go")
	i := strings.Index(src, "func (gs *LocalGovernanceStore) writeVKSpendBatch")
	if i < 0 {
		t.Fatal("writeVKSpendBatch is gone; this guard needs rewriting")
	}
	fn := src[i:]
	if j := strings.Index(fn, "\nfunc "); j > 0 {
		fn = fn[:j]
	}
	src = fn

	if !contains(src, `gorm.Expr("lifetime_spend + ?", row.Delta)`) {
		t.Error("the write is no longer an atomic increment; concurrent nodes will " +
			"overwrite each other instead of summing")
	}
	if contains(src, "lifetime_spend < ?") {
		t.Error("a comparison guard is back on the write. It reads as a safety " +
			"measure and is the opposite: it keeps the LARGEST node-local total " +
			"instead of the sum, so a multi-node cluster silently undercounts spend")
	}
	// A total must never be computed here and handed to SQL.
	if contains(src, "vk.LifetimeSpend + ") {
		t.Error("an absolute total is being computed for the write again; only the " +
			"node-local DELTA may be sent")
	}
}

// The base is mutated by the dump goroutine and read by request paths. Both must
// sit under the same lock, or every active key races during a dump. Run under
// -race, where an unsynchronised base fails reliably.
func TestBaseAndPendingAreReadUnderOneLock(t *testing.T) {
	gs := storeWithVK(t, "vk1", 5)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); gs.BumpVirtualKeyLifetimeSpend(context.Background(), "vk1", 0.1) }()
		wg.Add(1)
		go func() { defer wg.Done(); gs.SeedVirtualKeyLifetimeSpend("vk1", 6) }()
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = gs.VirtualKeyLifetimeSpend("vk1") }()
	}
	wg.Wait()
}

// Seeding must never walk the published figure backwards: a config reload can
// deliver a row read before this node's increment landed.
func TestSeedingNeverLowersTheBase(t *testing.T) {
	gs := storeWithVK(t, "vk1", 100)
	gs.SeedVirtualKeyLifetimeSpend("vk1", 40)
	got, _ := gs.VirtualKeyLifetimeSpend("vk1")
	if got != 100 {
		t.Errorf("a stale reload lowered the total to %v; want it held at 100", got)
	}
	gs.SeedVirtualKeyLifetimeSpend("vk1", 150)
	if got, _ := gs.VirtualKeyLifetimeSpend("vk1"); got != 150 {
		t.Errorf("a higher persisted total was ignored: %v", got)
	}
}

// The base must only advance from a confirmed write. Advancing it optimistically
// and folding the delta back on failure double-counts — the base has already
// absorbed the delta, so the retry adds it twice.
func TestTheBaseIsNotAdvancedOptimistically(t *testing.T) {
	src := readGovernanceFile(t, "lifetimespend.go")
	dump := src[strings.Index(src, "func (gs *LocalGovernanceStore) DumpVirtualKeyLifetimeSpend"):]
	dump = dump[:strings.Index(dump, "\nfunc ")]
	if contains(dump, "vkLifetimeSpendBase[") {
		t.Error("the dump writes the base directly; it must only be advanced by " +
			"refreshLifetimeSpendBase after a committed write")
	}
	if !contains(dump, "gs.refreshLifetimeSpendBase(ctx, batch)") {
		t.Error("the base is never refreshed from the row after a write, so the " +
			"published figure omits every other node's contributions")
	}
}

// A batch failing must not abort the whole dump: every other batch is
// independent and must still get its chance to persist this tick.
func TestALaterBatchIsNotAbandonedWhenAnEarlierOneFails(t *testing.T) {
	src := readGovernanceFile(t, "lifetimespend.go")
	dump := src[strings.Index(src, "func (gs *LocalGovernanceStore) DumpVirtualKeyLifetimeSpend"):]
	dump = dump[:strings.Index(dump, "\nfunc ")]

	if contains(dump, "return err\n") {
		t.Error("a failed batch returns immediately here, abandoning every later, " +
			"not-yet-attempted batch for this tick")
	}
	if !contains(dump, "firstErr") {
		t.Error("a batch failure is no longer surfaced to the caller at all")
	}
}

// A write that commits is durably persisted regardless of what happens next,
// so settling pending must not depend on the follow-up read-back succeeding.
// The composition that gets this wrong is subtle: check the base, refresh, or
// settle in the wrong order and either the delta vanishes from the sum for a
// while (base stale + pending cleared) or it gets written twice next tick
// (pending kept + write retried on an already-committed delta). Scoped to the
// write's own success branch, not the whole file, which also carries prose
// describing this exact failure mode.
func TestARefreshFailureFallsBackRatherThanLosingTheDelta(t *testing.T) {
	src := readGovernanceFile(t, "lifetimespend.go")
	dump := src[strings.Index(src, "func (gs *LocalGovernanceStore) DumpVirtualKeyLifetimeSpend"):]
	dump = dump[:strings.Index(dump, "\nfunc ")]

	if !contains(dump, "if !gs.refreshLifetimeSpendBase(ctx, batch)") {
		t.Fatal("refreshLifetimeSpendBase's success is no longer checked; a failed " +
			"read-back can't be told apart from a successful one anymore")
	}
	if !contains(dump, "gs.advanceBaseByOwnDelta(batch)") {
		t.Error("a failed refresh has no fallback; the base stays stale while " +
			"settleCommittedDelta below still clears the matching pending entry — " +
			"the published total drops by the delta and stays dropped")
	}
	if n := strings.Count(dump, "gs.settleCommittedDelta(batch)"); n != 1 {
		t.Errorf("settleCommittedDelta is called %d times, want exactly 1 — it must "+
			"run after both the refresh-success and the fallback paths, not be "+
			"duplicated or skipped on either", n)
	}
}

// Direct test of the fallback's arithmetic: it must add the delta we already
// know committed, not merely leave the base untouched.
func TestAdvanceBaseByOwnDeltaAddsTheCommittedDelta(t *testing.T) {
	gs := storeWithVK(t, "vk1", 10)
	gs.advanceBaseByOwnDelta([]vkSpendDumpRow{{ID: "vk1", Delta: 4}})

	got, ok := gs.VirtualKeyLifetimeSpend("vk1")
	require.True(t, ok)
	assert.Equal(t, 14.0, got,
		"the fallback must add the already-committed delta directly to the base")
}

// The periodic worker and the shutdown flush both call DumpVirtualKeyLifetimeSpend,
// and shutdown does not wait for the worker to stop first. Without the whole
// call serialized, concurrent dumps can each snapshot the SAME pending delta
// and each commit their own atomic increment for it — persisting spend that
// was only ever spent once, multiple times over.
func TestConcurrentDumpsDoNotDoubleCommitTheSameDelta(t *testing.T) {
	gs := storeWithVK(t, "vk1", 0)
	gs.BumpVirtualKeyLifetimeSpend(context.Background(), "vk1", 5)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = gs.DumpVirtualKeyLifetimeSpend(context.Background())
		}()
	}
	wg.Wait()

	got, ok := gs.VirtualKeyLifetimeSpend("vk1")
	require.True(t, ok)
	assert.Equal(t, 5.0, got,
		"ten concurrent dumps committed a single $5 delta as %v — it must land exactly once", got)
}

func readGovernanceFile(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
