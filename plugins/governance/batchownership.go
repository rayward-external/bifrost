package governance

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
)

// Per-tenant batch ownership enforcement.
//
// Batch lifecycle calls (list / retrieve / cancel / results / delete) relay to
// the SHARED upstream provider credentials with zero per-tenant scoping. Left
// unscoped, any valid virtual key can enumerate the whole shared workspace's
// batches (list) or read/cancel any batch whose id it learns or guesses
// (per-id verbs). This file closes that gap using a create-time ownership
// ledger (configstore.TableManagedBatch):
//
//   - CREATE  (PostLLMHook): record (provider, upstream batch id) -> owner tuple.
//     A ledger-write failure attempts a best-effort compensating cancel of the
//     just-created upstream batch, then fails the request — a batch that exists
//     upstream but is unowned is unreachable under the deny-unknown policy, which
//     is strictly worse than a create that never happened.
//   - LIST    (PreLLMHook SHORT-CIRCUIT, when the lifecycle client is present):
//     the upstream list endpoint is NEVER called for a scoped (non-admin) caller.
//     Instead the page is built from the ownership ledger with a deep keyset over
//     ALL of the caller's own batches (not just the most recent upstream page), in
//     REVERSE-CHRONOLOGICAL order to match native batch APIs, then a fresh
//     BatchRetrieve is fanned out per owned id to assemble live state. Cursor /
//     first_id / last_id are the caller's OWN batch ids. Leak-proof by
//     construction: no foreign page is ever fetched, and no returned field can
//     carry a batch id the caller does not own. When NO lifecycle client is
//     available (Go-SDK deployments), the request is relayed upstream and the
//     PostLLMHook single-page owner filter scopes the fetched page instead —
//     shallow but still leak-proof; isolation holds in both modes.
//   - PER-ID  (PreLLMHook): look up the owner and deny (404, existence-hiding)
//     when the caller is not the owner, BEFORE the upstream relay. Ledger-absent
//     ids follow UnknownBatchIDPolicy (deny by default — see resolveUnknownBatchIDPolicy).
//
// Ownership match mirrors the governance hierarchy: same VK id, OR shared
// non-empty team id, OR shared non-empty customer id (see ManagedBatchOwner).
//
// DEEP LIST PAGINATION (leak surface): the inbound pagination cursor is the
// dialect-native batch id (OpenAI after, Anthropic after_id/before_id, Gemini
// page token). That is safe because the gate resolves the cursor id through the
// ledger and only seeks past it when the CALLER OWNS it; an unowned or unknown
// cursor id yields an empty page (never leaks, never confirms the id exists).
// Forward (after) paging is the fully-supported path; Anthropic before_id is
// supported as the mirrored backward seek. A residual edge exists only when a
// dialect re-encodes cross-provider ids (e.g. OpenAI-dialect listing a Gemini
// batch): a re-encoded cursor will not resolve in the ledger and truncates to an
// empty page — it fails SAFE (no leak), and full cross-dialect cursor decoding is
// tracked as a follow-up.
//
// Because the short-circuit SKIPS the upstream call, it also re-applies the
// result-scoping filters the provider would otherwise have applied (Bedrock's
// statusEquals / nameContains — the only dialect that sends any) against the
// retrieved batch objects, and derives has_more / the continuation cursor from
// the FILTERED result, not raw keyset counts. Keyset pagination is a SEPARATE
// optional store capability (ManagedBatchPager): a store that implements
// ownership but not the pager still enforces isolation — the list falls back to
// paging the owned-id list (leak-proof, just without the index), never disabling
// enforcement.
//
// KNOWN UNCOVERED SIBLING LEAK: file-content reads (GET /v1/files/{id}/content,
// FileContentRequest) share this leak class — a caller who learns a batch output
// file id can download its content. Closing it cleanly requires a separate file
// ownership ledger (input files are uploaded out of band, not created by batch),
// so it is intentionally out of scope here and tracked in the security report.

const (
	// UnknownBatchIDPolicyDeny (the secure default) refuses a per-id verb for a
	// batch id that has no ownership-ledger row: it returns 404 rather than
	// relaying to the shared upstream credentials. UnknownBatchIDPolicyAllow is an
	// opt-in migration window that relays ledger-absent ids to upstream (the
	// pre-fix behavior, for ledger-absent ids only); ids that DO have a ledger row
	// are always owner-gated regardless. Admin VKs bypass either way.
	UnknownBatchIDPolicyDeny  = "deny"
	UnknownBatchIDPolicyAllow = "allow"

	// defaultBatchListLimit is the pre-allocation hint when the caller did not
	// specify a page size.
	defaultBatchListLimit = 20

	// maxBatchListPageSize clamps the caller-controlled page size. The transport
	// parses ?limit=/page_size= unbounded, so an absurd value (e.g. 1e12) would
	// otherwise be used as a slice capacity and OOM-kill the process. OpenAI and
	// Anthropic cap batch list at 100; we use the same ceiling.
	maxBatchListPageSize = 100

	// batchListRequestedLimitKey carries the caller's requested list page size
	// from PreLLMHook to PostLLMHook (used only as an allocation-capacity hint).
	batchListRequestedLimitKey schemas.BifrostContextKey = "bf-governance-batch-list-limit"

	// batchListFetchChunk is how many owned rows the deep-list assembler pulls from
	// the ledger per keyset round-trip. Larger than a typical page so a filtered
	// scan (which retrieves + tests each row) does not do one DB round-trip per row.
	batchListFetchChunk = 100

	// maxBatchListOwnedScan hard-bounds how many owned batches a single list request
	// will retrieve+examine while assembling a page under an active result filter.
	// Without it, a filter that matches sparsely over a large owned set could scan
	// unboundedly. On hitting it we FAIL LOUD (batchListScanCapError) rather than
	// falsely advertise has_more or silently truncate the caller's own results.
	maxBatchListOwnedScan = 2000
)

