package configstore

import (
	"context"
	"testing"
	"time"

	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRDBConfigStore_RegisterAndGetManagedBatch(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.RegisterManagedBatch(ctx, &tables.TableManagedBatch{
		Provider:          "anthropic",
		BatchID:           "msgbatch_abc",
		OwnerVirtualKeyID: "vk-1",
		OwnerTeamID:       "team-1",
		OwnerCustomerID:   "cust-1",
		OwnerUserID:       "user-1",
	}))

	got, err := store.GetManagedBatch(ctx, "anthropic", "msgbatch_abc")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "vk-1", got.OwnerVirtualKeyID)
	assert.Equal(t, "team-1", got.OwnerTeamID)
	assert.Equal(t, "cust-1", got.OwnerCustomerID)
	assert.NotEmpty(t, got.ID, "id should be auto-assigned")
	assert.False(t, got.CreatedAt.IsZero(), "created_at should be auto-set")
}

func TestRDBConfigStore_GetManagedBatchMissingReturnsNil(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	got, err := store.GetManagedBatch(ctx, "anthropic", "does-not-exist")
	require.NoError(t, err)
	assert.Nil(t, got)

	// Empty inputs are a no-op, not an error.
	got, err = store.GetManagedBatch(ctx, "", "")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestRDBConfigStore_RegisterManagedBatchIsIdempotentAndNeverReassigns(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.RegisterManagedBatch(ctx, &tables.TableManagedBatch{
		Provider:          "openai",
		BatchID:           "batch_1",
		OwnerVirtualKeyID: "vk-owner",
	}))

	// A second capture of the same (provider, batch_id) — even by a different
	// owner — must NOT overwrite the first writer's ownership.
	require.NoError(t, store.RegisterManagedBatch(ctx, &tables.TableManagedBatch{
		Provider:          "openai",
		BatchID:           "batch_1",
		OwnerVirtualKeyID: "vk-attacker",
	}))

	got, err := store.GetManagedBatch(ctx, "openai", "batch_1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "vk-owner", got.OwnerVirtualKeyID, "ownership must remain with the first writer")

	// Same batch id under a different provider is a distinct row.
	require.NoError(t, store.RegisterManagedBatch(ctx, &tables.TableManagedBatch{
		Provider:          "anthropic",
		BatchID:           "batch_1",
		OwnerVirtualKeyID: "vk-other",
	}))
	got, err = store.GetManagedBatch(ctx, "anthropic", "batch_1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "vk-other", got.OwnerVirtualKeyID)
}

func TestRDBConfigStore_RegisterManagedBatchValidatesInputs(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	assert.Error(t, store.RegisterManagedBatch(ctx, nil))
	assert.Error(t, store.RegisterManagedBatch(ctx, &tables.TableManagedBatch{BatchID: "b", OwnerVirtualKeyID: "vk"}))
	assert.Error(t, store.RegisterManagedBatch(ctx, &tables.TableManagedBatch{Provider: "p", OwnerVirtualKeyID: "vk"}))
	assert.Error(t, store.RegisterManagedBatch(ctx, &tables.TableManagedBatch{Provider: "p", BatchID: "b"}))
}

func TestRDBConfigStore_ListOwnedBatchIDs(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	seed := []*tables.TableManagedBatch{
		{Provider: "anthropic", BatchID: "b-vk", OwnerVirtualKeyID: "vk-1"},
		{Provider: "anthropic", BatchID: "b-team", OwnerVirtualKeyID: "vk-2", OwnerTeamID: "team-1"},
		{Provider: "anthropic", BatchID: "b-cust", OwnerVirtualKeyID: "vk-3", OwnerCustomerID: "cust-1"},
		{Provider: "anthropic", BatchID: "b-foreign", OwnerVirtualKeyID: "vk-9", OwnerTeamID: "team-9", OwnerCustomerID: "cust-9"},
		{Provider: "openai", BatchID: "b-otherprovider", OwnerVirtualKeyID: "vk-1"},
	}
	for _, s := range seed {
		require.NoError(t, store.RegisterManagedBatch(ctx, s))
	}

	// Match by VK id, shared team, and shared customer — but never the foreign row.
	ids, err := store.ListOwnedBatchIDs(ctx, "anthropic", tables.ManagedBatchOwner{
		VirtualKeyID: "vk-1",
		TeamID:       "team-1",
		CustomerID:   "cust-1",
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"b-vk", "b-team", "b-cust"}, ids)

	// Provider isolation: the openai row never shows up under anthropic.
	assert.NotContains(t, ids, "b-otherprovider")

	// A team-less, customer-less caller matches only its own VK's batches. vk-2
	// is the owner VK of b-team, so it matches by VK id — but a caller sharing
	// only team-1 (a different VK) still would not reach vk-1/vk-3's rows.
	ids, err = store.ListOwnedBatchIDs(ctx, "anthropic", tables.ManagedBatchOwner{VirtualKeyID: "vk-2"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"b-team"}, ids, "vk-2 owns b-team by VK id")

	// A caller that shares only team-1 (via a different VK, no customer) reaches
	// exactly the team-1 row.
	ids, err = store.ListOwnedBatchIDs(ctx, "anthropic", tables.ManagedBatchOwner{VirtualKeyID: "vk-outsider", TeamID: "team-1"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"b-team"}, ids)

	// Empty owner VK id yields an empty (non-nil) slice.
	ids, err = store.ListOwnedBatchIDs(ctx, "anthropic", tables.ManagedBatchOwner{})
	require.NoError(t, err)
	assert.Equal(t, []string{}, ids)
}

// pageIDs extracts the ordered batch ids from a keyset page.
func pageIDs(rows []tables.TableManagedBatch) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.BatchID)
	}
	return ids
}

