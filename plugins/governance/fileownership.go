package governance

import (
	"context"
	"fmt"
	"strings"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
)

// Per-tenant file ownership enforcement — the file-lifecycle sibling of
// batchownership.go, closing the SAME leak class for files.
//
// File lifecycle calls (list / retrieve / delete / content-download) relay to
// the SHARED upstream provider credentials with zero per-tenant scoping. Left
// unscoped, any valid virtual key can enumerate the whole shared workspace's
// files (list) or read metadata, DELETE, or — most damagingly — DOWNLOAD THE
// CONTENT of any file whose id it learns or guesses
// (GET /v1/files/{id}/content). Because batch results are delivered as files,
// this is also how a caller could exfiltrate another tenant's batch output. This
// file closes that gap using an upload-time ownership ledger
// (configstore.TableManagedFile):
//
//   - UPLOAD  (PostLLMHook): record (provider, upstream file id) -> owner tuple.
//     A ledger-write failure attempts a best-effort compensating delete of the
//     just-uploaded file, then fails the request — a file that exists upstream
//     but is unowned is unreachable under the deny-unknown policy, which is
//     strictly worse than an upload that never happened.
//   - LIST    (PreLLMHook SHORT-CIRCUIT, when a keyset pager + core client are
//     available): the upstream list endpoint is NEVER called for a scoped
//     (non-admin) caller; the page is built from the ownership ledger with a deep
//     keyset over ALL of the caller's own files, then a fresh FileRetrieve is
//     fanned out per owned id to assemble live state. When the pager or client is
//     unavailable, the request relays upstream and the returned single page is
//     FILTERED against the ledger in the PostLLMHook (leak-proof, just not deep).
//     Either way isolation is enforced — it is never disabled.
//   - PER-ID  (PreLLMHook): look up the owner and deny (404, existence-hiding)
//     when the caller is not the owner, BEFORE the upstream relay. Ledger-absent
//     ids follow the shared UnknownBatchIDPolicy (deny by default).
//
// Ownership match, the admin bypass, and the unknown-id policy are DELIBERATELY
// SHARED with the batch ledger (ManagedBatchOwner, isBatchOwnershipAdmin,
// resolveUnknownBatchIDPolicy): a tenant is the same tenant whether it owns a
// batch or a file, and an operator configures one lifecycle-ownership posture,
// not two.
//
// BATCH <-> FILE LINKAGE: a batch's input file is uploaded out of band and its
// output/error files only materialize at a terminal state, so ownership of them
// is captured by mirroring the batch owner onto those file ids — the input file
// when a batch is created, and the output/error files when a batch retrieve
// carries them. This lets a batch owner (and only the batch owner) fetch its own
// results via GET /v1/files/{output_file_id}/content; a foreign caller gets 404.

const (
	// defaultFileListLimit is the pre-allocation hint when the caller did not
	// specify a page size on a file list.
	defaultFileListLimit = 20

	// maxFileListPageSize clamps the caller-controlled file-list page size on the
	// DEEP (short-circuit) path, mirroring the batch ceiling. The transport parses
	// ?limit= unbounded, so an absurd value would otherwise drive an unbounded
	// slice allocation. Deep pagination continues past the clamp via the `after`
	// continuation cursor, so this bounds memory without truncating results.
	maxFileListPageSize = 100

	// fileListFetchChunk is how many owned rows the deep-list assembler pulls from
	// the ledger per keyset round-trip. Larger than a typical page so a filtered
	// scan (which retrieves + tests each row against the purpose filter) does not do
	// one DB round-trip per row. Mirrors batchListFetchChunk.
	fileListFetchChunk = 100

	// maxFileListOwnedScan hard-bounds how many owned files a single list request
	// will retrieve+examine while assembling a page under an active purpose filter.
	// Without it, a purpose that matches sparsely over a large owned set could scan
	// unboundedly. On hitting it we FAIL LOUD (fileListScanCapError) rather than
	// falsely advertise has_more or silently truncate the caller's own results.
	// Mirrors maxBatchListOwnedScan.
	maxFileListOwnedScan = 2000

	// fileListRequestedLimitKey carries the caller's requested file-list page size
	// from PreLLMHook to the owner-scoped ledger paging (allocation-capacity hint).
	fileListRequestedLimitKey schemas.BifrostContextKey = "bf-governance-file-list-limit"

	// fileListFilterInPostKey marks a file-list request that must have its single
	// upstream page filtered against the ledger in the PostLLMHook — the fallback
	// used when a keyset pager or the core client is unavailable, so the deep
	// short-circuit cannot run but isolation must still be enforced.
	fileListFilterInPostKey schemas.BifrostContextKey = "bf-governance-file-list-post-filter"
)