// batchLedger is the subset of the config store the batch-ownership gate depends
// on (identical to configstore.ManagedBatchStore). It is populated by
// type-asserting the plugin's config store to the optional ledger capability, so
// a store that does not support the ledger simply leaves it nil. Tests inject a
// fake.
type batchLedger interface {
	RegisterManagedBatch(ctx context.Context, batch *configstoreTables.TableManagedBatch, tx ...*gorm.DB) error
	GetManagedBatch(ctx context.Context, provider, batchID string, tx ...*gorm.DB) (*configstoreTables.TableManagedBatch, error)
	ListOwnedBatchIDs(ctx context.Context, provider string, owner configstoreTables.ManagedBatchOwner, tx ...*gorm.DB) ([]string, error)
}

// batchPager is the OPTIONAL keyset-pagination capability (configstore's
// ManagedBatchPager), asserted SEPARATELY from batchLedger so a store that
// implements ownership but not pagination still enforces isolation. When a store
// provides it, deep owner-scoped list pagination runs over the (created_at,
// batch_id) keyset; when it does not, listOwnedBatchesForCaller falls back to
// paging the owned-id list (leak-proof, just without the index acceleration) —
// enforcement is NEVER disabled for lack of a pager.
type batchPager interface {
	ListOwnedBatchIDsPage(ctx context.Context, provider string, owner configstoreTables.ManagedBatchOwner, cursorCreatedAt time.Time, cursorID string, hasCursor, before bool, limit int, tx ...*gorm.DB) ([]configstoreTables.TableManagedBatch, bool, error)
}

// BatchLifecycleClient is the narrow core-client capability the batch-ownership
// enforcement needs to issue its in-process sub-requests: a compensating cancel
// after a failed ownership capture, and a per-id BatchRetrieve fan-out used to
// build the owner-scoped list page from the ledger. *bifrost.Bifrost satisfies
// it. It is injected post-construction (SetBatchLifecycleClient) because the core
// client is created after the plugins, so it may be nil (both paths degrade — see
// call sites).
type BatchLifecycleClient interface {
	BatchCancelRequest(ctx *schemas.BifrostContext, req *schemas.BifrostBatchCancelRequest) (*schemas.BifrostBatchCancelResponse, *schemas.BifrostError)
	BatchRetrieveRequest(ctx *schemas.BifrostContext, req *schemas.BifrostBatchRetrieveRequest) (*schemas.BifrostBatchRetrieveResponse, *schemas.BifrostError)
}

// SetBatchLifecycleClient injects the core client used for the in-process
// compensating cancel. Wired by the transport after the client is constructed.
func (p *GovernancePlugin) SetBatchLifecycleClient(c BatchLifecycleClient) {
	p.batchClient = c
}

// configureBatchOwnership wires the ledger and the LIVE pointers to the
// batch-ownership operator knobs (unknown-id policy, admin-VK list) from the
// plugin config.
//
// The ledger is obtained by type-asserting the config store to the optional
// configstore.ManagedBatchStore capability: a nil store (in-memory-only mode) or
// a store that does not implement the capability leaves batchLedger nil,
// disabling enforcement — mirroring how the plugin treats every other optional
// dependency (model catalog, MCP catalog, config store): log-and-degrade rather
// than hard-fail. The production RDB store always implements the capability.
//
// The policy and admin list are stored as POINTERS to the live client-config
// fields (not snapshotted), so an /api/config update — which mutates ClientConfig
// in place via ReloadClientConfigFromConfigStore — takes effect WITHOUT a restart,
// mirroring how IsVkMandatory / RequiredHeaders stay live.
func (p *GovernancePlugin) configureBatchOwnership(config *Config, configStore configstore.ConfigStore) {
	if configStore != nil {
		if ledger, ok := configStore.(configstore.ManagedBatchStore); ok {
			p.batchLedger = ledger
		} else if p.logger != nil {
			p.logger.Warn("[Governance] config store does not support the batch-ownership ledger; per-tenant batch scoping is DISABLED for this store")
		}
		// Keyset pagination is a SEPARATE optional capability. A store that
		// implements ownership but not the pager still enforces isolation; the
		// list path falls back to an owned-id page (see listOwnedBatchesForCaller).
		if pager, ok := configStore.(configstore.ManagedBatchPager); ok {
			p.batchPager = pager
		} else if p.batchLedger != nil && p.logger != nil {
			p.logger.Warn("[Governance] config store implements the batch-ownership ledger but not the keyset pager; batch list falls back to an owned-id page (isolation stays enforced)")
		}
	}
	if config == nil {
		return
	}
	p.batchAdminVKIDs = config.BatchAdminVirtualKeyIDs
	p.batchUnknownIDPolicy = config.UnknownBatchIDPolicy
}

// resolveUnknownBatchIDPolicy reads the LIVE policy through the config pointer.
// Default (unset/empty) is deny — the secure posture: a ledger-absent id is
// refused rather than relayed to the shared upstream credentials. Only an
// explicit "allow" opts into the migration window.
func (p *GovernancePlugin) resolveUnknownBatchIDPolicy() string {
	if p.batchUnknownIDPolicy != nil &&
		strings.EqualFold(strings.TrimSpace(*p.batchUnknownIDPolicy), UnknownBatchIDPolicyAllow) {
		return UnknownBatchIDPolicyAllow
	}
	return UnknownBatchIDPolicyDeny
}

// resolveBatchListTarget derives the list allocation-capacity hint from the
// caller's requested page size, preferring Limit (OpenAI/Anthropic) then PageSize
// (Vertex/Gemini), falling back to the default, and clamping to
// maxBatchListPageSize so a caller-controlled value can never drive an unbounded
// allocation.
func resolveBatchListTarget(limit, pageSize int) int {
	target := limit
	if target <= 0 {
		target = pageSize
	}
	if target <= 0 {
		target = defaultBatchListLimit
	}
	if target > maxBatchListPageSize {
		target = maxBatchListPageSize
	}
	return target
}

