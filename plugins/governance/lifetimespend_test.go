package governance

import (
	"context"
	"math"
	"os"
	"strings"
	"sync"
	"testing"

	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
)

// storeWithVK builds a store holding one virtual key at a known starting total.
func storeWithVK(t *testing.T, vkID string, starting float64) *LocalGovernanceStore {
	t.Helper()
	gs := &LocalGovernanceStore{
		PendingVKLifetimeSpend: make(map[string]float64),
		vkLifetimeSpendBase:    make(map[string]float64),
	}
	gs.virtualKeysByID.Store(vkID, &configstoreTables.TableVirtualKey{ID: vkID, LifetimeSpend: starting})
	gs.SeedVirtualKeyLifetimeSpend(vkID, starting)
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

// A failed dump must cost a retry, not the spend. This is why the drained map is
// folded back rather than discarded.
func TestAFailedDumpDoesNotLoseTheSpend(t *testing.T) {
	gs := storeWithVK(t, "vk1", 10)
	gs.BumpVirtualKeyLifetimeSpend(context.Background(), "vk1", 3)

	gs.LifetimeSpendMu.Lock()
	gs.PendingVKLifetimeSpend = make(map[string]float64)
	gs.LifetimeSpendMu.Unlock()

	gs.foldBackLifetimeSpend([]vkSpendDumpRow{{ID: "vk1", Delta: 3}})

	if got := gs.PendingVKLifetimeSpend["vk1"]; got != 3 {
		t.Errorf("pending spend after fold-back = %v, want 3", got)
	}
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
	// configStore is nil here; if the function tried to write it would panic.
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

// A later batch failing must not requeue deltas an earlier batch already
// committed.
func TestFoldBackIsPerBatchNotPerDrain(t *testing.T) {
	src := readGovernanceFile(t, "lifetimespend.go")
	if !contains(src, "gs.foldBackLifetimeSpend(batch)") {
		t.Error("fold-back is no longer scoped to the failing batch; a late failure " +
			"would requeue already-persisted spend and double-count it")
	}
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