// fileLedger is the subset of the config store the file-ownership gate depends on
// (identical to configstore.ManagedFileStore). It is populated by type-asserting
// the plugin's config store to the optional ledger capability, so a store that
// does not support the ledger simply leaves it nil. Tests inject a fake.
type fileLedger interface {
	RegisterManagedFile(ctx context.Context, file *configstoreTables.TableManagedFile, tx ...*gorm.DB) error
	GetManagedFile(ctx context.Context, provider, fileID string, tx ...*gorm.DB) (*configstoreTables.TableManagedFile, error)
}

// filePager is the OPTIONAL keyset-pagination capability
// (configstore.ManagedFilePager), asserted SEPARATELY from fileLedger so a store
// that implements ownership but not pagination still enforces isolation (via the
// single-upstream-page filter in the post-hook). When present, deep owner-scoped
// file list pagination runs over the (created_at, file_id) keyset.
type filePager interface {
	ListOwnedFileIDsPage(ctx context.Context, provider string, owner configstoreTables.ManagedBatchOwner, cursorCreatedAt time.Time, cursorID string, hasCursor, asc bool, limit int, tx ...*gorm.DB) ([]configstoreTables.TableManagedFile, bool, error)
}

// fileProvenanceLinker is the OPTIONAL provenance capability
// (configstore.ManagedFileProvenanceStore), asserted SEPARATELY from fileLedger so
// a store that implements ownership but not provenance still enforces isolation
// (input-file provenance is simply skipped). It stamps SourceBatchID onto an
// EXISTING owned file row and can never mint one — the mechanism that lets a batch
// input file gain batch provenance WITHOUT batch-create ever granting ownership of
// an unowned file.
type fileProvenanceLinker interface {
	SetFileSourceBatch(ctx context.Context, provider, fileID, sourceBatchID string, owner configstoreTables.ManagedBatchOwner, tx ...*gorm.DB) (bool, error)
}

// configureFileOwnership wires the file ledger and (optional) keyset pager by
// type-asserting the config store to the optional capabilities, mirroring
// configureBatchOwnership. The operator knobs (admin VK list, unknown-id policy)
// are NOT re-read here: the file gate REUSES the shared batch knobs
// (p.batchAdminVKIDs / p.batchUnknownIDPolicy), which configureBatchOwnership
// already binds to the live ClientConfig fields.
func (p *GovernancePlugin) configureFileOwnership(configStore configstore.ConfigStore) {
	if configStore == nil {
		return
	}
	if ledger, ok := configStore.(configstore.ManagedFileStore); ok {
		p.fileLedger = ledger
	} else if p.logger != nil {
		p.logger.Warn("[Governance] config store does not support the file-ownership ledger; per-tenant file scoping is DISABLED for this store")
	}
	if pager, ok := configStore.(configstore.ManagedFilePager); ok {
		p.filePager = pager
	} else if p.fileLedger != nil && p.logger != nil {
		p.logger.Warn("[Governance] config store implements the file-ownership ledger but not the keyset pager; file list falls back to a single-upstream-page filter (isolation stays enforced)")
	}
	// Provenance-linking is a SEPARATE optional capability. A store that implements
	// ownership but not provenance still enforces isolation; input-file provenance
	// (SourceBatchID) is simply skipped — never a reason to mint an ownership row.
	if prov, ok := configStore.(configstore.ManagedFileProvenanceStore); ok {
		p.fileProvenance = prov
	}
}

// isFilePerIDVerb reports whether a request type targets a single, already
// existing file by id (as opposed to upload or list). Content-download is the
// most sensitive of the three — it returns the raw file bytes.
func isFilePerIDVerb(rt schemas.RequestType) bool {
	switch rt {
	case schemas.FileRetrieveRequest, schemas.FileDeleteRequest, schemas.FileContentRequest:
		return true
	}
	return false
}

// fileTargetFromRequest extracts the provider and upstream (native) file id
// addressed by a per-id file verb. Returns empty strings for any other request
// type. The id is the decoded provider-native id (the transport applies
// decodeStorageFileID before the core call), matching the ledger key.
func fileTargetFromRequest(req *schemas.BifrostRequest) (schemas.ModelProvider, string) {
	switch req.RequestType {
	case schemas.FileRetrieveRequest:
		if req.FileRetrieveRequest != nil {
			return req.FileRetrieveRequest.Provider, req.FileRetrieveRequest.FileID
		}
	case schemas.FileDeleteRequest:
		if req.FileDeleteRequest != nil {
			return req.FileDeleteRequest.Provider, req.FileDeleteRequest.FileID
		}
	case schemas.FileContentRequest:
		if req.FileContentRequest != nil {
			return req.FileContentRequest.Provider, req.FileContentRequest.FileID
		}
	}
	return "", ""
}

// fileNotFoundError builds the existence-hiding 404 returned for both
// foreign-owned and (under deny policy) ledger-absent file ids. Like
// batchNotFoundError it deliberately does NOT distinguish "exists but not yours"
// from "does not exist": a 403 would confirm the id belongs to someone.
func fileNotFoundError() *schemas.BifrostError {
	return &schemas.BifrostError{
		Type:       bifrost.Ptr("not_found_error"),
		StatusCode: bifrost.Ptr(404),
		Error: &schemas.ErrorField{
			Type:    bifrost.Ptr("not_found_error"),
			Message: "file not found",
		},
	}
}