// isBatchOwnershipAdmin reports whether the resolved VK is an opt-in batch admin
// key that bypasses the gate and list filter. Reads the LIVE admin list through
// the config pointer.
func (p *GovernancePlugin) isBatchOwnershipAdmin(vkID string) bool {
	if vkID == "" || p.batchAdminVKIDs == nil {
		return false
	}
	for _, id := range *p.batchAdminVKIDs {
		if strings.TrimSpace(id) == vkID {
			return true
		}
	}
	return false
}

// callerBatchOwner resolves the ownership tuple for a virtual key, mirroring the
// governance hierarchy: a VK's customer may be attached directly or via its
// team; likewise its team id may be on the FK column or the loaded relation.
func callerBatchOwner(vk *configstoreTables.TableVirtualKey) configstoreTables.ManagedBatchOwner {
	owner := configstoreTables.ManagedBatchOwner{VirtualKeyID: vk.ID}
	switch {
	case vk.TeamID != nil:
		owner.TeamID = *vk.TeamID
	case vk.Team != nil:
		owner.TeamID = vk.Team.ID
	}
	switch {
	case vk.CustomerID != nil:
		owner.CustomerID = *vk.CustomerID
	case vk.Customer != nil:
		owner.CustomerID = vk.Customer.ID
	case vk.Team != nil && vk.Team.CustomerID != nil:
		owner.CustomerID = *vk.Team.CustomerID
	case vk.Team != nil && vk.Team.Customer != nil:
		owner.CustomerID = vk.Team.Customer.ID
	}
	if vk.CreatedByUserID != nil {
		owner.UserID = *vk.CreatedByUserID
	}
	return owner
}

// isBatchPerIDVerb reports whether a request type targets a single, already
// existing batch by id (as opposed to create or list).
func isBatchPerIDVerb(rt schemas.RequestType) bool {
	switch rt {
	case schemas.BatchRetrieveRequest, schemas.BatchCancelRequest,
		schemas.BatchResultsRequest, schemas.BatchDeleteRequest:
		return true
	}
	return false
}

// batchTargetFromRequest extracts the provider and upstream batch id addressed
// by a per-id batch verb. Returns empty strings for any other request type.
func batchTargetFromRequest(req *schemas.BifrostRequest) (schemas.ModelProvider, string) {
	switch req.RequestType {
	case schemas.BatchRetrieveRequest:
		if req.BatchRetrieveRequest != nil {
			return req.BatchRetrieveRequest.Provider, req.BatchRetrieveRequest.BatchID
		}
	case schemas.BatchCancelRequest:
		if req.BatchCancelRequest != nil {
			return req.BatchCancelRequest.Provider, req.BatchCancelRequest.BatchID
		}
	case schemas.BatchResultsRequest:
		if req.BatchResultsRequest != nil {
			return req.BatchResultsRequest.Provider, req.BatchResultsRequest.BatchID
		}
	case schemas.BatchDeleteRequest:
		if req.BatchDeleteRequest != nil {
			return req.BatchDeleteRequest.Provider, req.BatchDeleteRequest.BatchID
		}
	}
	return "", ""
}

// batchNotFoundError builds the existence-hiding 404 returned for both
// foreign-owned and (under deny policy) ledger-absent batch ids. It deliberately
// does NOT distinguish "exists but not yours" from "does not exist": a 403 would
// confirm the id belongs to someone. Type "not_found_error" matches Anthropic's
// 404 shape; the OpenAI-dialect surface renders the same BifrostError verbatim.
func batchNotFoundError() *schemas.BifrostError {
	return &schemas.BifrostError{
		Type:       bifrost.Ptr("not_found_error"),
		StatusCode: bifrost.Ptr(404),
		Error: &schemas.ErrorField{
			Type:    bifrost.Ptr("not_found_error"),
			Message: "batch not found",
		},
	}
}

// newBatchSubRequestContext builds a context for the in-process compensating
// cancel. It sets SkipPluginPipeline so the cancel does NOT re-enter the
// governance gate (which would block it on the not-yet-recorded id).
//
// Crucially, BifrostContext.Value falls THROUGH to the parent, so a plain child
// would inherit the caller's transport state — URL-path override, raw/large body
// passthrough, forwarded headers, key routing. Inherited, the internal cancel
// would be misrouted to the caller's create path or made to relay the caller's
// body instead of performing the cancel — leaving the batch orphaned exactly when
// the ownership write failed. ClearContextForInternalRequest tombstones that
// state on the sub-context (its own value store shadows the parent) so the cancel
// carries only what it needs (provider + batch id in the request struct, plus
// tracing continuity, which is deliberately preserved).
//
// Callers MUST defer Cancel() on the returned context: NewBifrostContext starts a
// watcher goroutine bound to the parent, which Cancel() releases promptly.
func newBatchSubRequestContext(parent context.Context) *schemas.BifrostContext {
	sub := schemas.NewBifrostContext(parent, schemas.NoDeadline)
	sub.SetValue(schemas.BifrostContextKeySkipPluginPipeline, true)
	bifrost.ClearContextForInternalRequest(sub)
	return sub
}

