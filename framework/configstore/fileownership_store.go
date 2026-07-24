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

// ManagedFileStore is the file-ownership ledger capability, the file-lifecycle
// sibling of ManagedBatchStore. Like it, this is deliberately NOT part of the
// public ConfigStore interface: adding it there would force every external SDK
// implementer of ConfigStore to implement methods they may never use. Instead it
// is an OPTIONAL capability — the concrete RDB store implements it, and consumers
// (the governance plugin) obtain it by type-asserting their ConfigStore to this
// interface, degrading gracefully when the assertion fails.
//
// File lifecycle relays use shared upstream provider credentials with no
// per-tenant scoping, so the governance plugin records which tenant owns each
// file created through the gateway (keyed by the upstream provider file id) and
// consults it to scope list output and gate per-id file verbs — most importantly
// the content download (GET /v1/files/{id}/content). See tables.TableManagedFile.
type ManagedFileStore interface {
	// RegisterManagedFile persists one ownership row; it is idempotent on the
	// (provider, file_id) unique key (a repeat capture is a no-op and never
	// re-assigns ownership).
	RegisterManagedFile(ctx context.Context, file *tables.TableManagedFile, tx ...*gorm.DB) error
	// GetManagedFile returns the owner row for a (provider, file_id) or
	// (nil, nil) when absent.
	GetManagedFile(ctx context.Context, provider, fileID string, tx ...*gorm.DB) (*tables.TableManagedFile, error)
}

// ManagedFilePager is a SEPARATE optional capability, layered on top of
// ManagedFileStore, that adds keyset pagination over a caller's owned files. It
// is deliberately NOT folded into ManagedFileStore: doing so would break backward
// compatibility — an external config store that already implements the two-method
// ManagedFileStore capability would silently stop satisfying it after this method
// was added, the governance plugin's type-assertion would fail, and per-tenant
// file isolation would silently DEGRADE to the single-upstream-page filter on
// upgrade. Keeping it separate lets a store implement ownership WITHOUT
// pagination; the plugin then falls back to filtering the single upstream list
// page against the ledger (still leak-proof) rather than disabling enforcement.
type ManagedFilePager interface {
	// ListOwnedFileIDsPage returns ONE keyset page of the owner's file rows for a
	// provider, ordered NEWEST-FIRST by (created_at DESC, file_id DESC) — the
	// default order the OpenAI Files API returns. It powers deep owner-scoped list
	// pagination: the caller pages through all of its own files (not just the most
	// recent upstream page) while a foreign id can never appear, because only
	// owner-matched rows are ever returned.
	//
	// Seek: with hasCursor the page contains rows strictly PAST the
	// (cursorCreatedAt, cursorID) tuple in the sort direction — OLDER for the
	// default newest-first order, NEWER when asc is set (the OpenAI `order=asc`
	// oldest-first order). Without a cursor the first page in the chosen order is
	// returned. hasMore reports whether at least one more row exists beyond the
	// returned page (probed with a limit+1 read that is trimmed off). Returns an
	// empty (non-nil) slice when nothing matches or the owner VK id is empty.
	//
	// asc selects the sort/seek direction: false is the default DESC (created_at
	// DESC, file_id DESC); true honors the OpenAI Files `order=asc` request by
	// sorting ASC (created_at ASC, file_id ASC) and seeking strictly NEWER than the
	// cursor. The keyset stays stable and total in either direction.
	ListOwnedFileIDsPage(ctx context.Context, provider string, owner tables.ManagedBatchOwner, cursorCreatedAt time.Time, cursorID string, hasCursor, asc bool, limit int, tx ...*gorm.DB) (rows []tables.TableManagedFile, hasMore bool, err error)
}

