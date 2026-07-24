package tables

import "time"

// TableManagedBatch records which tenant owns each batch job created through
// this gateway.
//
// Why this table exists: batch lifecycle calls (list / retrieve / cancel /
// results / delete) relay to the SHARED upstream provider credentials with no
// per-tenant scoping. Without an ownership record, any valid virtual key can
// list, read, cancel, or fetch the results of any other tenant's batch simply
// by learning or guessing its provider batch id. The governance plugin writes
// one row per successful batch create — keyed by the upstream provider batch
// id — and consults it to scope list output and gate per-id lifecycle verbs to
// the owning tenant.
//
// Ownership is a tuple captured from the creating virtual key: its row id, and
// (when present) its team id, customer id, and creator user id. A caller is
// considered the owner when it shares the owning VK id, OR a non-empty team id,
// OR a non-empty customer id — mirroring the governance hierarchy's
// team/customer semantics.
type TableManagedBatch struct {
	ID string `gorm:"type:varchar(36);primaryKey" json:"id"`

	// Provider + BatchID identify the upstream batch. BatchID is stored in its
	// upstream (provider-native) form — the same form the governance plugin sees
	// on the create response and on per-id lifecycle requests, after the
	// integration layer has undone any dialect-specific id encoding. The unique
	// index makes a (provider, batch_id) capture idempotent and prevents a
	// second tenant from claiming an id another tenant already owns.
	Provider string `gorm:"type:varchar(64);not null;uniqueIndex:idx_managed_batch_provider_batch,priority:1" json:"provider"`
	BatchID  string `gorm:"type:varchar(512);not null;uniqueIndex:idx_managed_batch_provider_batch,priority:2" json:"batch_id"`

	// Owner tuple. OwnerVirtualKeyID is always set; the others are "" when the
	// creating key had no team / customer / creator. Empty owner columns never
	// match a caller (the match logic ignores empty values), so a team-less key's
	// batch is reachable only via its exact VK id.
	OwnerVirtualKeyID string `gorm:"type:varchar(255);not null;index:idx_managed_batch_owner_vk" json:"owner_virtual_key_id"`
	OwnerTeamID       string `gorm:"type:varchar(255);not null;default:''" json:"owner_team_id"`
	OwnerCustomerID   string `gorm:"type:varchar(255);not null;default:''" json:"owner_customer_id"`
	OwnerUserID       string `gorm:"type:varchar(255);not null;default:''" json:"owner_user_id"`

	CreatedAt time.Time `gorm:"not null" json:"created_at"`
}

// TableName sets the table name for the managed-batch ownership ledger.
func (TableManagedBatch) TableName() string { return "governance_managed_batches" }

// ManagedBatchOwner is the ownership tuple used to record a batch's owner and to
// test whether a caller owns a batch. VirtualKeyID is the VK row id; TeamID and
// CustomerID are the caller's effective team / customer (resolved directly from
// the VK or via its team). Empty TeamID / CustomerID mean "no team / customer"
// and must never be treated as a wildcard match.
type ManagedBatchOwner struct {
	VirtualKeyID string
	TeamID       string
	CustomerID   string
	UserID       string
}

// Matches reports whether the caller owner tuple owns a batch recorded with the
// given persisted owner columns. A match requires the same VK id, or a shared
// non-empty team id, or a shared non-empty customer id. This mirrors the
// governance hierarchy: keys under the same team or customer are one tenant.
func (caller ManagedBatchOwner) Matches(ownerVKID, ownerTeamID, ownerCustomerID string) bool {
	if caller.VirtualKeyID != "" && caller.VirtualKeyID == ownerVKID {
		return true
	}
	if caller.TeamID != "" && caller.TeamID == ownerTeamID {
		return true
	}
	if caller.CustomerID != "" && caller.CustomerID == ownerCustomerID {
		return true
	}
	return false
}