// enforceBatchOwnershipPreHook gates per-id batch verbs before the upstream
// relay. Returns a 404 BifrostError to short-circuit when the caller is not the
// owner (or the id is unknown under deny policy); nil to allow.
func (p *GovernancePlugin) enforceBatchOwnershipPreHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) *schemas.BifrostError {
	if req == nil || !isBatchPerIDVerb(req.RequestType) {
		return nil
	}
	// No ledger (in-memory-only mode / unsupported store) means no multi-tenant
	// relay to protect.
	if p.batchLedger == nil {
		return nil
	}

	provider, batchID := batchTargetFromRequest(req)
	if batchID == "" {
		return nil
	}

	// Resolve caller identity. Without a VK we cannot establish ownership; in
	// production enforce_auth_on_inference makes a VK mandatory (VK-less requests
	// are already rejected by EvaluateGovernanceRequest), so reaching here
	// VK-less means single-tenant/SDK usage where scoping does not apply.
	vkValue := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyVirtualKey)
	if vkValue == "" {
		return nil
	}
	vk, ok := p.store.GetVirtualKey(ctx, vkValue)
	if !ok || vk == nil {
		return nil
	}
	if p.isBatchOwnershipAdmin(vk.ID) {
		return nil
	}

	owner, err := p.batchLedger.GetManagedBatch(ctx, string(provider), batchID)
	if err != nil {
		// Fail closed on a ledger read error: deny rather than relay upstream
		// with no ownership check. Log for diagnostics.
		p.logger.Error("[Governance] batch ownership lookup failed for provider=%s: %v; denying %s", provider, err, req.RequestType)
		return batchNotFoundError()
	}
	if owner == nil {
		// Absent from the ledger: created before this ledger existed or out of
		// band. Policy decides; deny by default.
		if p.resolveUnknownBatchIDPolicy() == UnknownBatchIDPolicyAllow {
			return nil
		}
		return batchNotFoundError()
	}
	if callerBatchOwner(vk).Matches(owner.OwnerVirtualKeyID, owner.OwnerTeamID, owner.OwnerCustomerID) {
		return nil
	}
	// Foreign owner: always denied, existence-hidden, regardless of policy.
	return batchNotFoundError()
}

// applyBatchOwnershipPostHook records ownership on a successful create, and — in
// the NO-lifecycle-client deployment only — owner-filters the single upstream
// list page. Returns the (possibly mutated) result and a non-nil BifrostError
// only when a create-time ledger write fails (which must fail the request).
//
// LIST: when the lifecycle client is available, a scoped list is served entirely
// from the ledger via the PreLLMHook short-circuit (deep pagination), the upstream
// list is never fetched, and there is nothing to do here — so this post-hook
// filter runs ONLY when p.batchClient is nil (the request WAS relayed upstream).
// The two paths are mutually exclusive: the short-circuit is gated on the client
// being present, so a request that reached the upstream list had no client, and a
// short-circuited (ledger-built) response is never re-filtered here.
func (p *GovernancePlugin) applyBatchOwnershipPostHook(
	ctx *schemas.BifrostContext,
	result *schemas.BifrostResponse,
	requestType schemas.RequestType,
	provider schemas.ModelProvider,
	vkValue string,
) (*schemas.BifrostResponse, *schemas.BifrostError) {
	if result == nil || p.batchLedger == nil {
		return result, nil
	}
	switch requestType {
	case schemas.BatchCreateRequest:
		if berr := p.captureBatchOwnership(ctx, result, provider, vkValue); berr != nil {
			return nil, berr
		}
	case schemas.BatchListRequest:
		if p.batchClient == nil {
			// No client => the request relayed upstream; scope the fetched page.
			p.filterBatchListForCaller(ctx, result, provider, vkValue)
		}
	}
	return result, nil
}

// captureBatchOwnership writes the ownership row for a freshly created batch.
// The response id is the upstream provider id in its native form (dialect id
// re-encoding happens later, in the integration response converter), matching
// what per-id verbs resolve to — so the ledger key is consistent across create
// and lookup.
func (p *GovernancePlugin) captureBatchOwnership(
	ctx *schemas.BifrostContext,
	result *schemas.BifrostResponse,
	provider schemas.ModelProvider,
	vkValue string,
) *schemas.BifrostError {
	if result.BatchCreateResponse == nil || result.BatchCreateResponse.ID == "" {
		return nil
	}
	if provider == "" {
		provider = result.BatchCreateResponse.ExtraFields.Provider
	}
	if vkValue == "" {
		// No identity to attribute ownership to; nothing to record and nothing
		// to protect (single-tenant/SDK path).
		return nil
	}
	vk, ok := p.store.GetVirtualKey(ctx, vkValue)
	if !ok || vk == nil {
		return nil
	}
	owner := callerBatchOwner(vk)
	batchID := result.BatchCreateResponse.ID
	row := &configstoreTables.TableManagedBatch{
		Provider:          string(provider),
		BatchID:           batchID,
		OwnerVirtualKeyID: owner.VirtualKeyID,
		OwnerTeamID:       owner.TeamID,
		OwnerCustomerID:   owner.CustomerID,
		OwnerUserID:       owner.UserID,
	}
	err := p.batchLedger.RegisterManagedBatch(ctx, row)
	if err == nil {
		return nil
	}
	// The provider already created the batch but we could not record who owns it.
	// Returning a plain error would let the client retry and create a SECOND
	// upstream batch while this one runs orphaned, billable, and (under
	// deny-unknown) inaccessible to everyone. Attempt a best-effort compensating
	// cancel of the just-created batch so a retry is clean.
	p.logger.Error("[Governance] failed to record batch ownership (provider=%s batch=%s): %v", provider, batchID, err)
	if p.compensatingCancelBatch(ctx, provider, batchID) {
		// Cancelled: nothing orphaned, a retry is safe.
		return &schemas.BifrostError{
			StatusCode: bifrost.Ptr(503),
			Error: &schemas.ErrorField{
				Type:    bifrost.Ptr("batch_ownership_write_failed"),
				Message: "failed to record batch ownership; the created batch was cancelled — please retry",
			},
		}
	}
	// Could not cancel: a batch may be orphaned upstream. Surface that plainly to
	// the operator rather than pretending it was a clean retryable failure.
	p.logger.Error("[Governance] ORPHANED BATCH: ownership write AND compensating cancel both failed (provider=%s batch=%s); this batch is untracked and may run billable/inaccessible — manual cleanup required", provider, batchID)
	return &schemas.BifrostError{
		StatusCode: bifrost.Ptr(500),
		Error: &schemas.ErrorField{
			Type:    bifrost.Ptr("batch_ownership_write_failed"),
			Message: fmt.Sprintf("failed to record ownership for batch %s and could not cancel it; the batch may be orphaned upstream — contact an operator before retrying", batchID),
		},
	}
}