// ManagedFileProvenanceStore is a SEPARATE optional capability, layered on top of
// ManagedFileStore, that stamps batch provenance (SourceBatchID) onto an EXISTING
// owned file row. Like ManagedFilePager it is kept separate rather than folded into
// ManagedFileStore so a store that implements ownership but not provenance still
// enforces isolation (the plugin simply skips the diagnostic provenance marker
// rather than disabling file scoping).
//
// It exists to satisfy a hard security invariant of the batch<->file linkage: a
// batch input file is uploaded out of band and already owns a row from that upload,
// so batch-create must NEVER mint a new ownership row for the input file (doing so
// would let a caller claim ownership of a file it never uploaded merely by naming
// its id in a batch). SetFileSourceBatch therefore only UPDATES a row the caller
// already owns; it never creates one and never re-assigns ownership.
type ManagedFileProvenanceStore interface {
	// SetFileSourceBatch records batch provenance (SourceBatchID) on the EXISTING
	// row for (provider, file_id) IFF that row's owner matches the given owner
	// tuple. It never creates a row and never changes ownership: an absent or
	// foreign-owned file is left untouched and reported via linked=false. This is
	// how a batch input file (captured at upload time) gains its source-batch
	// marker without a batch-create ever being able to MINT ownership of a file the
	// caller did not already own.
	SetFileSourceBatch(ctx context.Context, provider, fileID, sourceBatchID string, owner tables.ManagedBatchOwner, tx ...*gorm.DB) (linked bool, err error)
}

// The concrete RDB store implements the base ledger capability and both optional
// capabilities (keyset pager + provenance linker).
var _ ManagedFileStore = (*RDBConfigStore)(nil)
var _ ManagedFilePager = (*RDBConfigStore)(nil)
var _ ManagedFileProvenanceStore = (*RDBConfigStore)(nil)

// dbForFileOwnership resolves the gorm handle for the managed-file ledger
// methods, honoring an optional caller-supplied transaction the same way the
// other configstore CRUD methods do.
func (s *RDBConfigStore) dbForFileOwnership(ctx context.Context, tx ...*gorm.DB) *gorm.DB {
	if len(tx) > 0 && tx[0] != nil {
		return tx[0].WithContext(ctx)
	}
	return s.DB().WithContext(ctx)
}

// RegisterManagedFile persists a file-ownership row. It is idempotent on the
// (provider, file_id) unique index: a repeated capture (e.g. re-linking a batch
// input file that was already captured on upload) is a no-op and, crucially,
// never re-assigns an existing file to a different owner — ownership is claimed
// exactly once, by the tenant that first created the file.
func (s *RDBConfigStore) RegisterManagedFile(ctx context.Context, file *tables.TableManagedFile, tx ...*gorm.DB) error {
	if file == nil {
		return errors.New("managed file cannot be nil")
	}
	if file.Provider == "" || file.FileID == "" {
		return errors.New("managed file requires provider and file_id")
	}
	if file.OwnerVirtualKeyID == "" {
		return errors.New("managed file requires an owner virtual key id")
	}
	if file.ID == "" {
		file.ID = uuid.NewString()
	}
	if file.CreatedAt.IsZero() {
		file.CreatedAt = time.Now().UTC()
	}
	// DoNothing on conflict keeps the first writer's ownership authoritative.
	return s.dbForFileOwnership(ctx, tx...).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "provider"}, {Name: "file_id"}},
			DoNothing: true,
		}).
		Create(file).Error
}