// enforceFileOwnershipPreHook gates per-id file verbs (retrieve / delete /
// content-download) before the upstream relay. Returns a 404 BifrostError to
// short-circuit when the caller is not the owner (or the id is unknown under deny
// policy); nil to allow. Mirrors enforceBatchOwnershipPreHook exactly.
func (p *GovernancePlugin) enforceFileOwnershipPreHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) *schemas.BifrostError {
	if req == nil || !isFilePerIDVerb(req.RequestType) {
		return nil
	}
	if p.fileLedger == nil {
		return nil
	}

	provider, fileID := fileTargetFromRequest(req)
	if fileID == "" {
		return nil
	}

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

	owner, err := p.fileLedger.GetManagedFile(ctx, string(provider), fileID)
	if err != nil {
		// Fail closed on a ledger read error: deny rather than relay upstream with
		// no ownership check.
		p.logger.Error("[Governance] file ownership lookup failed for provider=%s: %v; denying %s", provider, err, req.RequestType)
		return fileNotFoundError()
	}
	if owner == nil {
		// Absent from the ledger: uploaded before this ledger existed or out of
		// band. The shared lifecycle policy decides; deny by default.
		if p.resolveUnknownBatchIDPolicy() == UnknownBatchIDPolicyAllow {
			return nil
		}
		return fileNotFoundError()
	}
	if callerBatchOwner(vk).Matches(owner.OwnerVirtualKeyID, owner.OwnerTeamID, owner.OwnerCustomerID) {
		return nil
	}
	// Foreign owner: always denied, existence-hidden, regardless of policy.
	return fileNotFoundError()
}

// applyFileOwnershipPostHook is the file-ledger counterpart of
// applyBatchOwnershipPostHook. It:
//   - captures ownership on a successful file UPLOAD (fatal on ledger failure,
//     with a compensating delete);
//   - LINKS batch input/output/error files to the batch owner on a successful
//     batch create / retrieve (best-effort, never fatal);
//   - FILTERS a single upstream file-list page against the ledger when the deep
//     short-circuit could not run (the fallback path flagged in PreLLMHook).
//
// Returns the (possibly mutated) result and a non-nil BifrostError only when the
// upload-time ledger write fails (which must fail the request).
func (p *GovernancePlugin) applyFileOwnershipPostHook(
	ctx *schemas.BifrostContext,
	result *schemas.BifrostResponse,
	requestType schemas.RequestType,
	provider schemas.ModelProvider,
	vkValue string,
) (*schemas.BifrostResponse, *schemas.BifrostError) {
	if result == nil || p.fileLedger == nil {
		return result, nil
	}
	switch requestType {
	case schemas.FileUploadRequest:
		if berr := p.captureFileOwnership(ctx, result, provider, vkValue); berr != nil {
			return nil, berr
		}
	case schemas.BatchCreateRequest:
		p.linkBatchCreateFiles(ctx, result, provider)
	case schemas.BatchRetrieveRequest:
		p.linkBatchRetrieveFiles(ctx, result, provider)
	case schemas.BatchListRequest:
		p.linkBatchListFiles(ctx, result, provider)
	case schemas.FileListRequest:
		if bifrost.GetBoolFromContext(ctx, fileListFilterInPostKey) {
			p.filterFileListForCaller(ctx, result, provider, vkValue)
		}
	}
	return result, nil
}

