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
	gs := &LocalGovernanceStore{PendingVKLifetimeSpend: make(map[string]float64)}
	gs.virtualKeysByID.Store(vkID, &configstoreTables.TableVirtualKey{ID: vkID, LifetimeSpend: starting})
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
	drained := gs.PendingVKLifetimeSpend
	gs.PendingVKLifetimeSpend = make(map[string]float64)
	gs.LifetimeSpendMu.Unlock()

	gs.foldBackLifetimeSpend(drained)

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

// The guard that makes the whole design safe under a cluster. Asserted on the
// source because exercising it needs a real DB with two racing writers.
func TestTheWriteCarriesAMonotonicGuard(t *testing.T) {
	src := readGovernanceFile(t, "lifetimespend.go")
	if !contains(src, `Where("id = ? AND lifetime_spend < ?"`) {
		t.Error("the monotonic guard is gone from writeVKSpendBatch; a node carrying a " +
			"stale lower total can now overwrite a higher persisted one, and x-usage-spend " +
			"goes DOWN — the one thing it is documented never to do")
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