// compensatingCancelBatch best-effort cancels an upstream batch after a failed
// ownership capture. Returns true only if the cancel definitively succeeded.
// Robust to a nil client and to the cancel path itself panicking.
func (p *GovernancePlugin) compensatingCancelBatch(ctx context.Context, provider schemas.ModelProvider, batchID string) (ok bool) {
	if p.batchClient == nil {
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			p.logger.Error("[Governance] recovered from panic during compensating batch cancel (provider=%s batch=%s): %v", provider, batchID, r)
			ok = false
		}
	}()
	sub := newBatchSubRequestContext(ctx)
	defer sub.Cancel()
	_, cancelErr := p.batchClient.BatchCancelRequest(sub, &schemas.BifrostBatchCancelRequest{
		Provider: provider,
		BatchID:  batchID,
	})
	return cancelErr == nil
}

// batchListCursor is the parsed inbound pagination cursor for an owner-scoped
// batch list: the dialect-native boundary batch id and whether the caller is
// paging backward (Anthropic before_id) vs forward (after / after_id / page
// token).
type batchListCursor struct {
	id     string
	before bool
}

// parseBatchListCursor extracts the inbound pagination cursor from a batch list
// request across dialects: OpenAI `after`, Anthropic `after_id` (forward) /
// `before_id` (backward), Gemini `page_token` / `next_cursor`. A forward cursor
// wins if both are somehow present. Returns (cursor, true) when a cursor id is
// present, (zero, false) for a first page.
//
// The id is trusted as the ledger key only after listOwnedBatchesForCaller
// resolves it through GetManagedBatch and confirms the caller owns it — an
// unowned / unknown id yields an empty page, so a guessed or foreign cursor can
// neither leak nor advance pagination.
func parseBatchListCursor(req *schemas.BifrostBatchListRequest) (batchListCursor, bool) {
	switch {
	case req.After != nil && *req.After != "":
		return batchListCursor{id: *req.After}, true
	case req.AfterID != nil && *req.AfterID != "":
		return batchListCursor{id: *req.AfterID}, true
	case req.PageToken != nil && *req.PageToken != "":
		return batchListCursor{id: *req.PageToken}, true
	case req.NextCursor != nil && *req.NextCursor != "":
		return batchListCursor{id: *req.NextCursor}, true
	case req.BeforeID != nil && *req.BeforeID != "":
		return batchListCursor{id: *req.BeforeID, before: true}, true
	}
	return batchListCursor{}, false
}

// buildBatchListShortCircuit decides whether a batch list request is served from
// the ownership ledger (deep owner-scoped pagination) instead of relaying to the
// shared upstream list. It returns:
//
//   - nil to let the request relay upstream UNCHANGED — when there is no ledger
//     (in-memory-only / unsupported store), no virtual key (single-tenant / SDK
//     path, nothing to scope), or the caller is a batch-ownership admin (entitled
//     to the full cross-tenant list + raw passthrough);
//   - a short-circuit carrying a fully-built owner-scoped list Response otherwise
//     — the leak-proof path: the upstream list endpoint is never called, so no
//     foreign page is fetched. An unknown VK fails closed to an empty page.
//
// This mirrors, at the ledger level, exactly which callers the previous
// single-page post-hook filter scoped vs. passed through.
func (p *GovernancePlugin) buildBatchListShortCircuit(ctx *schemas.BifrostContext, req *schemas.BifrostBatchListRequest) *schemas.LLMPluginShortCircuit {
	if p.batchLedger == nil || req == nil {
		return nil
	}
	// Deep ledger-built pagination needs the in-process lifecycle client to fan out
	// per-id retrieves. In a Go-SDK deployment only the HTTP server injects it, so
	// it may be nil. Without it we do NOT short-circuit to an empty page (that would
	// silently drop a caller's own batches); instead we relay upstream and let the
	// PostLLMHook single-page owner filter scope the result (shallow but leak-proof,
	// the #210 behavior). Isolation holds either way; deep pagination is the
	// client-dependent enhancement.
	if p.batchClient == nil {
		return nil
	}
	provider := req.Provider
	vkValue := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyVirtualKey)
	if vkValue == "" {
		// Single-tenant / SDK path: no identity to scope by, relay upstream.
		return nil
	}
	vk, ok := p.store.GetVirtualKey(ctx, vkValue)
	if !ok || vk == nil {
		// Unknown VK: fail closed to an empty page — never relay the full list.
		return &schemas.LLMPluginShortCircuit{Response: builtBatchListResponse(provider)}
	}
	if p.isBatchOwnershipAdmin(vk.ID) {
		// Admin: entitled to the full upstream list; let it relay untouched.
		return nil
	}
	cursor, hasCursor := parseBatchListCursor(req)
	built, berr := p.listOwnedBatchesForCaller(ctx, provider, callerBatchOwner(vk), cursor, hasCursor, parseBatchListFilters(req))
	if berr != nil {
		return &schemas.LLMPluginShortCircuit{Error: berr}
	}
	return &schemas.LLMPluginShortCircuit{Response: built}
}

// batchListFilters are the result-scoping list filters the SHORT-CIRCUIT must
// re-apply itself, because it skips the upstream call the provider would have
// filtered. Only Bedrock sends any (statusEquals / nameContains, via
// ExtraParams); OpenAI / Anthropic / Gemini carry no result-scoping list filter.
type batchListFilters struct {
	statusEquals string
	nameContains string
}

func (f batchListFilters) active() bool { return f.statusEquals != "" || f.nameContains != "" }

