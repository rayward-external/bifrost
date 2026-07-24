package configstore

import (
	"context"
	"testing"

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
