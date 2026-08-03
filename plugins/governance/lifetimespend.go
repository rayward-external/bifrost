package governance

// Per-virtual-key LIFETIME spend: the cumulative dollar total a key has ever
// cost, never reset by any window rollover.
//
// WHY IT CANNOT COME FROM THE BUDGETS
//
// Every budget here carries a CurrentUsage the sweep zeroes each cycle, so no
// combination of them answers "how much has this key ever cost". The figure is
// published to callers as `x-usage-spend`, whose documented contract is a number
// that only ever goes up and never resets.
//
// WHY IT IS A PERSISTED COLUMN AND NOT SOMETHING CHEAPER
//
// Two cheaper designs were considered and both violate that contract:
//
//   - an in-memory counter resets to zero on every process restart;
//   - reading the log-store aggregate (mv_logs_hourly already sums cost by
//     virtual_key_id) lags its matview refresh, so the value can tick DOWNWARDS
//     after a restart, and the log store is retention-bounded so its "total" is
//     really "total since the oldest surviving row".
//
// A number that decreases under a name promising it cannot is worse than no
// number, which is why this gateway published nothing at all until now.
//
// HOW IT STAYS CORRECT ACROSS A CLUSTER
//
// Same delta-folded dump discipline as budget usage: each node accumulates
// in-memory, folds in its view of remote usage at dump time, and writes. On top
// of that the write carries a MONOTONIC GUARD in SQL — a row is only updated
// when the incoming value is greater than the stored one. That guard is what
// makes the property hold under races rather than under assumption: a node that
// has been partitioned and is carrying a stale, lower total cannot lower a
// higher value another node already persisted. Ordering in memory cannot
// guarantee that; a WHERE clause can.

import (
	"context"
	"fmt"
	"sort"

	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
)

// vkSpendDumpRow is one virtual key's cumulative spend, ready to persist.
type vkSpendDumpRow struct {
	ID            string
	LifetimeSpend float64
}

// BumpVirtualKeyLifetimeSpend adds this request's cost to the key's cumulative
// total, in memory. Persistence happens on the periodic dump.
//
// Negative and non-finite costs are refused rather than clamped. A negative cost
// would make a header documented as monotonic go down, and a NaN would poison the
// total permanently — every subsequent comparison against NaN is false, so the
// monotonic guard would silently stop updating the row forever.
func (gs *LocalGovernanceStore) BumpVirtualKeyLifetimeSpend(ctx context.Context, vkID string, cost float64) {
	if vkID == "" || !(cost > 0) || cost > 1e308 {
		// `!(cost > 0)` rather than `cost <= 0` so NaN is excluded too: every
		// comparison with NaN is false, so `NaN <= 0` would let it through.
		return
	}
	raw, exists := gs.virtualKeysByID.Load(vkID)
	if !exists || raw == nil {
		return
	}
	vk, ok := raw.(*configstoreTables.TableVirtualKey)
	if !ok || vk == nil {
		return
	}

	gs.LifetimeSpendMu.Lock()
	gs.PendingVKLifetimeSpend[vkID] += cost
	gs.LifetimeSpendMu.Unlock()
}

// VirtualKeyLifetimeSpend returns the key's cumulative spend as this node
// currently understands it: what was loaded from the row plus whatever this node
// has accumulated since and not yet dumped.
//
// The pending delta is added rather than ignored so the figure a caller reads
// includes the request they just made — the same freshness property the budget
// windows have. Without it the header would lag the dump interval and appear
// stuck across consecutive calls.
func (gs *LocalGovernanceStore) VirtualKeyLifetimeSpend(vkID string) (float64, bool) {
	if vkID == "" {
		return 0, false
	}
	raw, exists := gs.virtualKeysByID.Load(vkID)
	if !exists || raw == nil {
		return 0, false
	}
	vk, ok := raw.(*configstoreTables.TableVirtualKey)
	if !ok || vk == nil {
		return 0, false
	}

	gs.LifetimeSpendMu.RLock()
	pending := gs.PendingVKLifetimeSpend[vkID]
	gs.LifetimeSpendMu.RUnlock()

	return vk.LifetimeSpend + pending, true
}