// captureFileOwnership writes the ownership row for a freshly uploaded file. The
// response id is the upstream provider id in its native (decoded) form — the
// transport applies encodeStorageFileID only AFTER the post-hook returns — so the
// ledger key matches what per-id verbs resolve to. Mirrors captureBatchOwnership,
// including the compensating-delete-on-failure protocol.
func (p *GovernancePlugin) captureFileOwnership(
	ctx *schemas.BifrostContext,
	result *schemas.BifrostResponse,
	provider schemas.ModelProvider,
	vkValue string,
) *schemas.BifrostError {
	if result.FileUploadResponse == nil || result.FileUploadResponse.ID == "" {
		return nil
	}
	if provider == "" {
		provider = result.FileUploadResponse.ExtraFields.Provider
	}
	if vkValue == "" {
		// No identity to attribute ownership to; nothing to record and nothing to
		// protect (single-tenant / SDK path).
		return nil
	}
	vk, ok := p.store.GetVirtualKey(ctx, vkValue)
	if !ok || vk == nil {
		return nil
	}
	owner := callerBatchOwner(vk)
	fileID := result.FileUploadResponse.ID
	row := &configstoreTables.TableManagedFile{
		Provider:          string(provider),
		FileID:            fileID,
		OwnerVirtualKeyID: owner.VirtualKeyID,
		OwnerTeamID:       owner.TeamID,
		OwnerCustomerID:   owner.CustomerID,
		OwnerUserID:       owner.UserID,
		Purpose:           string(result.FileUploadResponse.Purpose),
	}
	err := p.fileLedger.RegisterManagedFile(ctx, row)
	if err == nil {
		return nil
	}
	// The provider already stored the file but we could not record who owns it.
	// Returning a plain error would let the client retry and create a SECOND
	// upstream file while this one sits orphaned and (under deny-unknown)
	// inaccessible to everyone. Attempt a best-effort compensating delete so a
	// retry is clean.
	p.logger.Error("[Governance] failed to record file ownership (provider=%s file=%s): %v", provider, fileID, err)
	if p.compensatingDeleteFile(ctx, provider, fileID) {
		return &schemas.BifrostError{
			StatusCode: bifrost.Ptr(503),
			Error: &schemas.ErrorField{
				Type:    bifrost.Ptr("file_ownership_write_failed"),
				Message: "failed to record file ownership; the uploaded file was deleted — please retry",
			},
		}
	}
	p.logger.Error("[Governance] ORPHANED FILE: ownership write AND compensating delete both failed (provider=%s file=%s); this file is untracked and may be inaccessible — manual cleanup required", provider, fileID)
	return &schemas.BifrostError{
		StatusCode: bifrost.Ptr(500),
		Error: &schemas.ErrorField{
			Type:    bifrost.Ptr("file_ownership_write_failed"),
			Message: fmt.Sprintf("failed to record ownership for file %s and could not delete it; the file may be orphaned upstream — contact an operator before retrying", fileID),
		},
	}
}

// compensatingDeleteFile best-effort deletes an upstream file after a failed
// ownership capture. Returns true only if the delete definitively succeeded.
// Robust to a nil client and to the delete path itself panicking. Mirrors
// compensatingCancelBatch.
func (p *GovernancePlugin) compensatingDeleteFile(ctx context.Context, provider schemas.ModelProvider, fileID string) (ok bool) {
	if p.batchClient == nil {
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			p.logger.Error("[Governance] recovered from panic during compensating file delete (provider=%s file=%s): %v", provider, fileID, r)
			ok = false
		}
	}()
	sub := newBatchSubRequestContext(ctx)
	defer sub.Cancel()
	_, delErr := p.batchClient.FileDeleteRequest(sub, &schemas.BifrostFileDeleteRequest{
		Provider: provider,
		FileID:   fileID,
	})
	return delErr == nil
}

// linkBatchCreateFiles records batch->file provenance on a successful batch
// create: the input file (uploaded out of band) is claimed for the batch owner,
// as are any output/error files already present on the create response (rare, but
// some dialects populate them). Best-effort — a link failure only means the owner
// may not reach that file yet (fail-closed / safe), so it never fails the batch.
func (p *GovernancePlugin) linkBatchCreateFiles(ctx *schemas.BifrostContext, result *schemas.BifrostResponse, provider schemas.ModelProvider) {
	resp := result.BatchCreateResponse
	if resp == nil || resp.ID == "" {
		return
	}
	if provider == "" {
		provider = resp.ExtraFields.Provider
	}
	p.linkBatchFiles(ctx, provider, resp.ID, resp.InputFileID, resp.OutputFileID, resp.ErrorFileID)
}

// linkBatchRetrieveFiles records batch->file provenance on a successful batch
// retrieve: the output and error files only materialize once a batch reaches a
// terminal state, so they are claimed for the batch owner here. Best-effort.
func (p *GovernancePlugin) linkBatchRetrieveFiles(ctx *schemas.BifrostContext, result *schemas.BifrostResponse, provider schemas.ModelProvider) {
	resp := result.BatchRetrieveResponse
	if resp == nil || resp.ID == "" {
		return
	}
	if provider == "" {
		provider = resp.ExtraFields.Provider
	}
	p.linkBatchFiles(ctx, provider, resp.ID, resp.InputFileID, resp.OutputFileID, resp.ErrorFileID)
}

// linkBatchListFiles links the output/error (and input-provenance) files of EVERY
// batch on a list response, owner-resolved per batch from the batch ledger — the
// same linkage the retrieve path performs, applied to each listed batch. Without
// it, an output file first observed via a batch LIST (rather than a retrieve) would
// get no ledger row, so the owner's own GET /v1/files/{output}/content would 404
// until a later per-id retrieve self-healed the link. Best-effort per batch.
func (p *GovernancePlugin) linkBatchListFiles(ctx *schemas.BifrostContext, result *schemas.BifrostResponse, provider schemas.ModelProvider) {
	resp := result.BatchListResponse
	if resp == nil {
		return
	}
	if provider == "" {
		provider = resp.ExtraFields.Provider
	}
	for i := range resp.Data {
		b := &resp.Data[i]
		if b.ID == "" {
			continue
		}
		p.linkBatchFiles(ctx, provider, b.ID, b.InputFileID, b.OutputFileID, b.ErrorFileID)
	}
}