func TestRDBConfigStore_ListOwnedBatchIDsPage_ForwardKeyset(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Five owned rows (ascending created_at) interleaved with a foreign row and an
	// other-provider row that must never surface.
	seed := []*tables.TableManagedBatch{
		{Provider: "anthropic", BatchID: "b1", OwnerVirtualKeyID: "vk-1", CreatedAt: base.Add(1 * time.Minute)},
		{Provider: "anthropic", BatchID: "b2", OwnerVirtualKeyID: "vk-1", CreatedAt: base.Add(2 * time.Minute)},
		{Provider: "anthropic", BatchID: "b3", OwnerVirtualKeyID: "vk-1", CreatedAt: base.Add(3 * time.Minute)},
		{Provider: "anthropic", BatchID: "b4", OwnerVirtualKeyID: "vk-1", CreatedAt: base.Add(4 * time.Minute)},
		{Provider: "anthropic", BatchID: "b5", OwnerVirtualKeyID: "vk-1", CreatedAt: base.Add(5 * time.Minute)},
		{Provider: "anthropic", BatchID: "f1", OwnerVirtualKeyID: "vk-foreign", CreatedAt: base.Add(90 * time.Second)},
		{Provider: "openai", BatchID: "o1", OwnerVirtualKeyID: "vk-1", CreatedAt: base.Add(3 * time.Minute)},
	}
	for _, s := range seed {
		require.NoError(t, store.RegisterManagedBatch(ctx, s))
	}
	owner := tables.ManagedBatchOwner{VirtualKeyID: "vk-1"}

	// Newest-first: page 1 (no cursor), limit 2 => the two NEWEST (b5, b4).
	rows, hasMore, err := store.ListOwnedBatchIDsPage(ctx, "anthropic", owner, time.Time{}, "", false, false, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"b5", "b4"}, pageIDs(rows))
	assert.True(t, hasMore)

	// Page 2 (after b4 => older), limit 2 => b3, b2.
	rows, hasMore, err = store.ListOwnedBatchIDsPage(ctx, "anthropic", owner, rows[len(rows)-1].CreatedAt, "b4", true, false, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"b3", "b2"}, pageIDs(rows))
	assert.True(t, hasMore)

	// Page 3 (after b2 => older), limit 2 — exactly one older left (b1), no foreign id.
	rows, hasMore, err = store.ListOwnedBatchIDsPage(ctx, "anthropic", owner, rows[len(rows)-1].CreatedAt, "b2", true, false, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"b1"}, pageIDs(rows))
	assert.False(t, hasMore)
}

