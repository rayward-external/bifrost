package configstore

import (
	"context"
	"testing"
	"time"

	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRDBConfigStore_RegisterAndGetManagedFile(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.RegisterManagedFile(ctx, &tables.TableManagedFile{
		Provider:          "openai",
		FileID:            "file-abc",
		OwnerVirtualKeyID: "vk-1",
		OwnerTeamID:       "team-1",
		OwnerCustomerID:   "cust-1",
		OwnerUserID:       "user-1",
		Purpose:           "batch",
		SourceBatchID:     "batch_1",
	}))

	got, err := store.GetManagedFile(ctx, "openai", "file-abc")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "vk-1", got.OwnerVirtualKeyID)
	assert.Equal(t, "team-1", got.OwnerTeamID)
	assert.Equal(t, "cust-1", got.OwnerCustomerID)
	assert.Equal(t, "batch", got.Purpose)
	assert.Equal(t, "batch_1", got.SourceBatchID)
	assert.NotEmpty(t, got.ID, "id should be auto-assigned")
	assert.False(t, got.CreatedAt.IsZero(), "created_at should be auto-set")
}

func TestRDBConfigStore_GetManagedFileMissingReturnsNil(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	got, err := store.GetManagedFile(ctx, "openai", "does-not-exist")
	require.NoError(t, err)
	assert.Nil(t, got)

	// Empty inputs are a no-op, not an error.
	got, err = store.GetManagedFile(ctx, "", "")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestRDBConfigStore_RegisterManagedFileIsIdempotentAndNeverReassigns(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.RegisterManagedFile(ctx, &tables.TableManagedFile{
		Provider:          "openai",
		FileID:            "file-1",
		OwnerVirtualKeyID: "vk-owner",
	}))

	// A second capture of the same (provider, file_id) — even by a different owner
	// (e.g. batch-output re-link) — must NOT overwrite the first writer.
	require.NoError(t, store.RegisterManagedFile(ctx, &tables.TableManagedFile{
		Provider:          "openai",
		FileID:            "file-1",
		OwnerVirtualKeyID: "vk-attacker",
	}))

	got, err := store.GetManagedFile(ctx, "openai", "file-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "vk-owner", got.OwnerVirtualKeyID, "ownership must remain with the first writer")

	// Same file id under a different provider is a distinct row.
	require.NoError(t, store.RegisterManagedFile(ctx, &tables.TableManagedFile{
		Provider:          "anthropic",
		FileID:            "file-1",
		OwnerVirtualKeyID: "vk-other",
	}))
	got, err = store.GetManagedFile(ctx, "anthropic", "file-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "vk-other", got.OwnerVirtualKeyID)
}

func TestRDBConfigStore_RegisterManagedFileValidatesInputs(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	assert.Error(t, store.RegisterManagedFile(ctx, nil))
	assert.Error(t, store.RegisterManagedFile(ctx, &tables.TableManagedFile{FileID: "f", OwnerVirtualKeyID: "vk"}))
	assert.Error(t, store.RegisterManagedFile(ctx, &tables.TableManagedFile{Provider: "p", OwnerVirtualKeyID: "vk"}))
	assert.Error(t, store.RegisterManagedFile(ctx, &tables.TableManagedFile{Provider: "p", FileID: "f"}))
}

// filePageIDs extracts the ordered file ids from a keyset page.
func filePageIDs(rows []tables.TableManagedFile) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.FileID)
	}
	return ids
}