// linkBatchFiles records batch<->file provenance for the OWNER OF THE BATCH
// (resolved from the batch ledger, NOT the caller — so an admin retrieving another
// tenant's batch attributes the files to the real owner). The input file and the
// output/error files are handled DIFFERENTLY, on purpose:
//
//   - OUTPUT / ERROR files are materialized BY the provider from the batch run, so
//     the batch owner is their rightful first owner: they are minted via
//     registerLinkedFile (idempotent — a pre-existing row keeps its first owner).
//   - The INPUT file is uploaded OUT OF BAND and already owns a ledger row from
//     that upload (captureFileOwnership). It is NEVER minted here: minting an
//     ownership row from a batch-create would let a caller claim ownership of a
//     file it merely NAMED in a batch (an untracked id, or another tenant's file),
//     a real cross-tenant escalation. Instead we only STAMP provenance on a row the
//     batch owner ALREADY owns (linkInputFileProvenance) — never creating one.
//
// Requires the batch ledger to resolve the owner; skips silently when the batch is
// not owned/known.
func (p *GovernancePlugin) linkBatchFiles(ctx *schemas.BifrostContext, provider schemas.ModelProvider, batchID string, inputFileID string, outputFileID, errorFileID *string) {
	if p.fileLedger == nil || p.batchLedger == nil || batchID == "" {
		return
	}
	if inputFileID == "" && (outputFileID == nil || *outputFileID == "") && (errorFileID == nil || *errorFileID == "") {
		return
	}
	batchRow, err := p.batchLedger.GetManagedBatch(ctx, string(provider), batchID)
	if err != nil {
		p.logger.Warn("[Governance] batch->file link: batch owner lookup failed (provider=%s batch=%s): %v; skipping", provider, batchID, err)
		return
	}
	if batchRow == nil {
		// Unknown/unowned batch (e.g. pre-ledger, or admin viewing an untracked
		// batch): nothing to attribute the files to.
		return
	}
	owner := configstoreTables.ManagedBatchOwner{
		VirtualKeyID: batchRow.OwnerVirtualKeyID,
		TeamID:       batchRow.OwnerTeamID,
		CustomerID:   batchRow.OwnerCustomerID,
		UserID:       batchRow.OwnerUserID,
	}
	p.linkInputFileProvenance(ctx, provider, inputFileID, owner, batchID)
	if outputFileID != nil {
		p.registerLinkedFile(ctx, provider, *outputFileID, owner, batchID, schemas.FilePurposeBatchOutput)
	}
	if errorFileID != nil {
		p.registerLinkedFile(ctx, provider, *errorFileID, owner, batchID, schemas.FilePurposeBatchOutput)
	}
}

// linkInputFileProvenance stamps SourceBatchID onto the input file's EXISTING
// owner-matched ledger row (written at upload time) WITHOUT ever minting one. This
// is the security-critical asymmetry with output/error linkage: because all
// tenants share upstream credentials, a caller can create a batch that references a
// file id it does not own; minting ownership of that id here would hand the caller
// a file it never uploaded (an untracked id otherwise 404s to everyone — a real
// escalation). By updating only a row the batch owner already owns, batch-create
// can never grant ownership of a file the caller did not already own. When the
// store lacks the provenance capability, or the input file has no caller-owned row,
// this is a silent no-op (isolation is unaffected — the marker is diagnostic).
func (p *GovernancePlugin) linkInputFileProvenance(ctx *schemas.BifrostContext, provider schemas.ModelProvider, fileID string, owner configstoreTables.ManagedBatchOwner, batchID string) {
	if fileID == "" || owner.VirtualKeyID == "" || p.fileProvenance == nil {
		return
	}
	linked, err := p.fileProvenance.SetFileSourceBatch(ctx, string(provider), fileID, batchID, owner)
	if err != nil {
		p.logger.Warn("[Governance] batch->file link: input-file provenance update failed (provider=%s file=%s batch=%s): %v", provider, fileID, batchID, err)
		return
	}
	if !linked {
		// No row the batch owner owns for this input id: it was not uploaded by (or
		// shared with) the batch owner. Deliberately do NOT mint one — batch-create
		// must never grant ownership of an unowned file.
		p.logger.Debug("[Governance] batch->file link: input file %s has no owner-matched ledger row; leaving ownership untouched (batch=%s)", fileID, batchID)
	}
}