// GetManagedFile returns the ownership row for a (provider, file_id), or
// (nil, nil) when no row exists.
func (s *RDBConfigStore) GetManagedFile(ctx context.Context, provider, fileID string, tx ...*gorm.DB) (*tables.TableManagedFile, error) {
	if provider == "" || fileID == "" {
		return nil, nil
	}
	var row tables.TableManagedFile
	err := s.dbForFileOwnership(ctx, tx...).
		Where("provider = ? AND file_id = ?", provider, fileID).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

// fileOwnerClause builds the OR-ownership sub-clause (VK id OR shared non-empty
// team OR shared non-empty customer) in an isolated session so it AND-s cleanly
// with the caller's other predicates, mirroring the batch store so the two never
// diverge on who owns what.
func (s *RDBConfigStore) fileOwnerClause(owner tables.ManagedBatchOwner) *gorm.DB {
	clause := s.DB().Session(&gorm.Session{NewDB: true}).
		Where("owner_virtual_key_id = ?", owner.VirtualKeyID)
	if owner.TeamID != "" {
		clause = clause.Or("owner_team_id = ?", owner.TeamID)
	}
	if owner.CustomerID != "" {
		clause = clause.Or("owner_customer_id = ?", owner.CustomerID)
	}
	return clause
}

// ListOwnedFileIDsPage returns one keyset page of the owner's file rows for a
// provider, ordered NEWEST-FIRST by (created_at DESC, file_id DESC) by default, or
// OLDEST-FIRST (created_at ASC, file_id ASC) when asc is set to honor the OpenAI
// Files `order=asc` request. It AND-s the provider filter with the OR-ownership
// sub-clause the batch store also builds, and adds a tuple seek so a caller can
// page deep through all of its own files.
//
// This is the leak-proof core of deep owner-scoped file list pagination: the
// WHERE clause admits only rows the caller owns, so no boundary id, cursor, or
// data row can ever reference a foreign file. The keyset (created_at, file_id) is
// stable and total (file_id is unique per provider), so pages neither skip nor
// duplicate rows across a mutating table, in either sort direction.
//
// It reads limit+1 rows to detect a further page (hasMore) without a second COUNT
// query, then trims the probe row. The OpenAI Files API (the only file-list
// dialect that paginates) exposes a single forward `after` cursor whose direction
// follows `order`, so the seek is forward-in-sort-order and there is no separate
// backward direction.
func (s *RDBConfigStore) ListOwnedFileIDsPage(ctx context.Context, provider string, owner tables.ManagedBatchOwner, cursorCreatedAt time.Time, cursorID string, hasCursor, asc bool, limit int, tx ...*gorm.DB) ([]tables.TableManagedFile, bool, error) {
	rows := []tables.TableManagedFile{}
	if provider == "" || owner.VirtualKeyID == "" {
		return rows, false, nil
	}
	if limit <= 0 {
		limit = 1
	}

	db := s.dbForFileOwnership(ctx, tx...).Model(&tables.TableManagedFile{}).Where("provider = ?", provider)
	db = db.Where(s.fileOwnerClause(owner))

	order := "created_at DESC, file_id DESC"
	if asc {
		order = "created_at ASC, file_id ASC"
	}
	if hasCursor {
		if asc {
			// Oldest-first: seek strictly NEWER than the cursor tuple.
			db = db.Where("(created_at > ? OR (created_at = ? AND file_id > ?))", cursorCreatedAt, cursorCreatedAt, cursorID)
		} else {
			// Newest-first: seek strictly OLDER than the cursor tuple.
			db = db.Where("(created_at < ? OR (created_at = ? AND file_id < ?))", cursorCreatedAt, cursorCreatedAt, cursorID)
		}
	}

	page := []tables.TableManagedFile{}
	if err := db.Order(order).Limit(limit + 1).Find(&page).Error; err != nil {
		return nil, false, err
	}

	hasMore := len(page) > limit
	if hasMore {
		page = page[:limit]
	}
	return page, hasMore, nil
}

// SetFileSourceBatch stamps batch provenance onto an EXISTING file row, guarded by
// the owner tuple in the same OR sub-clause the list keyset uses. It is a targeted
// UPDATE — never an insert — so it can neither create a row nor re-assign an
// existing one: a file that is absent or owned by another tenant matches zero rows
// and is reported via linked=false. This upholds the batch<->file invariant that a
// batch-create must never grant a caller ownership of a file it did not already
// own; the input file's ownership always comes from its own upload capture.
func (s *RDBConfigStore) SetFileSourceBatch(ctx context.Context, provider, fileID, sourceBatchID string, owner tables.ManagedBatchOwner, tx ...*gorm.DB) (bool, error) {
	if provider == "" || fileID == "" || sourceBatchID == "" || owner.VirtualKeyID == "" {
		return false, nil
	}
	res := s.dbForFileOwnership(ctx, tx...).
		Model(&tables.TableManagedFile{}).
		Where("provider = ? AND file_id = ?", provider, fileID).
		Where(s.fileOwnerClause(owner)).
		Update("source_batch_id", sourceBatchID)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}
