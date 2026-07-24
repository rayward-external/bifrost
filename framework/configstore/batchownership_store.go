package configstore

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ManagedBatchStore is the batch-ownership ledger capability. It is deliberately
// NOT part of the public ConfigStore interface: adding it there would force every
// external SDK implementer of ConfigStore to implement three methods they may
// never use. Instead it is an OPTIONAL capability — the concrete RDB store
// implements it, and consumers (the governance plugin) obtain it by
// type-asserting their ConfigStore to this interface, degrading gracefully when
// the assertion fails.
//
// Batch lifecycle relays use shared upstream provider credentials with no
// per-tenant scoping, so the governance plugin records which tenant owns each
// batch created through the gateway (keyed by the upstream provider batch id)
// and consults it to scope list output and gate per-id lifecycle verbs. See
// tables.TableManagedBatch.
type ManagedBatchStore interface {
	// RegisterManagedBatch persists one ownership row; it is idempotent on the
	// (provider, batch_id) unique key (a repeat capture is a no-op and never
	// re-assigns ownership).
	RegisterManagedBatch(ctx context.Context, batch *tables.TableManagedBatch, tx ...*gorm.DB) error
	// GetManagedBatch returns the owner row for a (provider, batch_id) or
	// (nil, nil) when absent.
	GetManagedBatch(ctx context.Context, provider, batchID string, tx ...*gorm.DB) (*tables.TableManagedBatch, error)
	// ListOwnedBatchIDs returns the upstream batch ids for a provider that the
	// given owner tuple owns.
	ListOwnedBatchIDs(ctx context.Context, provider string, owner tables.ManagedBatchOwner, tx ...*gorm.DB) ([]string, error)
}

// ManagedBatchPager is a SEPARATE optional capability, layered on top of
// ManagedBatchStore, that adds keyset pagination over a caller's owned batches.
// It is deliberately NOT folded into ManagedBatchStore: doing so would break
// backward compatibility — an external config store that already implements the
// three-method ManagedBatchStore capability would silently stop satisfying it
// after this method was added, the governance plugin's type-assertion would fail,
// and per-tenant batch isolation would silently DISABLE on upgrade (a security
// regression). Keeping it separate lets a store implement ownership WITHOUT
// pagination; the plugin then falls back to an owned-id list (still leak-proof)
// rather than disabling enforcement.
type ManagedBatchPager interface {
	// ListOwnedBatchIDsPage returns ONE keyset page of the owner's batch rows for
	// a provider in REVERSE-CHRONOLOGICAL (newest-first) order — matching native
	// batch list APIs. It powers deep owner-scoped list pagination: the caller
	// pages through all of its own batches (not just the most recent upstream page)
	// while a foreign id can never appear, because only owner-matched rows are
	// returned.
	//
	// Seek: with hasCursor and before=false the page contains rows strictly OLDER
	// than the (cursorCreatedAt, cursorID) tuple (the forward/"after" direction of
	// a newest-first list); with before=true it contains rows strictly NEWER than
	// it (the CLOSEST such rows). Every page is returned newest→oldest regardless
	// of direction. Without a cursor the newest page is returned. hasMore reports
	// whether at least one more row exists in the seek direction beyond the page
	// (probed with a limit+1 read that is trimmed off). Returns an empty (non-nil)
	// slice when nothing matches or the owner VK id is empty.
	ListOwnedBatchIDsPage(ctx context.Context, provider string, owner tables.ManagedBatchOwner, cursorCreatedAt time.Time, cursorID string, hasCursor, before bool, limit int, tx ...*gorm.DB) (rows []tables.TableManagedBatch, hasMore bool, err error)
}

// The concrete RDB store implements both the base ledger capability and the
// optional pager capability.
var _ ManagedBatchStore = (*RDBConfigStore)(nil)
var _ ManagedBatchPager = (*RDBConfigStore)(nil)

// dbForBatchOwnership resolves the gorm handle for the managed-batch ledger
// methods, honoring an optional caller-supplied transaction the same way the
// other configstore CRUD methods do.
func (s *RDBConfigStore) dbForBatchOwnership(ctx context.Context, tx ...*gorm.DB) *gorm.DB {
	if len(tx) > 0 && tx[0] != nil {
		return tx[0].WithContext(ctx)
	}
	return s.DB().WithContext(ctx)
}

// RegisterManagedBatch persists a batch-ownership row. It is idempotent on the
// (provider, batch_id) unique index: a repeated capture (e.g. a retried create)
// is a no-op and, crucially, never re-assigns an existing batch to a different
// owner — ownership is claimed exactly once, by the tenant that first created
// the batch.
func (s *RDBConfigStore) RegisterManagedBatch(ctx context.Context, batch *tables.TableManagedBatch, tx ...*gorm.DB) error {
	if batch == nil {
		return errors.New("managed batch cannot be nil")
	}
	if batch.Provider == "" || batch.BatchID == "" {
		return errors.New("managed batch requires provider and batch_id")
	}
	if batch.OwnerVirtualKeyID == "" {
		return errors.New("managed batch requires an owner virtual key id")
	}
	if batch.ID == "" {
		batch.ID = uuid.NewString()
	}
	if batch.CreatedAt.IsZero() {
		batch.CreatedAt = time.Now().UTC()
	}
	// DoNothing on conflict keeps the first writer's ownership authoritative.
	return s.dbForBatchOwnership(ctx, tx...).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "provider"}, {Name: "batch_id"}},
			DoNothing: true,
		}).
		Create(batch).Error
}