// registerLinkedFile writes one best-effort batch-provenance file-ownership row.
// A failure is logged, not surfaced — the batch itself is already owned and
// safe; an unrecorded output file is merely unreachable (fail-closed), which a
// later batch retrieve can re-attempt.
func (p *GovernancePlugin) registerLinkedFile(ctx *schemas.BifrostContext, provider schemas.ModelProvider, fileID string, owner configstoreTables.ManagedBatchOwner, batchID string, purpose schemas.FilePurpose) {
	if fileID == "" || owner.VirtualKeyID == "" {
		return
	}
	row := &configstoreTables.TableManagedFile{
		Provider:          string(provider),
		FileID:            fileID,
		OwnerVirtualKeyID: owner.VirtualKeyID,
		OwnerTeamID:       owner.TeamID,
		OwnerCustomerID:   owner.CustomerID,
		OwnerUserID:       owner.UserID,
		Purpose:           string(purpose),
		SourceBatchID:     batchID,
	}
	if err := p.fileLedger.RegisterManagedFile(ctx, row); err != nil {
		p.logger.Warn("[Governance] batch->file link: failed to record file ownership (provider=%s file=%s batch=%s): %v", provider, fileID, batchID, err)
	}
}

// buildFileListShortCircuit decides whether a file list request is served from
// the ownership ledger (deep owner-scoped pagination) instead of relaying to the
// shared upstream list. It returns:
//
//   - nil to let the request relay upstream — when there is no ledger, no virtual
//     key (single-tenant / SDK), or the caller is a lifecycle admin (entitled to
//     the full cross-tenant list). When the caller IS a scoped tenant but no
//     keyset pager or core client is available, it also returns nil AFTER flagging
//     the request for a single-upstream-page filter in the post-hook — isolation
//     is enforced there instead of here;
//   - a short-circuit carrying a fully-built owner-scoped list Response otherwise
//     — the leak-proof deep path. An unknown VK fails closed to an empty page.
func (p *GovernancePlugin) buildFileListShortCircuit(ctx *schemas.BifrostContext, req *schemas.BifrostFileListRequest) *schemas.LLMPluginShortCircuit {
	if p.fileLedger == nil || req == nil {
		return nil
	}
	provider := req.Provider
	vkValue := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyVirtualKey)
	if vkValue == "" {
		return nil // single-tenant / SDK path
	}
	vk, ok := p.store.GetVirtualKey(ctx, vkValue)
	if !ok || vk == nil {
		// Unknown VK: fail closed to an empty page — never relay the full list.
		return &schemas.LLMPluginShortCircuit{Response: builtFileListResponse(provider)}
	}
	if p.isBatchOwnershipAdmin(vk.ID) {
		return nil // admin: entitled to the full upstream list
	}
	if p.filePager == nil || p.batchClient == nil {
		// No deep-paging capability: relay upstream and filter the returned single
		// page against the ledger in the post-hook. Isolation stays enforced.
		ctx.SetValue(fileListFilterInPostKey, true)
		return nil
	}
	var cursor string
	hasCursor := false
	if req.After != nil && *req.After != "" {
		cursor, hasCursor = *req.After, true
	}
	built, berr := p.listOwnedFilesForCaller(ctx, provider, callerBatchOwner(vk), cursor, hasCursor, req.Purpose, fileListOrderAsc(req.Order))
	if berr != nil {
		return &schemas.LLMPluginShortCircuit{Error: berr}
	}
	return &schemas.LLMPluginShortCircuit{Response: built}
}

// fileListOrderAsc reports whether the request asked for oldest-first order via the
// OpenAI Files `order=asc` param. Absent/unset/"desc" means the default
// newest-first. Only "asc" (case-insensitive) flips the direction.
func fileListOrderAsc(order *string) bool {
	return order != nil && strings.EqualFold(strings.TrimSpace(*order), "asc")
}

// fileListScanCapError fails a filtered file-list request loud when the owned-file
// scan would exceed maxFileListOwnedScan — a sparse purpose filter over a very
// large owned set. Preferred over silently truncating results or falsely
// advertising has_more. Mirrors batchListScanCapError.
func fileListScanCapError() *schemas.BifrostError {
	return &schemas.BifrostError{
		StatusCode: bifrost.Ptr(400),
		Error: &schemas.ErrorField{
			Type:    bifrost.Ptr("file_list_filter_too_sparse"),
			Message: fmt.Sprintf("the file list purpose filter matched too sparsely across more than %d of your files to paginate reliably; narrow the filter or list without it", maxFileListOwnedScan),
		},
	}
}