// DumpVirtualKeyLifetimeSpend persists every key's accumulated spend.
//
// Mirrors DumpBudgets: stable ID order so concurrent dumpers take row locks in
// the same sequence (deadlock-free by construction rather than by luck), and
// batched writes so locks are released between chunks.
//
// The pending delta is drained BEFORE the write and folded back on failure. The
// alternative — clearing after a successful write — loses every increment that
// arrived during the write. Folding back on failure can double-count only if the
// write actually committed and then reported an error, and the monotonic guard
// makes that case a no-op rather than an inflation.
func (gs *LocalGovernanceStore) DumpVirtualKeyLifetimeSpend(ctx context.Context) error {
	if gs.configStore == nil {
		return nil
	}

	gs.LifetimeSpendMu.Lock()
	if len(gs.PendingVKLifetimeSpend) == 0 {
		gs.LifetimeSpendMu.Unlock()
		return nil
	}
	drained := gs.PendingVKLifetimeSpend
	gs.PendingVKLifetimeSpend = make(map[string]float64, len(drained))
	gs.LifetimeSpendMu.Unlock()

	rows := make([]vkSpendDumpRow, 0, len(drained))
	for vkID, delta := range drained {
		raw, exists := gs.virtualKeysByID.Load(vkID)
		if !exists || raw == nil {
			continue
		}
		vk, ok := raw.(*configstoreTables.TableVirtualKey)
		if !ok || vk == nil {
			continue
		}
		total := vk.LifetimeSpend + delta
		// Advance this node's own view so a reader between dumps does not see
		// the figure jump backwards when the pending map is cleared.
		vk.LifetimeSpend = total
		rows = append(rows, vkSpendDumpRow{ID: vkID, LifetimeSpend: total})
	}
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	for start := 0; start < len(rows); start += dumpBatchSize {
		end := min(start+dumpBatchSize, len(rows))
		batch := rows[start:end]
		if err := gs.configStore.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
			return gs.writeVKSpendBatch(ctx, tx, batch)
		}); err != nil {
			gs.foldBackLifetimeSpend(drained)
			return err
		}
	}
	return nil
}

// foldBackLifetimeSpend returns drained deltas to the pending map after a failed
// write, so a transient DB error costs a retry rather than the spend itself.
func (gs *LocalGovernanceStore) foldBackLifetimeSpend(drained map[string]float64) {
	gs.LifetimeSpendMu.Lock()
	defer gs.LifetimeSpendMu.Unlock()
	for vkID, delta := range drained {
		gs.PendingVKLifetimeSpend[vkID] += delta
	}
}

// writeVKSpendBatch persists one chunk under the monotonic guard.
//
// `WHERE lifetime_spend < ?` is the whole safety argument. A node that fell
// behind — partitioned, slow, or restarted with a stale in-memory view — computes
// a total lower than what a peer already wrote. Without the guard it would
// overwrite and the published figure would go DOWN, breaking the one property
// `x-usage-spend` promises. With it, the stale write matches no row and is
// discarded.
//
// Row-at-a-time rather than a batched CASE expression: the guard has to be
// evaluated per row against that row's stored value, and correctness here is
// worth more than a round trip on a table with one row per key.
func (gs *LocalGovernanceStore) writeVKSpendBatch(ctx context.Context, tx *gorm.DB, batch []vkSpendDumpRow) error {
	for _, row := range batch {
		if err := tx.WithContext(ctx).
			Session(&gorm.Session{SkipHooks: true}).
			Model(&configstoreTables.TableVirtualKey{}).
			Where("id = ? AND lifetime_spend < ?", row.ID, row.LifetimeSpend).
			Update("lifetime_spend", row.LifetimeSpend).Error; err != nil {
			return fmt.Errorf("failed to update lifetime spend for virtual key %s: %w", row.ID, err)
		}
	}
	return nil
}