func TestRDBConfigStore_ListOwnedFileIDsPage_NewestFirstKeyset(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Five owned rows (ascending created_at) interleaved with a foreign row and an
	// other-provider row that must never surface. The page walks NEWEST-FIRST.
	seed := []*tables.TableManagedFile{
		{Provider: "openai", FileID: "f1", OwnerVirtualKeyID: "vk-1", CreatedAt: base.Add(1 * time.Minute)},
		{Provider: "openai", FileID: "f2", OwnerVirtualKeyID: "vk-1", CreatedAt: base.Add(2 * time.Minute)},
		{Provider: "openai", FileID: "f3", OwnerVirtualKeyID: "vk-1", CreatedAt: base.Add(3 * time.Minute)},
		{Provider: "openai", FileID: "f4", OwnerVirtualKeyID: "vk-1", CreatedAt: base.Add(4 * time.Minute)},
		{Provider: "openai", FileID: "f5", OwnerVirtualKeyID: "vk-1", CreatedAt: base.Add(5 * time.Minute)},
		{Provider: "openai", FileID: "foreign", OwnerVirtualKeyID: "vk-foreign", CreatedAt: base.Add(90 * time.Second)},
		{Provider: "anthropic", FileID: "other", OwnerVirtualKeyID: "vk-1", CreatedAt: base.Add(3 * time.Minute)},
	}
	for _, s := range seed {
		require.NoError(t, store.RegisterManagedFile(ctx, s))
	}
	owner := tables.ManagedBatchOwner{VirtualKeyID: "vk-1"}

	// Page 1 (no cursor), limit 2 — newest first: f5, f4.
	rows, hasMore, err := store.ListOwnedFileIDsPage(ctx, "openai", owner, time.Time{}, "", false, false, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"f5", "f4"}, filePageIDs(rows))
	assert.True(t, hasMore)

	// Page 2 (after f4), limit 2 — f3, f2.
	rows, hasMore, err = store.ListOwnedFileIDsPage(ctx, "openai", owner, rows[len(rows)-1].CreatedAt, "f4", true, false, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"f3", "f2"}, filePageIDs(rows))
	assert.True(t, hasMore)

	// Page 3 (after f2), limit 2 — exactly one left (f1), has_more false, no foreign id.
	rows, hasMore, err = store.ListOwnedFileIDsPage(ctx, "openai", owner, rows[len(rows)-1].CreatedAt, "f2", true, false, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"f1"}, filePageIDs(rows))
	assert.False(t, hasMore)
}

// TestRDBConfigStore_ListOwnedFileIDsPage_OrderAsc proves order=asc walks the SAME
// owned set OLDEST-FIRST with a matching forward (seek-newer) cursor.
func TestRDBConfigStore_ListOwnedFileIDsPage_OrderAsc(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seed := []*tables.TableManagedFile{
		{Provider: "openai", FileID: "f1", OwnerVirtualKeyID: "vk-1", CreatedAt: base.Add(1 * time.Minute)},
		{Provider: "openai", FileID: "f2", OwnerVirtualKeyID: "vk-1", CreatedAt: base.Add(2 * time.Minute)},
		{Provider: "openai", FileID: "f3", OwnerVirtualKeyID: "vk-1", CreatedAt: base.Add(3 * time.Minute)},
		{Provider: "openai", FileID: "foreign", OwnerVirtualKeyID: "vk-foreign", CreatedAt: base.Add(90 * time.Second)},
	}
	for _, s := range seed {
		require.NoError(t, store.RegisterManagedFile(ctx, s))
	}
	owner := tables.ManagedBatchOwner{VirtualKeyID: "vk-1"}

	// Page 1 asc (no cursor), limit 2 — oldest first: f1, f2.
	rows, hasMore, err := store.ListOwnedFileIDsPage(ctx, "openai", owner, time.Time{}, "", false, true, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"f1", "f2"}, filePageIDs(rows))
	assert.True(t, hasMore)

	// Page 2 asc (after f2), limit 2 — seek NEWER than f2: only f3 remains.
	rows, hasMore, err = store.ListOwnedFileIDsPage(ctx, "openai", owner, rows[len(rows)-1].CreatedAt, "f2", true, true, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"f3"}, filePageIDs(rows))
	assert.False(t, hasMore)
	assert.NotContains(t, filePageIDs(rows), "foreign")
}

