package tables

import "time"

// TableManagedFile records which tenant owns each file created through this
// gateway. It is the file-lifecycle sibling of TableManagedBatch.
//
// Why this table exists: file lifecycle calls (list / retrieve / delete /
// content-download) relay to the SHARED upstream provider credentials with no
// per-tenant scoping. Without an ownership record, any valid virtual key can
// list, read metadata for, delete, or — most damagingly — download the CONTENT
// of any other tenant's file simply by learning or guessing its provider file
// id (GET /v1/files/{id}/content). The governance plugin writes one row per
// successful file upload — keyed by the upstream provider file id — and consults
// it to scope list output and gate per-id file verbs to the owning tenant.
//
// Ownership is a tuple captured from the creating virtual key: its row id, and
// (when present) its team id, customer id, and creator user id. A caller is
// considered the owner when it shares the owning VK id, OR a non-empty team id,
// OR a non-empty customer id — mirroring the governance hierarchy's
// team/customer semantics. The owner tuple type (ManagedBatchOwner) and its
// Matches semantics are DELIBERATELY SHARED with the batch ledger: a tenant is
// the same tenant whether it owns a batch or a file, so there is exactly one
// notion of ownership.
type TableManagedFile struct {
	ID string `gorm:"type:varchar(36);primaryKey" json:"id"`

	// Provider + FileID identify the upstream file. FileID is stored in its
	// upstream (provider-native, DECODED) form — the same form the governance
	// plugin sees on the upload response (before the transport applies
	// encodeStorageFileID) and on per-id file requests (after the transport
	// applies decodeStorageFileID). The unique index makes a (provider, file_id)
	// capture idempotent and prevents a second tenant from claiming an id another
	// tenant already owns.
	//
	// FileID also carries the trailing tie-breaker column of the owner keyset
	// index (idx_managed_file_owner_keyset) so a caller's own files can be
	// paginated by a stable (created_at, file_id) keyset without a full scan.
	Provider string `gorm:"type:varchar(64);not null;uniqueIndex:idx_managed_file_provider_file,priority:1" json:"provider"`
	FileID   string `gorm:"type:varchar(512);not null;uniqueIndex:idx_managed_file_provider_file,priority:2;index:idx_managed_file_owner_keyset,priority:3" json:"file_id"`

	// Owner tuple. OwnerVirtualKeyID is always set; the others are "" when the
	// creating key had no team / customer / creator. Empty owner columns never
	// match a caller (the match logic ignores empty values), so a team-less key's
	// file is reachable only via its exact VK id.
	//
	// OwnerVirtualKeyID leads the owner keyset index
	// (owner_virtual_key_id, created_at, file_id): the dominant single-VK list
	// case (a key paging its own files) is served entirely from this index.
	OwnerVirtualKeyID string `gorm:"type:varchar(255);not null;index:idx_managed_file_owner_vk;index:idx_managed_file_owner_keyset,priority:1" json:"owner_virtual_key_id"`
	OwnerTeamID       string `gorm:"type:varchar(255);not null;default:''" json:"owner_team_id"`
	OwnerCustomerID   string `gorm:"type:varchar(255);not null;default:''" json:"owner_customer_id"`
	OwnerUserID       string `gorm:"type:varchar(255);not null;default:''" json:"owner_user_id"`

	// Purpose is the file's upload purpose (e.g. "batch", "batch_output",
	// "user_data"), captured from the upload request / response for diagnostics
	// and possible future purpose-scoped listing. Stored as a plain string to keep
	// this table free of a schemas dependency.
	Purpose string `gorm:"type:varchar(64);not null;default:''" json:"purpose"`

	// SourceBatchID records provenance for files materialized by a batch (a batch
	// input file linked at create time, or an output / error file linked when a
	// batch reaches a terminal state). Empty for ordinary uploads. It is not a
	// foreign key — it is a soft provenance marker so an operator can see which
	// batch produced a file — and it never participates in ownership matching.
	SourceBatchID string `gorm:"type:varchar(512);not null;default:''" json:"source_batch_id"`

	CreatedAt time.Time `gorm:"not null;index:idx_managed_file_owner_keyset,priority:2" json:"created_at"`
}

// TableName sets the table name for the managed-file ownership ledger.
func (TableManagedFile) TableName() string { return "governance_managed_files" }