// listOwnedFilesForCaller builds one owner-scoped file list page entirely from the
// ownership ledger. It resolves the inbound `after` cursor (if any) to a
// caller-OWNED ledger row, then walks the caller's own files in keyset order —
// newest-first by default, or oldest-first when `asc` honors an `order=asc`
// request — retrieving a fresh FileRetrieve per owned id and applying the request's
// purpose filter, until it has collected `limit` MATCHES or exhausted the owned
// set.
//
// has_more reflects the FILTERED result, not raw keyset counts: after the page is
// full it PROBES further owned rows for a (limit+1)th purpose MATCH, and only then
// reports has_more with the last match on the page as the `after` continuation. The
// probe is hard-bounded by maxFileListOwnedScan (a sparse filter over a huge owned
// set fails loud rather than scanning unboundedly or lying about has_more).
//
// Leak-proof by construction: every id examined is caller-owned, so no Data row or
// continuation cursor can carry a foreign id. Continuation is emitted ONLY when a
// further page exists. A retrieve that errors for one id is skipped (the seek still
// advances past it). Returns a BifrostError only for the scan-cap case. Mirrors
// listOwnedBatchesForCaller.
func (p *GovernancePlugin) listOwnedFilesForCaller(
	ctx *schemas.BifrostContext,
	provider schemas.ModelProvider,
	owner configstoreTables.ManagedBatchOwner,
	cursorID string,
	hasCursor bool,
	purpose schemas.FilePurpose,
	asc bool,
) (*schemas.BifrostResponse, *schemas.BifrostError) {
	limit := resolveFileListTarget(bifrost.GetIntFromContext(ctx, fileListRequestedLimitKey))

	built := builtFileListResponse(provider)
	list := built.FileListResponse

	var (
		seekCreatedAt time.Time
		seekID        string
		seek          bool
	)
	if hasCursor {
		row, err := p.fileLedger.GetManagedFile(ctx, string(provider), cursorID)
		if err != nil {
			p.logger.Error("[Governance] file list cursor lookup failed for provider=%s: %v; returning empty page", provider, err)
			return built, nil
		}
		if row == nil || !owner.Matches(row.OwnerVirtualKeyID, row.OwnerTeamID, row.OwnerCustomerID) {
			// Unowned / unknown cursor: neither leak it nor confirm it exists.
			return built, nil
		}
		seekCreatedAt = row.CreatedAt
		seekID = row.FileID
		seek = true
	}

	filterActive := purpose != ""
	data := make([]schemas.FileObject, 0, limit)
	var lastMatchID string // id of the last MATCH on the page = the continuation cursor
	hasMore := false
	scanned := 0      // owned rows retrieved+examined (bounded by maxFileListOwnedScan)
	pageFull := false // filter path: once true we retrieve only to PROBE for one more match

fetch:
	for {
		rows, moreBeyond, err := p.filePager.ListOwnedFileIDsPage(ctx, string(provider), owner, seekCreatedAt, seekID, seek, asc, fileListFetchChunk)
		if err != nil {
			p.logger.Error("[Governance] file list keyset query failed for provider=%s: %v; returning empty page", provider, err)
			return builtFileListResponse(provider), nil
		}
		if len(rows) == 0 {
			hasMore = false // owned set exhausted
			break
		}
		for i := range rows {
			// Advance the seek cursor to this row BEFORE keeping/skipping it.
			seekCreatedAt, seekID, seek = rows[i].CreatedAt, rows[i].FileID, true

			if !pageFull {
				fo := p.retrieveOwnedFile(ctx, provider, rows[i].FileID)
				scanned++
				if fo == nil {
					continue // retrieve error: skip (already advanced past it)
				}
				if filterActive && fo.Purpose != purpose {
					if scanned >= maxFileListOwnedScan {
						return nil, fileListScanCapError()
					}
					continue
				}
				data = append(data, *fo)
				lastMatchID = rows[i].FileID
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
			fo := p.retrieveOwnedFile(ctx, provider, rows[i].FileID)
			scanned++
			if fo != nil && fo.Purpose == purpose {
				hasMore = true // a further match exists => a genuine next page
				break fetch
			}
			if scanned >= maxFileListOwnedScan {
				return nil, fileListScanCapError()
			}
		}
		if !moreBeyond {
			hasMore = false // exhausted: page not full, or probe found no further match
			break
		}
		if scanned >= maxFileListOwnedScan {
			return nil, fileListScanCapError()
		}
	}

	list.Data = data
	list.HasMore = hasMore
	// `after` is the continuation cursor for the OpenAI Files dialect, so emit it
	// ONLY when a further page exists — a terminal page must not hand back a token
	// that fetches an empty page.
	if hasMore && lastMatchID != "" {
		last := lastMatchID
		list.After = &last
	}
	return built, nil
}

// retrieveOwnedFile issues one in-process FileRetrieve for an owned file id while
// assembling the owner-scoped list page. It uses the same isolated sub-request
// context as the batch retrieve fan-out (skip the plugin pipeline so it does not
// re-enter the gate; tombstone the caller's transport state so it is not
// misrouted to the list path). Returns nil (skip this row) on a nil client, a
// retrieve error, or a panic — a single bad id must not tank the whole page.
func (p *GovernancePlugin) retrieveOwnedFile(ctx context.Context, provider schemas.ModelProvider, fileID string) (fo *schemas.FileObject) {
	if p.batchClient == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			p.logger.Error("[Governance] recovered from panic during owner-scoped file retrieve (provider=%s file=%s): %v", provider, fileID, r)
			fo = nil
		}
	}()
	sub := newBatchSubRequestContext(ctx)
	defer sub.Cancel()
	resp, retErr := p.batchClient.FileRetrieveRequest(sub, &schemas.BifrostFileRetrieveRequest{
		Provider: provider,
		FileID:   fileID,
	})
	if retErr != nil {
		p.logger.Warn("[Governance] owner-scoped file retrieve failed (provider=%s file=%s); skipping from list page", provider, fileID)
		return nil
	}
	return fileObjectFromRetrieve(resp)
}