func TestRDBConfigStore_ListOwnedBatchIDsPage_TieBreakOnBatchID(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) // identical created_at for all three

	for _, id := range []string{"bc", "ba", "bb"} {
		require.NoError(t, store.RegisterManagedBatch(ctx, &tables.TableManagedBatch{
			Provider: "anthropic", BatchID: id, OwnerVirtualKeyID: "vk-1", CreatedAt: ts,
		}))
	}
	owner := tables.ManagedBatchOwner{VirtualKeyID: "vk-1"}

	// With equal created_at, the batch_id tie-breaker orders newest-first DESC
	// (bc>bb>ba), so page 1 is [bc, bb].
	rows, hasMore, err := store.ListOwnedBatchIDsPage(ctx, "anthropic", owner, time.Time{}, "", false, false, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"bc", "bb"}, pageIDs(rows))
	assert.True(t, hasMore)

	// Seek past bb (forward => older) at the same timestamp — the tuple seek must
	// advance to ba (batch_id < bb).
	rows, hasMore, err = store.ListOwnedBatchIDsPage(ctx, "anthropic", owner, ts, "bb", true, false, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"ba"}, pageIDs(rows))
	assert.False(t, hasMore)
}

func TestRDBConfigStore_ListOwnedBatchIDsPage_BackwardKeyset(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i, id := range []string{"b1", "b2", "b3", "b4", "b5"} {
		require.NoError(t, store.RegisterManagedBatch(ctx, &tables.TableManagedBatch{
			Provider: "anthropic", BatchID: id, OwnerVirtualKeyID: "vk-1", CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}))
	}
	owner := tables.ManagedBatchOwner{VirtualKeyID: "vk-1"}
	b2, err := store.GetManagedBatch(ctx, "anthropic", "b2")
	require.NoError(t, err)

	// before b2, limit 2 => the two rows just NEWER than b2 (closest first),
	// returned newest-first: [b4, b3]; b5 (even newer) still remains.
	rows, hasMore, err := store.ListOwnedBatchIDsPage(ctx, "anthropic", owner, b2.CreatedAt, "b2", true, true, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"b4", "b3"}, pageIDs(rows))
	assert.True(t, hasMore, "b5 (newer) still remains beyond the page")
}

func TestRDBConfigStore_ListOwnedBatchIDsPage_OwnerORAndIsolation(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seed := []*tables.TableManagedBatch{
		{Provider: "anthropic", BatchID: "b-vk", OwnerVirtualKeyID: "vk-1", CreatedAt: base.Add(1 * time.Minute)},
		{Provider: "anthropic", BatchID: "b-team", OwnerVirtualKeyID: "vk-2", OwnerTeamID: "team-1", CreatedAt: base.Add(2 * time.Minute)},
		{Provider: "anthropic", BatchID: "b-cust", OwnerVirtualKeyID: "vk-3", OwnerCustomerID: "cust-1", CreatedAt: base.Add(3 * time.Minute)},
		{Provider: "anthropic", BatchID: "b-foreign", OwnerVirtualKeyID: "vk-9", OwnerTeamID: "team-9", CreatedAt: base.Add(4 * time.Minute)},
	}
	for _, s := range seed {
		require.NoError(t, store.RegisterManagedBatch(ctx, s))
	}

	// OR across VK / team / customer, newest-first, foreign excluded, single page.
	rows, hasMore, err := store.ListOwnedBatchIDsPage(ctx, "anthropic",
		tables.ManagedBatchOwner{VirtualKeyID: "vk-1", TeamID: "team-1", CustomerID: "cust-1"},
		time.Time{}, "", false, false, 100)
	require.NoError(t, err)
	assert.Equal(t, []string{"b-cust", "b-team", "b-vk"}, pageIDs(rows))
	assert.False(t, hasMore)

	// Empty owner VK id yields an empty (non-nil) page.
	rows, hasMore, err = store.ListOwnedBatchIDsPage(ctx, "anthropic", tables.ManagedBatchOwner{}, time.Time{}, "", false, false, 10)
	require.NoError(t, err)
	assert.Equal(t, []tables.TableManagedBatch{}, rows)
	assert.False(t, hasMore)
}

func TestRDBConfigStore_ListOwnedBatchIDsEmptyOwnerColumnsDoNotWildcardMatch(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Row owned by a team-less/customer-less key.
	require.NoError(t, store.RegisterManagedBatch(ctx, &tables.TableManagedBatch{
		Provider:          "anthropic",
		BatchID:           "b-lonely",
		OwnerVirtualKeyID: "vk-lonely",
	}))

	// A different caller that also has no team/customer must NOT match the row
	// on the empty team/customer columns.
	ids, err := store.ListOwnedBatchIDs(ctx, "anthropic", tables.ManagedBatchOwner{VirtualKeyID: "vk-different"})
	require.NoError(t, err)
	assert.Empty(t, ids)
}
