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
// HOW IT STAYS CORRECT ACROSS A CLUSTER — AND THE DESIGN THAT DID NOT
//
// The write is an ATOMIC INCREMENT: `SET lifetime_spend = lifetime_spend + ?`.
// Each node contributes only its own delta and the database adds them, so two
// nodes accruing 5 and 7 leave 12.
//
// The first version of this file did something else, and it was wrong in a way
// worth recording because it looked safer. It computed an absolute total per
// node and wrote it under a guard, `WHERE lifetime_spend < ?`, reasoning that a
// stale node could then never lower a higher value. It cannot — but two healthy
// nodes each computing an absolute total from the same starting point produce
// 5 and 7, and the guard keeps 7. The cluster silently loses 5. A "monotonic"
// guard turns concurrent contributions into a MAXIMUM, and a maximum is not a
// sum. Monotonicity here is a property of only ever adding positive deltas,
// which BumpVirtualKeyLifetimeSpend enforces at the source; it does not need
// and must not use a comparison guard.

import (
	"context"
	"fmt"
	"sort"

	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
)

// vkSpendDumpRow is one virtual key's un-persisted delta.
//
// A DELTA, not a total: the write adds it to whatever the row already holds, so
// what this node believes the total to be never enters the SQL.
type vkSpendDumpRow struct {
	ID    string
	Delta float64
}

// BumpVirtualKeyLifetimeSpend adds this request's cost to the key's cumulative
// total, in memory. Persistence happens on the periodic dump.
//
// Negative and non-finite costs are refused rather than clamped. A negative cost
// would make a header documented as monotonic go down; +Inf would saturate the
// total permanently; and NaN would poison it beyond recovery, since NaN
// propagates through every subsequent addition.
//
// This refusal is also what makes monotonicity hold at all — the increment below
// has no comparison guard, so "only goes up" is exactly the guarantee that every
// delta reaching it is positive and finite.
func (gs *LocalGovernanceStore) BumpVirtualKeyLifetimeSpend(ctx context.Context, vkID string, cost float64) {
	// `!(cost > 0)` rather than `cost <= 0` so NaN is excluded too: every
	// comparison with NaN is false, so `NaN <= 0` would let it straight through.
	if vkID == "" || !(cost > 0) || cost > 1e308 {
		return
	}
	if _, exists := gs.virtualKeysByID.Load(vkID); !exists {
		return
	}

	gs.LifetimeSpendMu.Lock()
	gs.PendingVKLifetimeSpend[vkID] += cost
	gs.LifetimeSpendMu.Unlock()
}

// VirtualKeyLifetimeSpend returns the key's cumulative spend: the last value
// this node read back from the row, plus whatever it has accrued since and not
// yet persisted.
//
// The pending delta is included so the figure a caller reads covers the request
// they just made — the same freshness the budget windows have. Without it the
// header would lag the dump interval and look frozen across consecutive calls.
//
// Both halves are read under the SAME lock. The base is mutated by the dump
// goroutine, so reading it unsynchronised while a dump runs is a data race on
// every active key, and can hand a caller a torn value.
func (gs *LocalGovernanceStore) VirtualKeyLifetimeSpend(vkID string) (float64, bool) {
	if vkID == "" {
		return 0, false
	}
	if _, exists := gs.virtualKeysByID.Load(vkID); !exists {
		return 0, false
	}

	gs.LifetimeSpendMu.RLock()
	defer gs.LifetimeSpendMu.RUnlock()
	return gs.vkLifetimeSpendBase[vkID] + gs.PendingVKLifetimeSpend[vkID], true
}

// SeedVirtualKeyLifetimeSpend installs the persisted total for a key as this
// node's base. Called when a key is loaded or reloaded from the config store.
//
// Takes the LARGER of the incoming and current base: a reload that races a dump
// could otherwise deliver a row read before this node's increment landed and
// walk the published figure backwards.
func (gs *LocalGovernanceStore) SeedVirtualKeyLifetimeSpend(vkID string, persisted float64) {
	if vkID == "" || persisted < 0 {
		return
	}
	gs.LifetimeSpendMu.Lock()
	defer gs.LifetimeSpendMu.Unlock()
	if persisted > gs.vkLifetimeSpendBase[vkID] {
		gs.vkLifetimeSpendBase[vkID] = persisted
	}
}