// TestRDBConfigStore_SetFileSourceBatch proves batch provenance is stamped only on
// an owner-matched EXISTING row, never minting or re-assigning.
func TestRDBConfigStore_SetFileSourceBatch(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.RegisterManagedFile(ctx, &tables.TableManagedFile{
		Provider: "openai", FileID: "file-input", OwnerVirtualKeyID: "vk-owner", OwnerTeamID: "team-1",
	}))

	// A caller in the owner's team stamps provenance on the existing row.
	linked, err := store.SetFileSourceBatch(ctx, "openai", "file-input", "batch_1",
		tables.ManagedBatchOwner{VirtualKeyID: "vk-teammate", TeamID: "team-1"})
	require.NoError(t, err)
	assert.True(t, linked)
	got, err := store.GetManagedFile(ctx, "openai", "file-input")
	require.NoError(t, err)
	assert.Equal(t, "batch_1", got.SourceBatchID)
	assert.Equal(t, "vk-owner", got.OwnerVirtualKeyID, "ownership must be unchanged")

	// A foreign caller cannot stamp (and thus cannot backdoor a link onto) the row.
	linked, err = store.SetFileSourceBatch(ctx, "openai", "file-input", "batch_evil",
		tables.ManagedBatchOwner{VirtualKeyID: "vk-foreign"})
	require.NoError(t, err)
	assert.False(t, linked)
	got, _ = store.GetManagedFile(ctx, "openai", "file-input")
	assert.Equal(t, "batch_1", got.SourceBatchID, "foreign provenance must not overwrite")

	// An absent file id never creates a row.
	linked, err = store.SetFileSourceBatch(ctx, "openai", "file-absent", "batch_1",
		tables.ManagedBatchOwner{VirtualKeyID: "vk-owner"})
	require.NoError(t, err)
	assert.False(t, linked)
	got, err = store.GetManagedFile(ctx, "openai", "file-absent")
	require.NoError(t, err)
	assert.Nil(t, got, "SetFileSourceBatch must never mint a row")
}

func TestRDBConfigStore_ListOwnedFileIDsPage_OwnershipAndTieBreak(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) // identical created_at for the tie-break rows

	// Ownership fan-out: VK, shared team, shared customer — plus a foreign row.
	require.NoError(t, store.RegisterManagedFile(ctx, &tables.TableManagedFile{Provider: "openai", FileID: "fa", OwnerVirtualKeyID: "vk-1", CreatedAt: ts}))
	require.NoError(t, store.RegisterManagedFile(ctx, &tables.TableManagedFile{Provider: "openai", FileID: "fb", OwnerVirtualKeyID: "vk-2", OwnerTeamID: "team-1", CreatedAt: ts}))
	require.NoError(t, store.RegisterManagedFile(ctx, &tables.TableManagedFile{Provider: "openai", FileID: "fc", OwnerVirtualKeyID: "vk-3", OwnerCustomerID: "cust-1", CreatedAt: ts}))
	require.NoError(t, store.RegisterManagedFile(ctx, &tables.TableManagedFile{Provider: "openai", FileID: "ff", OwnerVirtualKeyID: "vk-9", CreatedAt: ts}))

	owner := tables.ManagedBatchOwner{VirtualKeyID: "vk-1", TeamID: "team-1", CustomerID: "cust-1"}

	// All three owned rows, none foreign; with equal created_at the file_id
	// tie-breaker orders newest-first as fc>fb>fa.
	rows, hasMore, err := store.ListOwnedFileIDsPage(ctx, "openai", owner, time.Time{}, "", false, false, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"fc", "fb", "fa"}, filePageIDs(rows))
	assert.False(t, hasMore)

	// Seek past fb at the same timestamp — the tuple seek advances to the older fa.
	rows, hasMore, err = store.ListOwnedFileIDsPage(ctx, "openai", owner, ts, "fb", true, false, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"fa"}, filePageIDs(rows))
	assert.False(t, hasMore)

	// Empty owner VK id yields an empty (non-nil) slice.
	rows, _, err = store.ListOwnedFileIDsPage(ctx, "openai", tables.ManagedBatchOwner{}, time.Time{}, "", false, false, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{}, filePageIDs(rows))
}