// filterFileListForCaller scopes a single upstream file-list page to the caller's
// ledger-owned files — the fallback used when a keyset pager or the core client
// is unavailable, so the deep short-circuit could not run. It keeps only ids the
// caller owns (ledger-absent ids follow the shared unknown-id policy; deny drops
// them), and — CRITICALLY — resets the page's continuation state and suppresses the
// raw upstream passthrough, mirroring the batch fallback (filterBatchListForCaller):
//
//   - ExtraFields.RawResponse is cleared so the dialect converter rebuilds the page
//     from the FILTERED Data. The OpenAI file-list converter returns RawResponse
//     verbatim when present, BEFORE ever reading Data, so leaving it set would ship
//     the COMPLETE unfiltered upstream workspace list to a scoped tenant.
//   - After is dropped and HasMore is set false. After is the upstream continuation
//     token — a FOREIGN file id in the OpenAI dialect — so echoing it would both
//     leak a foreign id and hand back a cursor that resumes the unfiltered upstream
//     list. The fallback is a shallow single scoped page by construction.
//
// The upstream page ids are provider-native (the transport encodes them only after
// this hook), matching the ledger key.
func (p *GovernancePlugin) filterFileListForCaller(ctx *schemas.BifrostContext, result *schemas.BifrostResponse, provider schemas.ModelProvider, vkValue string) {
	list := result.FileListResponse
	if list == nil || vkValue == "" {
		return
	}
	if provider == "" {
		provider = list.ExtraFields.Provider
	}
	vk, ok := p.store.GetVirtualKey(ctx, vkValue)
	if !ok || vk == nil {
		emptyFileList(list) // unknown VK: fail closed, no leak, no cursor, no raw
		return
	}
	owner := callerBatchOwner(vk)
	allowUnknown := p.resolveUnknownBatchIDPolicy() == UnknownBatchIDPolicyAllow
	kept := make([]schemas.FileObject, 0, len(list.Data))
	for i := range list.Data {
		fo := list.Data[i]
		row, err := p.fileLedger.GetManagedFile(ctx, string(provider), fo.ID)
		if err != nil {
			// Fail closed on a lookup error: drop the row rather than leak it.
			p.logger.Error("[Governance] file list filter lookup failed for provider=%s: %v; dropping row", provider, err)
			continue
		}
		if row == nil {
			if allowUnknown {
				kept = append(kept, fo)
			}
			continue
		}
		if owner.Matches(row.OwnerVirtualKeyID, row.OwnerTeamID, row.OwnerCustomerID) {
			kept = append(kept, fo)
		}
	}
	list.Data = kept
	// Reset continuation + suppress raw so a filtered page can neither leak the
	// foreign `after` id nor bypass Data via the raw passthrough.
	list.HasMore = false
	list.After = nil
	list.ExtraFields.RawResponse = nil
}

// emptyFileList fail-closes a file-list response to zero rows with no continuation
// cursor and no raw passthrough — the leak-proof empty page.
func emptyFileList(list *schemas.BifrostFileListResponse) {
	list.Data = []schemas.FileObject{}
	list.HasMore = false
	list.After = nil
	list.ExtraFields.RawResponse = nil
}

// fileObjectFromRetrieve maps a FileRetrieve response onto the FileObject shape
// the list response carries.
func fileObjectFromRetrieve(r *schemas.BifrostFileRetrieveResponse) *schemas.FileObject {
	if r == nil {
		return nil
	}
	return &schemas.FileObject{
		ID:            r.ID,
		Object:        r.Object,
		Bytes:         r.Bytes,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
		Filename:      r.Filename,
		Purpose:       r.Purpose,
		Status:        r.Status,
		StatusDetails: r.StatusDetails,
		ExpiresAt:     r.ExpiresAt,
	}
}

// resolveFileListTarget derives the file-list allocation-capacity hint from the
// caller's requested limit, falling back to the default and clamping to
// maxFileListPageSize so a caller-controlled value can never drive an unbounded
// allocation.
func resolveFileListTarget(limit int) int {
	target := limit
	if target <= 0 {
		target = defaultFileListLimit
	}
	if target > maxFileListPageSize {
		target = maxFileListPageSize
	}
	return target
}

// builtFileListResponse returns a fresh, empty owner-scoped file list response
// for a provider: object "list", a non-nil empty Data slice, no cursor. It is the
// base for both the assembled page and the fail-closed empty page.
func builtFileListResponse(provider schemas.ModelProvider) *schemas.BifrostResponse {
	return &schemas.BifrostResponse{
		FileListResponse: &schemas.BifrostFileListResponse{
			Object:      "list",
			Data:        []schemas.FileObject{},
			ExtraFields: schemas.BifrostResponseExtraFields{Provider: provider},
		},
	}
}