// DumpVirtualKeyLifetimeSpend persists every key's accrued delta.
//
// Mirrors DumpBudgets: stable ID order so concurrent dumpers take row locks in
// the same sequence (deadlock-free by construction rather than by luck), and
// batched writes so locks are released between chunks.
//
// The in-memory base is advanced ONLY from a value read back after a confirmed
// write. An earlier version advanced it optimistically before the write and
// folded the delta back on failure, which double-counts: the base had already
// absorbed the delta, so the retry added it a second time.
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
		if delta <= 0 {
			continue
		}
		if _, exists := gs.virtualKeysByID.Load(vkID); !exists {
			continue
		}
		rows = append(rows, vkSpendDumpRow{ID: vkID, Delta: delta})
	}
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	// Folded back PER BATCH, not for the whole drain: a later batch failing must
	// not requeue deltas that earlier batches already committed.
	for start := 0; start < len(rows); start += dumpBatchSize {
		end := min(start+dumpBatchSize, len(rows))
		batch := rows[start:end]
		if err := gs.configStore.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
			return gs.writeVKSpendBatch(ctx, tx, batch)
		}); err != nil {
			gs.foldBackLifetimeSpend(batch)
			return err
		}
		gs.refreshLifetimeSpendBase(ctx, batch)
	}
	return nil
}

// foldBackLifetimeSpend returns a failed batch's deltas to the pending map, so a
// transient DB error costs a retry rather than the spend itself.
func (gs *LocalGovernanceStore) foldBackLifetimeSpend(batch []vkSpendDumpRow) {
	gs.LifetimeSpendMu.Lock()
	defer gs.LifetimeSpendMu.Unlock()
	for _, row := range batch {
		gs.PendingVKLifetimeSpend[row.ID] += row.Delta
	}
}

// refreshLifetimeSpendBase re-reads the authoritative totals for a committed
// batch and installs them as this node's base.
//
// Read back rather than added locally, because the row now includes every OTHER
// node's contributions too. Adding our own delta to a stale base would publish a
// figure that is correct only on a single-node deployment.
//
// Best-effort: a failed read leaves the base where it was, which understates
// until the next dump. Understating is the safe direction, and the spend is
// already persisted — nothing is lost.
func (gs *LocalGovernanceStore) refreshLifetimeSpendBase(ctx context.Context, batch []vkSpendDumpRow) {
	ids := make([]string, 0, len(batch))
	for _, row := range batch {
		ids = append(ids, row.ID)
	}

	type idSpend struct {
		ID            string
		LifetimeSpend float64
	}
	var fresh []idSpend
	if err := gs.configStore.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		return tx.WithContext(ctx).
			Model(&configstoreTables.TableVirtualKey{}).
			Select("id", "lifetime_spend").
			Where("id IN ?", ids).
			Find(&fresh).Error
	}); err != nil {
		gs.logger.Debug("could not refresh lifetime spend base: %v", err)
		return
	}

	gs.LifetimeSpendMu.Lock()
	defer gs.LifetimeSpendMu.Unlock()
	for _, row := range fresh {
		// Never downwards. A read that raced another node's write could return a
		// value below what we already published.
		if row.LifetimeSpend > gs.vkLifetimeSpendBase[row.ID] {
			gs.vkLifetimeSpendBase[row.ID] = row.LifetimeSpend
		}
	}
}

// writeVKSpendBatch adds each node-local delta to the stored total.
//
// `SET lifetime_spend = lifetime_spend + ?` — an ATOMIC INCREMENT, evaluated by
// the database against the row's current value. This is what makes concurrent
// nodes sum rather than overwrite, and it is why there is no comparison guard
// here: see the file header for the version that had one and lost spend.
//
// Row-at-a-time rather than a batched CASE expression: each row needs its own
// delta folded into its own current value, and this table holds one row per key.
func (gs *LocalGovernanceStore) writeVKSpendBatch(ctx context.Context, tx *gorm.DB, batch []vkSpendDumpRow) error {
	for _, row := range batch {
		if err := tx.WithContext(ctx).
			Session(&gorm.Session{SkipHooks: true}).
			Model(&configstoreTables.TableVirtualKey{}).
			Where("id = ?", row.ID).
			Update("lifetime_spend", gorm.Expr("lifetime_spend + ?", row.Delta)).Error; err != nil {
			return fmt.Errorf("failed to add lifetime spend for virtual key %s: %w", row.ID, err)
		}
	}
	return nil
}