// parseBatchListFilters extracts the result-scoping filters from a list request.
func parseBatchListFilters(req *schemas.BifrostBatchListRequest) batchListFilters {
	f := batchListFilters{}
	if req == nil || req.ExtraParams == nil {
		return f
	}
	if v, ok := req.ExtraParams["statusEquals"].(string); ok {
		f.statusEquals = strings.TrimSpace(v)
	}
	if v, ok := req.ExtraParams["nameContains"].(string); ok {
		f.nameContains = strings.TrimSpace(v)
	}
	return f
}

// matches reports whether a retrieved batch passes the active filters.
func (f batchListFilters) matches(b *schemas.BifrostBatchRetrieveResponse) bool {
	if b == nil {
		return false
	}
	if f.statusEquals != "" && !batchStatusMatches(b.Status, f.statusEquals) {
		return false
	}
	if f.nameContains != "" && !batchNameContains(b, f.nameContains) {
		return false
	}
	return true
}

// batchStatusMatches compares a wanted status against both the bifrost-native
// status and its Bedrock spelling — Bedrock is the only dialect that sends
// statusEquals, and its vocabulary (InProgress / Stopping / Stopped / ...) differs
// from the bifrost-native one (in_progress / cancelling / cancelled / ...).
func batchStatusMatches(status schemas.BatchStatus, want string) bool {
	if strings.EqualFold(string(status), want) {
		return true
	}
	if bedrock := bedrockStatusSpelling(status); bedrock != "" && strings.EqualFold(bedrock, want) {
		return true
	}
	return false
}

// bedrockStatusSpelling mirrors core/providers/bedrock toBedrockBatchStatus so a
// Bedrock ?statusEquals= (Bedrock vocabulary) matches a bifrost-native status
// when the short-circuit assembles the list. Kept in sync with that mapping.
func bedrockStatusSpelling(status schemas.BatchStatus) string {
	switch status {
	case schemas.BatchStatusValidating:
		return "Validating"
	case schemas.BatchStatusInProgress:
		return "InProgress"
	case schemas.BatchStatusCompleted, schemas.BatchStatusEnded:
		return "Completed"
	case schemas.BatchStatusFailed:
		return "Failed"
	case schemas.BatchStatusCancelling:
		return "Stopping"
	case schemas.BatchStatusCancelled:
		return "Stopped"
	case schemas.BatchStatusExpired:
		return "Expired"
	}
	return ""
}

// batchNameContains is a case-insensitive substring match over the batch's name,
// checking both the generic DisplayName and the Bedrock job name
// (Metadata["job_name"], which the Bedrock list converter reads).
func batchNameContains(b *schemas.BifrostBatchRetrieveResponse, want string) bool {
	needle := strings.ToLower(want)
	if b.DisplayName != nil && strings.Contains(strings.ToLower(*b.DisplayName), needle) {
		return true
	}
	if b.Metadata != nil {
		if jobName, ok := b.Metadata["job_name"]; ok && strings.Contains(strings.ToLower(jobName), needle) {
			return true
		}
	}
	return false
}

// ownedBatchRow is the minimal owned-batch keyset row the list assembler needs:
// the batch id and (when the keyset pager provides it) its created_at, used to
// seek the next chunk. The fallback path leaves created_at zero and seeks by id.
type ownedBatchRow struct {
	id        string
	createdAt time.Time
}

// batchListScanCapError fails a filtered list request loud when the owned-batch
// scan would exceed maxBatchListOwnedScan — a sparse filter over a very large
// owned set. Preferred over silently truncating results or falsely advertising
// has_more (either of which would mislead the caller about their own batches).
func batchListScanCapError() *schemas.BifrostError {
	return &schemas.BifrostError{
		StatusCode: bifrost.Ptr(400),
		Error: &schemas.ErrorField{
			Type:    bifrost.Ptr("batch_list_filter_too_sparse"),
			Message: fmt.Sprintf("the batch list filter matched too sparsely across more than %d of your batches to paginate reliably; narrow the filter or list without it", maxBatchListOwnedScan),
		},
	}
}