// GetManagedBatch returns the ownership row for a (provider, batch_id), or
// (nil, nil) when no row exists.
func (s *RDBConfigStore) GetManagedBatch(ctx context.Context, provider, batchID string, tx ...*gorm.DB) (*tables.TableManagedBatch, error) {
	if provider == "" || batchID == "" {
		return nil, nil
	}
	var row tables.TableManagedBatch
	err := s.dbForBatchOwnership(ctx, tx...).
		Where("provider = ? AND batch_id = ?", provider, batchID).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

// ListOwnedBatchIDs returns the upstream batch ids for a provider owned by the
// given owner tuple. Ownership is OR across VK / team / customer, ignoring any
// empty owner value so a team-less or customer-less caller never matches on an
// empty column. Returns an empty slice (not nil) when nothing matches.
func (s *RDBConfigStore) ListOwnedBatchIDs(ctx context.Context, provider string, owner tables.ManagedBatchOwner, tx ...*gorm.DB) ([]string, error) {
	ids := []string{}
	if provider == "" || owner.VirtualKeyID == "" {
		return ids, nil
	}
	db := s.dbForBatchOwnership(ctx, tx...).Model(&tables.TableManagedBatch{}).Where("provider = ?", provider)

	// Build the OR ownership predicate in an isolated sub-clause so it is
	// AND-ed with the provider filter rather than widening it.
	ownerClause := s.DB().Session(&gorm.Session{NewDB: true}).
		Where("owner_virtual_key_id = ?", owner.VirtualKeyID)
	if owner.TeamID != "" {
		ownerClause = ownerClause.Or("owner_team_id = ?", owner.TeamID)
	}
	if owner.CustomerID != "" {
		ownerClause = ownerClause.Or("owner_customer_id = ?", owner.CustomerID)
	}

	if err := db.Where(ownerClause).Pluck("batch_id", &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

// ListOwnedBatchIDsPage returns one keyset page of the owner's batch rows for a
// provider in REVERSE-CHRONOLOGICAL (newest-first) order — matching native batch
// list APIs (OpenAI / Anthropic / Bedrock all page newest→oldest). It mirrors the
// OR-ownership sub-clause ListOwnedBatchIDs builds — AND-ed with the provider
// filter — and adds a (created_at, batch_id) tuple seek so a caller can page deep
// through all of its own batches.
//
// This is the leak-proof core of deep owner-scoped list pagination: the WHERE
// clause admits only rows the caller owns, so no boundary id, cursor, or data row
// can ever reference a foreign batch. The keyset is stable and total (batch_id is
// unique per provider), so pages neither skip nor duplicate across a mutating
// table. It reads limit+1 rows to detect a further page (hasMore) then trims.
//
// Direction: the DEFAULT and forward ("after") direction pages newest→oldest —
// the seek admits rows OLDER than the cursor and orders DESC. Backward ("before",
// Anthropic before_id = toward newer) admits rows NEWER than the cursor, fetches
// the ones CLOSEST to the cursor first (ASC), then reverses to newest-first so
// every page is returned newest→oldest regardless of direction.
func (s *RDBConfigStore) ListOwnedBatchIDsPage(ctx context.Context, provider string, owner tables.ManagedBatchOwner, cursorCreatedAt time.Time, cursorID string, hasCursor, before bool, limit int, tx ...*gorm.DB) ([]tables.TableManagedBatch, bool, error) {
	rows := []tables.TableManagedBatch{}
	if provider == "" || owner.VirtualKeyID == "" {
		return rows, false, nil
	}
	if limit <= 0 {
		limit = 1
	}

	db := s.dbForBatchOwnership(ctx, tx...).Model(&tables.TableManagedBatch{}).Where("provider = ?", provider)

	// OR ownership predicate in an isolated sub-clause (AND-ed with provider),
	// identical to ListOwnedBatchIDs so the two never diverge on who owns what.
	ownerClause := s.DB().Session(&gorm.Session{NewDB: true}).
		Where("owner_virtual_key_id = ?", owner.VirtualKeyID)
	if owner.TeamID != "" {
		ownerClause = ownerClause.Or("owner_team_id = ?", owner.TeamID)
	}
	if owner.CustomerID != "" {
		ownerClause = ownerClause.Or("owner_customer_id = ?", owner.CustomerID)
	}
	db = db.Where(ownerClause)

	if hasCursor {
		if before {
			// "before" the cursor in a newest-first list = strictly NEWER.
			db = db.Where("(created_at > ? OR (created_at = ? AND batch_id > ?))", cursorCreatedAt, cursorCreatedAt, cursorID)
		} else {
			// forward/"after" in a newest-first list = strictly OLDER.
			db = db.Where("(created_at < ? OR (created_at = ? AND batch_id < ?))", cursorCreatedAt, cursorCreatedAt, cursorID)
		}
	}

	// Default/forward: newest-first (DESC). Backward: fetch closest-to-cursor first
	// (ASC), reversed below back to newest-first.
	order := "created_at DESC, batch_id DESC"
	if before {
		order = "created_at ASC, batch_id ASC"
	}

	page := []tables.TableManagedBatch{}
	if err := db.Order(order).Limit(limit + 1).Find(&page).Error; err != nil {
		return nil, false, err
	}

	hasMore := len(page) > limit
	if hasMore {
		page = page[:limit]
	}
	if before {
		// Reverse the ASC "before" page back to newest-first (DESC) display order.
		for i, j := 0, len(page)-1; i < j; i, j = i+1, j-1 {
			page[i], page[j] = page[j], page[i]
		}
	}
	return page, hasMore, nil
}
