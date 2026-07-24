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

// The concrete RDB store implements the optional ledger capability.
var _ ManagedBatchStore = (*RDBConfigStore)(nil)

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