// listOwnedBatchesForCaller builds one owner-scoped list page entirely from the
// ownership ledger. It resolves the inbound cursor (if any) to a caller-OWNED
// ledger row, then walks the caller's own batches in REVERSE-CHRONOLOGICAL
// (newest-first) keyset order — retrieving a fresh BatchRetrieve per owned id (the
// ledger is a pure ownership index, so live state comes from upstream at read
// time) and applying the request's result filters — until it has collected
// `limit` MATCHES or exhausted the owned set.
//
// has_more reflects the FILTERED result, not raw keyset counts: after the page is
// full it PROBES further owned rows for a (limit+1)th MATCH, and only then reports
// has_more with the last match on the page as the continuation cursor. The probe
// is hard-bounded by maxBatchListOwnedScan (a sparse filter over a huge owned set
// fails loud rather than scanning unboundedly or lying about has_more).
//
// Leak-proof by construction: every id examined is caller-owned, so no Data row,
// first_id, last_id, or next_cursor can carry a foreign id. Continuation-bearing
// fields are emitted ONLY when a further page exists. A retrieve that errors for
// one id is skipped (the seek still advances past it). Returns a BifrostError only
// for the scan-cap case.
func (p *GovernancePlugin) listOwnedBatchesForCaller(
	ctx *schemas.BifrostContext,
	provider schemas.ModelProvider,
	owner configstoreTables.ManagedBatchOwner,
	cursor batchListCursor,
	hasCursor bool,
	filters batchListFilters,
) (*schemas.BifrostResponse, *schemas.BifrostError) {
	limit := resolveBatchListTarget(bifrost.GetIntFromContext(ctx, batchListRequestedLimitKey), 0)

	built := builtBatchListResponse(provider)
	list := built.BatchListResponse

	// Resolve the inbound cursor to a caller-OWNED ledger row. An unowned/unknown
	// cursor id yields an empty page: we neither leak it nor confirm it exists.
	var (
		seekCreatedAt time.Time
		seekID        string
		seek          bool
	)
	if hasCursor {
		row, err := p.batchLedger.GetManagedBatch(ctx, string(provider), cursor.id)
		if err != nil {
			p.logger.Error("[Governance] batch list cursor lookup failed for provider=%s: %v; returning empty page", provider, err)
			return built, nil
		}
		if row == nil || !owner.Matches(row.OwnerVirtualKeyID, row.OwnerTeamID, row.OwnerCustomerID) {
			return built, nil
		}
		seekCreatedAt = row.CreatedAt
		seekID = row.BatchID
		seek = true
	}

	filterActive := filters.active()
	// Forward pages pull a large chunk per round-trip (a sparse filter retrieves +
	// tests each row); backward ("before") must fetch exactly the page size so the
	// keyset returns the `limit` rows CLOSEST to the cursor (not the newest), and it
	// is single-chunk by construction.
	fetchSize := batchListFetchChunk
	if cursor.before {
		fetchSize = limit
	}
	data := make([]schemas.BifrostBatchRetrieveResponse, 0, limit)
	var lastPageID string // id of the last MATCH on the page = the continuation cursor
	hasMore := false
	scanned := 0      // owned rows retrieved+examined (bounded by maxBatchListOwnedScan)
	pageFull := false // filter path: once true we retrieve only to PROBE for one more match

fetch:
	for {
		rows, moreBeyond, err := p.pageOwnedRows(ctx, provider, owner, seekCreatedAt, seekID, seek, cursor.before, fetchSize)
		if err != nil {
			p.logger.Error("[Governance] batch list keyset query failed for provider=%s: %v; returning empty page", provider, err)
			return builtBatchListResponse(provider), nil
		}
		if len(rows) == 0 {
			hasMore = false // owned set exhausted
			break
		}
		for i := range rows {
			// Advance the seek cursor to this row BEFORE keeping/skipping it.
			seekCreatedAt, seekID, seek = rows[i].createdAt, rows[i].id, true

			if !pageFull {
				rb := p.retrieveOwnedBatch(ctx, provider, rows[i].id)
				scanned++
				if rb == nil {
					continue // retrieve error: skip (already advanced past it)
				}
				if filterActive && !filters.matches(rb) {
					if scanned >= maxBatchListOwnedScan {
						return nil, batchListScanCapError()
					}
					continue
				}
				data = append(data, *rb)
				lastPageID = rows[i].id
				if len(data) == limit {
					if !filterActive {
						// Every owned row matches; has_more is exact from the keyset
						// without any further retrieves.
						hasMore = i < len(rows)-1 || moreBeyond
						break fetch
					}
					pageFull = true // page filled; now PROBE for a (limit+1)th match
				}
				continue
			}

			// PROBE phase (filter active): retrieve only to learn whether a further
			// MATCH exists; these rows are NOT added to the page.
			rb := p.retrieveOwnedBatch(ctx, provider, rows[i].id)
			scanned++
			if rb != nil && filters.matches(rb) {
				hasMore = true // a further match exists => a genuine next page
				break fetch
			}
			if scanned >= maxBatchListOwnedScan {
				return nil, batchListScanCapError()
			}
		}
		if cursor.before {
			// Backward (before_id) paging is single-chunk: the fetch chunk is >= the
			// page size, and the only multi-chunk case — a sparse forward filter —
			// cannot co-occur with before_id (no filtering dialect sends it). Avoid
			// an ill-defined backward re-seek across chunks.
			break
		}
		if !moreBeyond {
			hasMore = false // exhausted: page not full, or probe found no further match
			break
		}
		if scanned >= maxBatchListOwnedScan {
			return nil, batchListScanCapError()
		}
	}

	list.Data = data
	list.HasMore = hasMore
	if len(data) > 0 {
		first := data[0].ID
		list.FirstID = &first // display only; never a continuation token in any dialect
		// last_id / next_cursor ARE continuation-bearing in several dialects
		// (Bedrock NextToken = last_id; Gemini/Vertex NextPageToken = next_cursor),
		// so emit them ONLY when a further page exists — otherwise a terminal page
		// hands back a token that fetches an empty page. Backward paging continues
		// via first_id, so it emits last_id (Bedrock-style token) but not next_cursor.
		if hasMore {
			last := lastPageID
			list.LastID = &last
			if !cursor.before {
				list.NextCursor = &last
			}
		}
	}
	return built, nil
}

// pageOwnedRows returns one ordered chunk of the caller's owned batch rows plus
// whether the keyset has more rows beyond it, in REVERSE-CHRONOLOGICAL
// (newest-first) order to match native batch-list APIs. It uses the keyset pager
// when the store provides it; otherwise it FALLS BACK to the owned-id list
// (ManagedBatchStore.ListOwnedBatchIDs), ordering + seeking by batch id DESC. The
// fallback is leak-proof (only owned ids are returned) and keeps tenant isolation
// enforced on a store that implements ownership but not pagination — enforcement
// is never disabled for lack of a pager.
func (p *GovernancePlugin) pageOwnedRows(
	ctx *schemas.BifrostContext,
	provider schemas.ModelProvider,
	owner configstoreTables.ManagedBatchOwner,
	seekCreatedAt time.Time,
	seekID string,
	seek, before bool,
	limit int,
) ([]ownedBatchRow, bool, error) {
	if limit <= 0 {
		limit = 1
	}
	if p.batchPager != nil {
		rows, hasMore, err := p.batchPager.ListOwnedBatchIDsPage(ctx, string(provider), owner, seekCreatedAt, seekID, seek, before, limit)
		if err != nil {
			return nil, false, err
		}
		out := make([]ownedBatchRow, len(rows))
		for i := range rows {
			out[i] = ownedBatchRow{id: rows[i].BatchID, createdAt: rows[i].CreatedAt}
		}
		return out, hasMore, nil
	}

	// Fallback: no keyset pager. Order + seek by batch id, newest-first (id DESC).
	ids, err := p.batchLedger.ListOwnedBatchIDs(ctx, string(provider), owner)
	if err != nil {
		return nil, false, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	filtered := make([]string, 0, len(ids))
	for _, id := range ids {
		if seek {
			if before && id <= seekID {
				continue // "before" wants ids NEWER than the cursor (id > cursor)
			}
			if !before && id >= seekID {
				continue // forward wants ids OLDER than the cursor (id < cursor)
			}
		}
		filtered = append(filtered, id)
	}
	hasMore := len(filtered) > limit
	if before {
		// The `limit` rows closest to the cursor = the smallest ids among those
		// newer than it = the tail of the DESC list (still newest-first among itself).
		if len(filtered) > limit {
			filtered = filtered[len(filtered)-limit:]
		}
	} else if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	out := make([]ownedBatchRow, len(filtered))
	for i, id := range filtered {
		out[i] = ownedBatchRow{id: id} // no created_at in the fallback; seek is by id
	}
	return out, hasMore, nil
}

// retrieveOwnedBatch issues one in-process BatchRetrieve for an owned batch id
// while assembling the owner-scoped list page. It uses the same isolated
// sub-request context as the compensating cancel (skip the plugin pipeline so it
// does not re-enter the gate; tombstone the caller's transport state so the
// retrieve is not misrouted to the list path). Returns nil (skip this row) on a
// nil client, a retrieve error, or a panic — a single bad id must not tank the
// whole page.
func (p *GovernancePlugin) retrieveOwnedBatch(ctx context.Context, provider schemas.ModelProvider, batchID string) (rb *schemas.BifrostBatchRetrieveResponse) {
	if p.batchClient == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			p.logger.Error("[Governance] recovered from panic during owner-scoped batch retrieve (provider=%s batch=%s): %v", provider, batchID, r)
			rb = nil
		}
	}()
	sub := newBatchSubRequestContext(ctx)
	defer sub.Cancel()
	resp, retErr := p.batchClient.BatchRetrieveRequest(sub, &schemas.BifrostBatchRetrieveRequest{
		Provider: provider,
		BatchID:  batchID,
	})
	if retErr != nil {
		p.logger.Warn("[Governance] owner-scoped batch retrieve failed (provider=%s batch=%s); skipping from list page", provider, batchID)
		return nil
	}
	return resp
}

// builtBatchListResponse returns a fresh, empty owner-scoped list response for a
// provider: object "list", a non-nil empty Data slice, no cursor, and no raw
// passthrough (so every dialect converter rebuilds from Data). It is the base for
// both the assembled page and the fail-closed empty page.
func builtBatchListResponse(provider schemas.ModelProvider) *schemas.BifrostResponse {
	return &schemas.BifrostResponse{
		BatchListResponse: &schemas.BifrostBatchListResponse{
			Object:      "list",
			Data:        []schemas.BifrostBatchRetrieveResponse{},
			ExtraFields: schemas.BifrostResponseExtraFields{Provider: provider},
		},
	}
}

// filterBatchListForCaller is the FALLBACK used only when no lifecycle client is
// available (Go-SDK deployments), so the list request was relayed upstream and
// this narrows the SINGLE fetched page to the caller's ledger-known rows: it keeps
// only owned rows, sets first_id/last_id to the caller's own ids, DROPS the
// pagination cursor, and suppresses the raw passthrough so the dialect converter
// rebuilds from the filtered Data. Admin keys are exempt (full list, raw kept).
// Fails closed to an empty list on a missing VK or ledger error — never leaks the
// full upstream list. Shallow (one page only), but leak-proof; deep pagination is
// the client-dependent enhancement served by the short-circuit path.
func (p *GovernancePlugin) filterBatchListForCaller(
	ctx *schemas.BifrostContext,
	result *schemas.BifrostResponse,
	provider schemas.ModelProvider,
	vkValue string,
) {
	list := result.BatchListResponse
	if list == nil {
		return
	}
	if provider == "" {
		provider = list.ExtraFields.Provider
	}
	if vkValue == "" {
		return // single-tenant / SDK path: nothing to scope
	}
	vk, ok := p.store.GetVirtualKey(ctx, vkValue)
	if !ok || vk == nil {
		emptyBatchList(list) // unknown VK: fail closed
		return
	}
	if p.isBatchOwnershipAdmin(vk.ID) {
		return // admin: full list, raw preserved
	}

	ownedIDs, err := p.batchLedger.ListOwnedBatchIDs(ctx, string(provider), callerBatchOwner(vk))
	if err != nil {
		p.logger.Error("[Governance] batch list ownership lookup failed for provider=%s: %v; returning empty list", provider, err)
		emptyBatchList(list)
		return
	}
	owned := make(map[string]struct{}, len(ownedIDs))
	for _, id := range ownedIDs {
		owned[id] = struct{}{}
	}

	capHint := resolveBatchListTarget(bifrost.GetIntFromContext(ctx, batchListRequestedLimitKey), 0)
	filtered := make([]schemas.BifrostBatchRetrieveResponse, 0, capHint)
	for i := range list.Data {
		if _, isOwned := owned[list.Data[i].ID]; isOwned {
			filtered = append(filtered, list.Data[i])
		}
	}
	list.Data = filtered
	if len(filtered) == 0 {
		list.FirstID = nil
		list.LastID = nil
	} else {
		firstOwned := filtered[0].ID
		lastOwned := filtered[len(filtered)-1].ID
		list.FirstID = &firstOwned
		list.LastID = &lastOwned
	}
	list.HasMore = false
	list.NextCursor = nil
	list.ExtraFields.RawResponse = nil
}

// emptyBatchList fail-closes a list response to zero rows with no cursor and no
// raw passthrough.
func emptyBatchList(list *schemas.BifrostBatchListResponse) {
	list.Data = []schemas.BifrostBatchRetrieveResponse{}
	list.HasMore = false
	list.FirstID = nil
	list.LastID = nil
	list.NextCursor = nil
	list.ExtraFields.RawResponse = nil
}
