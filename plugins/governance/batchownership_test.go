package governance

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/providers/bedrock"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// fakeBatchLedger is an in-memory batchLedger that mirrors the real store's
// semantics (idempotent register keyed by provider|batch_id; OR-ownership list)
// so the gate/capture/filter logic can be exercised without a database.
type fakeBatchLedger struct {
	rows        map[string]*configstoreTables.TableManagedBatch // key: provider + "|" + batchID
	registerErr error
	getErr      error
	listErr     error
	registered  int
}

func newFakeBatchLedger() *fakeBatchLedger {
	return &fakeBatchLedger{rows: map[string]*configstoreTables.TableManagedBatch{}}
}

func ledgerKey(provider, batchID string) string { return provider + "|" + batchID }

func (f *fakeBatchLedger) RegisterManagedBatch(_ context.Context, batch *configstoreTables.TableManagedBatch, _ ...*gorm.DB) error {
	if f.registerErr != nil {
		return f.registerErr
	}
	key := ledgerKey(batch.Provider, batch.BatchID)
	if _, exists := f.rows[key]; !exists { // DoNothing on conflict: first writer wins
		cp := *batch
		f.rows[key] = &cp
	}
	f.registered++
	return nil
}

func (f *fakeBatchLedger) GetManagedBatch(_ context.Context, provider, batchID string, _ ...*gorm.DB) (*configstoreTables.TableManagedBatch, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.rows[ledgerKey(provider, batchID)], nil
}

func (f *fakeBatchLedger) ListOwnedBatchIDs(_ context.Context, provider string, owner configstoreTables.ManagedBatchOwner, _ ...*gorm.DB) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	ids := []string{}
	for _, row := range f.rows {
		if row.Provider == provider && owner.Matches(row.OwnerVirtualKeyID, row.OwnerTeamID, row.OwnerCustomerID) {
			ids = append(ids, row.BatchID)
		}
	}
	return ids, nil
}

// ListOwnedBatchIDsPage mirrors the RDB keyset in REVERSE-CHRONOLOGICAL order:
// owner-matched rows sorted newest-first by (created_at, batch_id), seeked past
// the cursor (forward = older, before = the closest newer rows), with a limit+1
// hasMore probe. Every page is returned newest-first.
func (f *fakeBatchLedger) ListOwnedBatchIDsPage(_ context.Context, provider string, owner configstoreTables.ManagedBatchOwner, cursorCreatedAt time.Time, cursorID string, hasCursor, before bool, limit int, _ ...*gorm.DB) ([]configstoreTables.TableManagedBatch, bool, error) {
	if f.listErr != nil {
		return nil, false, f.listErr
	}
	matched := []configstoreTables.TableManagedBatch{}
	for _, row := range f.rows {
		if row.Provider == provider && owner.Matches(row.OwnerVirtualKeyID, row.OwnerTeamID, row.OwnerCustomerID) {
			matched = append(matched, *row)
		}
	}
	// Newest-first (DESC by created_at, then batch_id).
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}
		return matched[i].BatchID > matched[j].BatchID
	})

	filtered := []configstoreTables.TableManagedBatch{}
	for _, row := range matched { // iterating newest-first
		if hasCursor {
			newer := row.CreatedAt.After(cursorCreatedAt) || (row.CreatedAt.Equal(cursorCreatedAt) && row.BatchID > cursorID)
			older := row.CreatedAt.Before(cursorCreatedAt) || (row.CreatedAt.Equal(cursorCreatedAt) && row.BatchID < cursorID)
			if before && !newer { // "before" wants rows NEWER than the cursor
				continue
			}
			if !before && !older { // forward wants rows OLDER than the cursor
				continue
			}
		}
		filtered = append(filtered, row)
	}

	if limit <= 0 {
		limit = 1
	}
	hasMore := len(filtered) > limit
	var page []configstoreTables.TableManagedBatch
	if before {
		// The `limit` rows closest to the cursor = the tail of the newest-first
		// "newer than cursor" list (still newest-first among themselves).
		start := 0
		if len(filtered) > limit {
			start = len(filtered) - limit
		}
		page = filtered[start:]
	} else {
		end := limit
		if end > len(filtered) {
			end = len(filtered)
		}
		page = filtered[:end]
	}
	return append([]configstoreTables.TableManagedBatch{}, page...), hasMore, nil
}

// fakeBatchClient is an in-process BatchLifecycleClient stand-in. It records the
// compensating-cancel calls and the sub-request context state, to assert the
// cancel targets the right id and does NOT inherit the caller's transport state.
type fakeBatchClient struct {
	cancelCalls        []string
	cancelErr          *schemas.BifrostError
	lastCancelURLPath  any
	lastCancelRawBody  any
	lastCancelSkipPipe any

	// Retrieve fan-out (owner-scoped list assembly).
	retrieveCalls        []string
	retrieveErrIDs       map[string]bool // ids whose retrieve returns an error (skipped from the page)
	lastRetrieveURLPath  any
	lastRetrieveRawBody  any
	lastRetrieveSkipPipe any
}

func (f *fakeBatchClient) BatchCancelRequest(ctx *schemas.BifrostContext, req *schemas.BifrostBatchCancelRequest) (*schemas.BifrostBatchCancelResponse, *schemas.BifrostError) {
	f.cancelCalls = append(f.cancelCalls, req.BatchID)
	f.lastCancelURLPath = ctx.Value(schemas.BifrostContextKeyURLPath)
	f.lastCancelRawBody = ctx.Value(schemas.BifrostContextKeyUseRawRequestBody)
	f.lastCancelSkipPipe = ctx.Value(schemas.BifrostContextKeySkipPluginPipeline)
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	return &schemas.BifrostBatchCancelResponse{ID: req.BatchID, Status: schemas.BatchStatusCancelling}, nil
}

func (f *fakeBatchClient) BatchRetrieveRequest(ctx *schemas.BifrostContext, req *schemas.BifrostBatchRetrieveRequest) (*schemas.BifrostBatchRetrieveResponse, *schemas.BifrostError) {
	f.retrieveCalls = append(f.retrieveCalls, req.BatchID)
	f.lastRetrieveURLPath = ctx.Value(schemas.BifrostContextKeyURLPath)
	f.lastRetrieveRawBody = ctx.Value(schemas.BifrostContextKeyUseRawRequestBody)
	f.lastRetrieveSkipPipe = ctx.Value(schemas.BifrostContextKeySkipPluginPipeline)
	if f.retrieveErrIDs[req.BatchID] {
		return nil, &schemas.BifrostError{Error: &schemas.ErrorField{Message: "retrieve failed"}}
	}
	// Fresh live state assembled per row (id echoed; the ledger stores no status).
	return &schemas.BifrostBatchRetrieveResponse{ID: req.BatchID, Status: schemas.BatchStatusCompleted}, nil
}

// FileRetrieveRequest / FileDeleteRequest satisfy the shared BatchLifecycleClient
// interface (extended in Part B for the file-ownership gate). The batch tests do
// not exercise the file paths, so these are inert stubs; the file tests use their
// own client fake (see fileownership_test.go).
func (f *fakeBatchClient) FileRetrieveRequest(_ *schemas.BifrostContext, req *schemas.BifrostFileRetrieveRequest) (*schemas.BifrostFileRetrieveResponse, *schemas.BifrostError) {
	return &schemas.BifrostFileRetrieveResponse{ID: req.FileID}, nil
}

func (f *fakeBatchClient) FileDeleteRequest(_ *schemas.BifrostContext, req *schemas.BifrostFileDeleteRequest) (*schemas.BifrostFileDeleteResponse, *schemas.BifrostError) {
	return &schemas.BifrostFileDeleteResponse{ID: req.FileID, Deleted: true}, nil
}

func ptr(s string) *string { return &s }

// newBatchPluginWithConfig builds a governance plugin backed by an in-memory VK
// store and the given (fake) ledger, configured from the given plugin Config —
// the same path plugins.go uses (which passes live ClientConfig-backed pointers).
func newBatchPluginWithConfig(t *testing.T, vks []*configstoreTables.TableVirtualKey, ledger batchLedger, config *Config) *GovernancePlugin {
	t.Helper()
	logger := NewMockLogger()
	vkVals := make([]configstoreTables.TableVirtualKey, 0, len(vks))
	for _, vk := range vks {
		vkVals = append(vkVals, *vk)
	}
	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: vkVals,
	}, nil)
	require.NoError(t, err)
	p := &GovernancePlugin{
		logger:   logger,
		store:    store,
		resolver: NewBudgetResolver(store, nil, logger, nil),
	}
	p.configureBatchOwnership(config, nil)
	p.batchLedger = ledger
	// Wire the optional keyset pager when the injected ledger provides it (the
	// real path type-asserts ManagedBatchPager). A ledger that implements only the
	// 3-method batchLedger leaves p.batchPager nil, exercising the owned-id
	// fallback — enforcement stays on either way.
	if pager, ok := ledger.(batchPager); ok {
		p.batchPager = pager
	}
	return p
}

// newBatchPlugin is the common case: admin IDs + policy value, wrapped in live
// pointers exactly as plugins.go wires them from ClientConfig.
func newBatchPlugin(t *testing.T, vks []*configstoreTables.TableVirtualKey, ledger batchLedger, adminIDs []string, policy string) *GovernancePlugin {
	t.Helper()
	return newBatchPluginWithConfig(t, vks, ledger, &Config{
		BatchAdminVirtualKeyIDs: &adminIDs,
		UnknownBatchIDPolicy:    &policy,
	})
}

func batchCtx(vkValue string) *schemas.BifrostContext {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if vkValue != "" {
		ctx.SetValue(schemas.BifrostContextKeyVirtualKey, vkValue)
	}
	return ctx
}

func retrieveReq(provider schemas.ModelProvider, batchID string) *schemas.BifrostRequest {
	return &schemas.BifrostRequest{
		RequestType:          schemas.BatchRetrieveRequest,
		BatchRetrieveRequest: &schemas.BifrostBatchRetrieveRequest{Provider: provider, BatchID: batchID},
	}
}

func TestCallerBatchOwner_ResolvesHierarchy(t *testing.T) {
	teamID, custID, userID := "team-1", "cust-1", "user-1"

	t.Run("direct team and customer FKs", func(t *testing.T) {
		vk := buildVirtualKey("vk-1", "sk-bf-1", "vk", true)
		vk.TeamID = &teamID
		vk.CustomerID = &custID
		vk.CreatedByUserID = &userID
		owner := callerBatchOwner(vk)
		assert.Equal(t, configstoreTables.ManagedBatchOwner{VirtualKeyID: "vk-1", TeamID: teamID, CustomerID: custID, UserID: userID}, owner)
	})

	t.Run("customer resolved via team relation", func(t *testing.T) {
		vk := buildVirtualKey("vk-2", "sk-bf-2", "vk", true)
		team := buildTeam(teamID, "t", nil)
		team.CustomerID = &custID
		vk.Team = team
		owner := callerBatchOwner(vk)
		assert.Equal(t, teamID, owner.TeamID)
		assert.Equal(t, custID, owner.CustomerID)
	})
}

func TestBatchTargetFromRequest(t *testing.T) {
	cases := []struct {
		name string
		req  *schemas.BifrostRequest
		want string
	}{
		{"retrieve", retrieveReq(schemas.Anthropic, "b-r"), "b-r"},
		{"cancel", &schemas.BifrostRequest{RequestType: schemas.BatchCancelRequest, BatchCancelRequest: &schemas.BifrostBatchCancelRequest{Provider: schemas.Anthropic, BatchID: "b-c"}}, "b-c"},
		{"results", &schemas.BifrostRequest{RequestType: schemas.BatchResultsRequest, BatchResultsRequest: &schemas.BifrostBatchResultsRequest{Provider: schemas.Anthropic, BatchID: "b-res"}}, "b-res"},
		{"delete", &schemas.BifrostRequest{RequestType: schemas.BatchDeleteRequest, BatchDeleteRequest: &schemas.BifrostBatchDeleteRequest{Provider: schemas.Anthropic, BatchID: "b-d"}}, "b-d"},
		{"non-per-id (create)", &schemas.BifrostRequest{RequestType: schemas.BatchCreateRequest}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, id := batchTargetFromRequest(c.req)
			assert.Equal(t, c.want, id)
		})
	}
}

func TestEnforceBatchOwnershipPreHook(t *testing.T) {
	ownerVKID, foreignVKID := "vk-owner", "vk-foreign"
	teamID, custID := "team-shared", "cust-shared"

	buildVK := func(id, value string, team, cust *string) *configstoreTables.TableVirtualKey {
		vk := buildVirtualKey(id, value, id, true)
		vk.TeamID = team
		vk.CustomerID = cust
		return vk
	}

	ownerVK := buildVK(ownerVKID, "sk-bf-owner", &teamID, nil)
	teammateVK := buildVK("vk-teammate", "sk-bf-teammate", &teamID, nil) // same team
	custMateVK := buildVK("vk-custmate", "sk-bf-custmate", nil, &custID)
	foreignVK := buildVK(foreignVKID, "sk-bf-foreign", nil, nil)
	adminVK := buildVK("vk-admin", "sk-bf-admin", nil, nil)

	newLedgerWithBatch := func() *fakeBatchLedger {
		l := newFakeBatchLedger()
		l.rows[ledgerKey("anthropic", "msgbatch_owned")] = &configstoreTables.TableManagedBatch{
			Provider: "anthropic", BatchID: "msgbatch_owned",
			OwnerVirtualKeyID: ownerVKID, OwnerTeamID: teamID, OwnerCustomerID: custID,
		}
		return l
	}

	allVKs := []*configstoreTables.TableVirtualKey{ownerVK, teammateVK, custMateVK, foreignVK, adminVK}

	t.Run("owner is allowed", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, newLedgerWithBatch(), nil, "")
		err := p.enforceBatchOwnershipPreHook(batchCtx("sk-bf-owner"), retrieveReq(schemas.Anthropic, "msgbatch_owned"))
		assert.Nil(t, err)
	})

	t.Run("same-team caller is allowed", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, newLedgerWithBatch(), nil, "")
		err := p.enforceBatchOwnershipPreHook(batchCtx("sk-bf-teammate"), retrieveReq(schemas.Anthropic, "msgbatch_owned"))
		assert.Nil(t, err)
	})

	t.Run("same-customer caller is allowed", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, newLedgerWithBatch(), nil, "")
		err := p.enforceBatchOwnershipPreHook(batchCtx("sk-bf-custmate"), retrieveReq(schemas.Anthropic, "msgbatch_owned"))
		assert.Nil(t, err)
	})

	t.Run("foreign caller gets existence-hiding 404", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, newLedgerWithBatch(), nil, "")
		err := p.enforceBatchOwnershipPreHook(batchCtx("sk-bf-foreign"), retrieveReq(schemas.Anthropic, "msgbatch_owned"))
		require.NotNil(t, err)
		assert.Equal(t, 404, *err.StatusCode)
		require.NotNil(t, err.Error.Type)
		assert.Equal(t, "not_found_error", *err.Error.Type)
		assert.Equal(t, "batch not found", err.Error.Message)
	})

	t.Run("unknown id DENIED by default (secure) policy", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, newLedgerWithBatch(), nil, "")
		err := p.enforceBatchOwnershipPreHook(batchCtx("sk-bf-owner"), retrieveReq(schemas.Anthropic, "msgbatch_ghost"))
		require.NotNil(t, err, "default is deny: a ledger-absent id is refused, not relayed upstream")
		assert.Equal(t, 404, *err.StatusCode)
	})

	t.Run("unknown id denied under explicit deny policy", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, newLedgerWithBatch(), nil, UnknownBatchIDPolicyDeny)
		err := p.enforceBatchOwnershipPreHook(batchCtx("sk-bf-owner"), retrieveReq(schemas.Anthropic, "msgbatch_ghost"))
		require.NotNil(t, err)
		assert.Equal(t, 404, *err.StatusCode)
	})

	t.Run("unknown id allowed under explicit allow (migration) policy", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, newLedgerWithBatch(), nil, UnknownBatchIDPolicyAllow)
		err := p.enforceBatchOwnershipPreHook(batchCtx("sk-bf-owner"), retrieveReq(schemas.Anthropic, "msgbatch_ghost"))
		assert.Nil(t, err)
	})

	t.Run("admin bypasses the gate for a foreign batch", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, newLedgerWithBatch(), []string{"vk-admin"}, "")
		err := p.enforceBatchOwnershipPreHook(batchCtx("sk-bf-admin"), retrieveReq(schemas.Anthropic, "msgbatch_owned"))
		assert.Nil(t, err)
	})

	t.Run("nil ledger disables enforcement", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, nil, nil, "")
		err := p.enforceBatchOwnershipPreHook(batchCtx("sk-bf-foreign"), retrieveReq(schemas.Anthropic, "msgbatch_owned"))
		assert.Nil(t, err)
	})

	t.Run("ledger read error fails closed with 404", func(t *testing.T) {
		l := newLedgerWithBatch()
		l.getErr = errors.New("db down")
		p := newBatchPlugin(t, allVKs, l, nil, "")
		err := p.enforceBatchOwnershipPreHook(batchCtx("sk-bf-owner"), retrieveReq(schemas.Anthropic, "msgbatch_owned"))
		require.NotNil(t, err)
		assert.Equal(t, 404, *err.StatusCode)
	})

	t.Run("non-batch request is ignored", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, newLedgerWithBatch(), nil, "")
		chat := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest}
		assert.Nil(t, p.enforceBatchOwnershipPreHook(batchCtx("sk-bf-owner"), chat))
	})
}

func TestCaptureBatchOwnership(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "owner", true)
	teamID := "team-1"
	ownerVK.TeamID = &teamID

	t.Run("records ownership on create success", func(t *testing.T) {
		ledger := newFakeBatchLedger()
		p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
		result := &schemas.BifrostResponse{
			BatchCreateResponse: &schemas.BifrostBatchCreateResponse{
				ID:          "msgbatch_new",
				ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.Anthropic},
			},
		}
		out, berr := p.applyBatchOwnershipPostHook(batchCtx("sk-bf-owner"), result, schemas.BatchCreateRequest, schemas.Anthropic, "sk-bf-owner")
		assert.Nil(t, berr)
		assert.Same(t, result, out)
		row := ledger.rows[ledgerKey("anthropic", "msgbatch_new")]
		require.NotNil(t, row)
		assert.Equal(t, "vk-owner", row.OwnerVirtualKeyID)
		assert.Equal(t, teamID, row.OwnerTeamID)
	})

	t.Run("ledger fail + compensating cancel ok => retryable 503, batch cancelled", func(t *testing.T) {
		ledger := newFakeBatchLedger()
		ledger.registerErr = errors.New("db down")
		client := &fakeBatchClient{} // cancel succeeds
		p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
		p.batchClient = client
		// Simulate caller transport state that must NOT leak into the internal
		// cancel sub-request: the create path + raw-body passthrough.
		parent := batchCtx("sk-bf-owner")
		parent.SetValue(schemas.BifrostContextKeyURLPath, "/v1/messages/batches")
		parent.SetValue(schemas.BifrostContextKeyUseRawRequestBody, true)
		result := &schemas.BifrostResponse{
			BatchCreateResponse: &schemas.BifrostBatchCreateResponse{ID: "msgbatch_x", ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.Anthropic}},
		}
		out, berr := p.applyBatchOwnershipPostHook(parent, result, schemas.BatchCreateRequest, schemas.Anthropic, "sk-bf-owner")
		require.NotNil(t, berr)
		assert.Nil(t, out)
		assert.Equal(t, 503, *berr.StatusCode, "retryable when nothing is orphaned")
		assert.Equal(t, []string{"msgbatch_x"}, client.cancelCalls, "the just-created batch must be cancelled by id")
		// The sub-request must be isolated: no inherited URL path / raw-body, but
		// the plugin pipeline must be skipped so it does not re-enter the gate.
		assert.Nil(t, client.lastCancelURLPath, "cancel must not inherit the caller's URL path")
		assert.Nil(t, client.lastCancelRawBody, "cancel must not inherit the caller's raw-body passthrough")
		assert.Equal(t, true, client.lastCancelSkipPipe, "cancel sub-request must skip the plugin pipeline")
	})

	t.Run("ledger fail + compensating cancel fail => 500 orphan warning", func(t *testing.T) {
		ledger := newFakeBatchLedger()
		ledger.registerErr = errors.New("db down")
		client := &fakeBatchClient{cancelErr: &schemas.BifrostError{Error: &schemas.ErrorField{Message: "cancel failed"}}}
		p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
		p.batchClient = client
		result := &schemas.BifrostResponse{
			BatchCreateResponse: &schemas.BifrostBatchCreateResponse{ID: "msgbatch_orphan", ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.Anthropic}},
		}
		out, berr := p.applyBatchOwnershipPostHook(batchCtx("sk-bf-owner"), result, schemas.BatchCreateRequest, schemas.Anthropic, "sk-bf-owner")
		require.NotNil(t, berr)
		assert.Nil(t, out)
		assert.Equal(t, 500, *berr.StatusCode)
		assert.Equal(t, []string{"msgbatch_orphan"}, client.cancelCalls)
		assert.Contains(t, berr.Error.Message, "orphaned")
	})

	t.Run("ledger fail + no client => 500 orphan (cannot cancel)", func(t *testing.T) {
		ledger := newFakeBatchLedger()
		ledger.registerErr = errors.New("db down")
		p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
		// p.batchClient stays nil
		result := &schemas.BifrostResponse{
			BatchCreateResponse: &schemas.BifrostBatchCreateResponse{ID: "msgbatch_y", ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.Anthropic}},
		}
		out, berr := p.applyBatchOwnershipPostHook(batchCtx("sk-bf-owner"), result, schemas.BatchCreateRequest, schemas.Anthropic, "sk-bf-owner")
		require.NotNil(t, berr)
		assert.Nil(t, out)
		assert.Equal(t, 500, *berr.StatusCode)
	})

	t.Run("no VK is a no-op (single-tenant path)", func(t *testing.T) {
		ledger := newFakeBatchLedger()
		p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
		result := &schemas.BifrostResponse{
			BatchCreateResponse: &schemas.BifrostBatchCreateResponse{ID: "msgbatch_y", ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.Anthropic}},
		}
		_, berr := p.applyBatchOwnershipPostHook(batchCtx(""), result, schemas.BatchCreateRequest, schemas.Anthropic, "")
		assert.Nil(t, berr)
		assert.Equal(t, 0, ledger.registered)
	})
}

// listBatchCtx builds a caller context carrying the VK and the (already clamped)
// requested page size, exactly as PreLLMHook records it before the short-circuit.
func listBatchCtx(vkValue string, limit int) *schemas.BifrostContext {
	ctx := batchCtx(vkValue)
	ctx.SetValue(batchListRequestedLimitKey, resolveBatchListTarget(limit, 0))
	return ctx
}

func listReq(provider schemas.ModelProvider, limit int) *schemas.BifrostBatchListRequest {
	return &schemas.BifrostBatchListRequest{Provider: provider, Limit: limit}
}

// ownedOnly asserts every id-bearing field of a built list page references only
// owned batch ids — the leak-proof invariant. No Data row, first_id, last_id, or
// next_cursor may ever carry a foreign id.
func ownedOnly(t *testing.T, list *schemas.BifrostBatchListResponse, owned map[string]bool) {
	t.Helper()
	for _, b := range list.Data {
		assert.True(t, owned[b.ID], "data must contain only owned ids, got %q", b.ID)
	}
	if list.FirstID != nil {
		assert.True(t, owned[*list.FirstID], "first_id must be owned, got %q", *list.FirstID)
	}
	if list.LastID != nil {
		assert.True(t, owned[*list.LastID], "last_id must be owned, got %q", *list.LastID)
	}
	if list.NextCursor != nil {
		assert.True(t, owned[*list.NextCursor], "next_cursor must be an owned id, got %q", *list.NextCursor)
	}
	assert.Nil(t, list.ExtraFields.RawResponse, "no upstream raw passthrough may be present (list is ledger-built)")
}

// TestListOwnedBatchesForCaller_DeepPagination proves LIST is now a deep keyset
// over the ledger: a caller's own batches that span multiple pages all appear,
// across pages, and the cursor/first_id/last_id only ever carry owned ids.
func TestListOwnedBatchesForCaller_DeepPagination(t *testing.T) {
	ownerVKID, teamID := "vk-owner", "team-1"
	ownerVK := buildVirtualKey(ownerVKID, "sk-bf-owner", "owner", true)
	ownerVK.TeamID = &teamID
	allVKs := []*configstoreTables.TableVirtualKey{ownerVK}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ledger := newFakeBatchLedger()
	ownedSet := map[string]bool{}
	// Five owned batches, oldest→newest, plus two foreign rows on the same provider.
	ownedIDs := []string{"b1", "b2", "b3", "b4", "b5"}
	for i, id := range ownedIDs {
		ledger.rows[ledgerKey("anthropic", id)] = &configstoreTables.TableManagedBatch{
			Provider: "anthropic", BatchID: id, OwnerVirtualKeyID: ownerVKID, OwnerTeamID: teamID,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		ownedSet[id] = true
	}
	ledger.rows[ledgerKey("anthropic", "f1")] = &configstoreTables.TableManagedBatch{Provider: "anthropic", BatchID: "f1", OwnerVirtualKeyID: "vk-foreign", CreatedAt: base.Add(90 * time.Second)}
	ledger.rows[ledgerKey("anthropic", "f2")] = &configstoreTables.TableManagedBatch{Provider: "anthropic", BatchID: "f2", OwnerVirtualKeyID: "vk-foreign", CreatedAt: base.Add(200 * time.Second)}

	client := &fakeBatchClient{}
	p := newBatchPlugin(t, allVKs, ledger, nil, "")
	p.batchClient = client
	owner := callerBatchOwner(ownerVK)

	// Walk the whole owner set two-per-page via after-cursors; union every page.
	// Native batch lists are NEWEST-FIRST, so page 1 returns the two newest.
	seen := []string{}
	cursor := batchListCursor{}
	hasCursor := false
	for page := 0; page < 10; page++ {
		ctx := listBatchCtx("sk-bf-owner", 2)
		built, berr := p.listOwnedBatchesForCaller(ctx, schemas.Anthropic, owner, cursor, hasCursor, batchListFilters{})
		require.Nil(t, berr)
		list := built.BatchListResponse
		ownedOnly(t, list, ownedSet)
		for _, b := range list.Data {
			seen = append(seen, b.ID)
		}
		if !list.HasMore {
			assert.Nil(t, list.NextCursor, "no next_cursor on the final page")
			break
		}
		require.NotNil(t, list.LastID, "a page with has_more must expose last_id to continue")
		require.NotNil(t, list.NextCursor, "forward has_more must emit next_cursor")
		assert.Equal(t, *list.LastID, *list.NextCursor)
		cursor = batchListCursor{id: *list.LastID}
		hasCursor = true
	}
	assert.Equal(t, []string{"b5", "b4", "b3", "b2", "b1"}, seen, "every owned batch paginates through NEWEST-FIRST, none skipped or foreign")

	// has_more is exact at a page boundary: after b3 exactly b2,b1 remain (older)
	// => has_more=false on that page.
	ctx := listBatchCtx("sk-bf-owner", 2)
	built, _ := p.listOwnedBatchesForCaller(ctx, schemas.Anthropic, owner, batchListCursor{id: "b3"}, true, batchListFilters{})
	assert.Equal(t, []string{"b2", "b1"}, dataIDs(built.BatchListResponse))
	assert.False(t, built.BatchListResponse.HasMore, "exactly the last two (older) remain: has_more must be false")
}

// TestListOwnedBatchesForCaller_ForeignCursorEmptyPage proves a cursor id the
// caller does not own (or that does not exist) yields an EMPTY page — never a
// leak, never a confirmation the id exists.
func TestListOwnedBatchesForCaller_ForeignCursorEmptyPage(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "owner", true)
	ledger := newFakeBatchLedger()
	ledger.rows[ledgerKey("anthropic", "b1")] = &configstoreTables.TableManagedBatch{Provider: "anthropic", BatchID: "b1", OwnerVirtualKeyID: "vk-owner"}
	ledger.rows[ledgerKey("anthropic", "foreign")] = &configstoreTables.TableManagedBatch{Provider: "anthropic", BatchID: "foreign", OwnerVirtualKeyID: "vk-other"}
	client := &fakeBatchClient{}
	p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
	p.batchClient = client
	owner := callerBatchOwner(ownerVK)

	t.Run("cursor id owned by someone else", func(t *testing.T) {
		built, _ := p.listOwnedBatchesForCaller(listBatchCtx("sk-bf-owner", 20), schemas.Anthropic, owner, batchListCursor{id: "foreign"}, true, batchListFilters{})
		assert.Empty(t, built.BatchListResponse.Data)
		assert.Nil(t, built.BatchListResponse.FirstID)
		assert.Nil(t, built.BatchListResponse.LastID)
		assert.Nil(t, built.BatchListResponse.NextCursor)
		assert.Empty(t, client.retrieveCalls, "no retrieve may be issued for a page short-circuited to empty")
	})

	t.Run("cursor id that does not exist", func(t *testing.T) {
		built, _ := p.listOwnedBatchesForCaller(listBatchCtx("sk-bf-owner", 20), schemas.Anthropic, owner, batchListCursor{id: "ghost"}, true, batchListFilters{})
		assert.Empty(t, built.BatchListResponse.Data)
	})
}

// TestListOwnedBatchesForCaller_RetrieveErrorSkipsRow proves one bad upstream
// retrieve is skipped (not fatal): every owned row is still attempted newest-first,
// the survivors are returned, and pagination stays coherent.
func TestListOwnedBatchesForCaller_RetrieveErrorSkipsRow(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "owner", true)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ledger := newFakeBatchLedger()
	for i, id := range []string{"b1", "b2", "b3"} {
		ledger.rows[ledgerKey("anthropic", id)] = &configstoreTables.TableManagedBatch{Provider: "anthropic", BatchID: id, OwnerVirtualKeyID: "vk-owner", CreatedAt: base.Add(time.Duration(i) * time.Minute)}
	}
	client := &fakeBatchClient{retrieveErrIDs: map[string]bool{"b2": true}} // middle row fails
	p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
	p.batchClient = client

	built, _ := p.listOwnedBatchesForCaller(listBatchCtx("sk-bf-owner", 10), schemas.Anthropic, callerBatchOwner(ownerVK), batchListCursor{}, false, batchListFilters{})
	list := built.BatchListResponse
	assert.Equal(t, []string{"b3", "b1"}, dataIDs(list), "the failed retrieve is skipped, the rest survive (newest-first)")
	assert.Equal(t, []string{"b3", "b2", "b1"}, client.retrieveCalls, "every owned row is attempted newest-first (b2 advanced past on error)")
	// This is the terminal page (all owned rows examined), so no continuation.
	assert.False(t, list.HasMore)
	require.NotNil(t, list.FirstID)
	assert.Equal(t, "b3", *list.FirstID, "first_id is the first returned item (display)")
	assert.Nil(t, list.LastID, "no continuation token on the final page")
	assert.Nil(t, list.NextCursor, "no next cursor on the final page")
}

// TestListOwnedBatchesForCaller_BackwardPaging proves Anthropic before_id paging
// returns the owner's rows just NEWER than the cursor (closest first), newest-first
// display, still owned-only.
func TestListOwnedBatchesForCaller_BackwardPaging(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "owner", true)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ledger := newFakeBatchLedger()
	ownedSet := map[string]bool{}
	for i, id := range []string{"b1", "b2", "b3", "b4", "b5"} {
		ledger.rows[ledgerKey("anthropic", id)] = &configstoreTables.TableManagedBatch{Provider: "anthropic", BatchID: id, OwnerVirtualKeyID: "vk-owner", CreatedAt: base.Add(time.Duration(i) * time.Minute)}
		ownedSet[id] = true
	}
	p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
	p.batchClient = &fakeBatchClient{}

	// before b2, page size 2 => the two rows just NEWER than b2 (closest first) are
	// b3,b4, shown newest-first => [b4,b3]; b5 (even newer) still remains.
	built, _ := p.listOwnedBatchesForCaller(listBatchCtx("sk-bf-owner", 2), schemas.Anthropic, callerBatchOwner(ownerVK), batchListCursor{id: "b2", before: true}, true, batchListFilters{})
	list := built.BatchListResponse
	assert.Equal(t, []string{"b4", "b3"}, dataIDs(list))
	assert.True(t, list.HasMore, "b5 (newer) still remains beyond the page")
	assert.Nil(t, list.NextCursor, "backward paging continues via first_id, not next_cursor")
	ownedOnly(t, list, ownedSet)
}

// TestBuildBatchListShortCircuit_Routing covers which callers are served from the
// ledger (short-circuit) vs. relayed upstream (nil): admin and single-tenant
// relay upstream; an unknown VK fails closed to an empty page.
func TestBuildBatchListShortCircuit_Routing(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "owner", true)
	adminVK := buildVirtualKey("vk-admin", "sk-bf-admin", "admin", true)
	allVKs := []*configstoreTables.TableVirtualKey{ownerVK, adminVK}
	ledger := newFakeBatchLedger()
	ledger.rows[ledgerKey("anthropic", "b1")] = &configstoreTables.TableManagedBatch{Provider: "anthropic", BatchID: "b1", OwnerVirtualKeyID: "vk-owner"}

	t.Run("admin relays upstream (nil short-circuit)", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, ledger, []string{"vk-admin"}, "")
		p.batchClient = &fakeBatchClient{}
		sc := p.buildBatchListShortCircuit(listBatchCtx("sk-bf-admin", 20), listReq(schemas.Anthropic, 20))
		assert.Nil(t, sc, "admin is entitled to the full upstream list")
	})

	t.Run("no VK (single-tenant) relays upstream", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, ledger, nil, "")
		p.batchClient = &fakeBatchClient{}
		sc := p.buildBatchListShortCircuit(listBatchCtx("", 20), listReq(schemas.Anthropic, 20))
		assert.Nil(t, sc, "no identity to scope by => relay upstream unchanged")
	})

	t.Run("no ledger relays upstream", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, nil, nil, "")
		sc := p.buildBatchListShortCircuit(listBatchCtx("sk-bf-owner", 20), listReq(schemas.Anthropic, 20))
		assert.Nil(t, sc)
	})

	t.Run("no lifecycle client relays upstream (deep pagination unavailable)", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, ledger, nil, "")
		// p.batchClient stays nil (Go-SDK deployment).
		sc := p.buildBatchListShortCircuit(listBatchCtx("sk-bf-owner", 20), listReq(schemas.Anthropic, 20))
		assert.Nil(t, sc, "without the client we must relay upstream (post-hook filters), not short-circuit to empty")
	})

	t.Run("unknown VK fails closed to an empty short-circuit page", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, ledger, nil, "")
		p.batchClient = &fakeBatchClient{}
		sc := p.buildBatchListShortCircuit(listBatchCtx("sk-bf-nobody", 20), listReq(schemas.Anthropic, 20))
		require.NotNil(t, sc)
		require.NotNil(t, sc.Response)
		require.NotNil(t, sc.Response.BatchListResponse)
		assert.Empty(t, sc.Response.BatchListResponse.Data, "unknown VK must never relay the full list")
	})

	t.Run("scoped caller is served a built page (short-circuit, upstream never called)", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, ledger, nil, "")
		p.batchClient = &fakeBatchClient{}
		sc := p.buildBatchListShortCircuit(listBatchCtx("sk-bf-owner", 20), listReq(schemas.Anthropic, 20))
		require.NotNil(t, sc)
		require.NotNil(t, sc.Response)
		require.NotNil(t, sc.Response.BatchListResponse)
		assert.Equal(t, []string{"b1"}, dataIDs(sc.Response.BatchListResponse))
	})
}

// TestListOwnedBatchesForCaller_FanOutIsolatedSubRequest proves each retrieve in
// the fan-out uses an isolated sub-request context: the plugin pipeline is
// skipped (so it does not re-enter the gate) and the caller's transport state
// (URL path / raw-body passthrough) is not inherited.
func TestListOwnedBatchesForCaller_FanOutIsolatedSubRequest(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "owner", true)
	ledger := newFakeBatchLedger()
	ledger.rows[ledgerKey("anthropic", "b1")] = &configstoreTables.TableManagedBatch{Provider: "anthropic", BatchID: "b1", OwnerVirtualKeyID: "vk-owner"}
	client := &fakeBatchClient{}
	p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
	p.batchClient = client

	ctx := listBatchCtx("sk-bf-owner", 20)
	ctx.SetValue(schemas.BifrostContextKeyURLPath, "/v1/messages/batches")
	ctx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, true)
	built, _ := p.listOwnedBatchesForCaller(ctx, schemas.Anthropic, callerBatchOwner(ownerVK), batchListCursor{}, false, batchListFilters{})
	require.Equal(t, []string{"b1"}, dataIDs(built.BatchListResponse))
	assert.Equal(t, true, client.lastRetrieveSkipPipe, "retrieve sub-request must skip the plugin pipeline")
	assert.Nil(t, client.lastRetrieveURLPath, "retrieve must not inherit the caller's URL path")
	assert.Nil(t, client.lastRetrieveRawBody, "retrieve must not inherit the caller's raw-body passthrough")
}

// TestListOwnedBatchesForCaller_KeysetErrorEmptyPage proves a ledger keyset error
// fails closed to an empty page (never leaks, never relays the full list).
func TestListOwnedBatchesForCaller_KeysetErrorEmptyPage(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "owner", true)
	ledger := newFakeBatchLedger()
	ledger.listErr = errors.New("db down")
	p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
	p.batchClient = &fakeBatchClient{}
	built, _ := p.listOwnedBatchesForCaller(listBatchCtx("sk-bf-owner", 20), schemas.Anthropic, callerBatchOwner(ownerVK), batchListCursor{}, false, batchListFilters{})
	assert.Empty(t, built.BatchListResponse.Data)
	assert.Nil(t, built.BatchListResponse.FirstID)
	assert.Nil(t, built.BatchListResponse.LastID)
}

// noPagerLedger implements ONLY the 3-method batchLedger capability (no
// ListOwnedBatchIDsPage), simulating an external config store that provides
// ownership but not the keyset pager. It must NOT satisfy batchPager.
type noPagerLedger struct{ inner *fakeBatchLedger }

func (l *noPagerLedger) RegisterManagedBatch(ctx context.Context, batch *configstoreTables.TableManagedBatch, tx ...*gorm.DB) error {
	return l.inner.RegisterManagedBatch(ctx, batch, tx...)
}
func (l *noPagerLedger) GetManagedBatch(ctx context.Context, provider, batchID string, tx ...*gorm.DB) (*configstoreTables.TableManagedBatch, error) {
	return l.inner.GetManagedBatch(ctx, provider, batchID, tx...)
}
func (l *noPagerLedger) ListOwnedBatchIDs(ctx context.Context, provider string, owner configstoreTables.ManagedBatchOwner, tx ...*gorm.DB) ([]string, error) {
	return l.inner.ListOwnedBatchIDs(ctx, provider, owner, tx...)
}

// TestListOwnedBatchesForCaller_NoPagerFallbackEnforcesOwnership proves that a
// store WITHOUT the keyset pager still enforces tenant isolation: the list falls
// back to the owned-id path, returning ONLY the caller's batches (foreign ids
// never appear) and paginating by id — enforcement is NOT disabled.
func TestListOwnedBatchesForCaller_NoPagerFallbackEnforcesOwnership(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "owner", true)
	inner := newFakeBatchLedger()
	ownedSet := map[string]bool{}
	for _, id := range []string{"b1", "b2", "b3"} {
		inner.rows[ledgerKey("anthropic", id)] = &configstoreTables.TableManagedBatch{Provider: "anthropic", BatchID: id, OwnerVirtualKeyID: "vk-owner"}
		ownedSet[id] = true
	}
	inner.rows[ledgerKey("anthropic", "foreign")] = &configstoreTables.TableManagedBatch{Provider: "anthropic", BatchID: "foreign", OwnerVirtualKeyID: "vk-other"}

	nopager := &noPagerLedger{inner: inner}
	// Guard: the fallback ledger must NOT be a pager, or the test is meaningless.
	_, isPager := any(nopager).(batchPager)
	require.False(t, isPager, "noPagerLedger must not satisfy batchPager")

	p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, nopager, nil, "")
	require.Nil(t, p.batchPager, "no pager must be wired for a 3-method-only store")
	p.batchClient = &fakeBatchClient{}
	owner := callerBatchOwner(ownerVK)

	// Page 1 (no cursor), limit 2 — owned only, foreign excluded. The fallback
	// orders by batch id DESC (newest-first), so the page is [b3,b2].
	built, _ := p.listOwnedBatchesForCaller(listBatchCtx("sk-bf-owner", 2), schemas.Anthropic, owner, batchListCursor{}, false, batchListFilters{})
	list := built.BatchListResponse
	assert.Equal(t, []string{"b3", "b2"}, dataIDs(list))
	assert.True(t, list.HasMore)
	ownedOnly(t, list, ownedSet)

	// Page 2 (after b2, forward) — the remaining older owned batch, no foreign leak.
	built, _ = p.listOwnedBatchesForCaller(listBatchCtx("sk-bf-owner", 2), schemas.Anthropic, owner, batchListCursor{id: "b2"}, true, batchListFilters{})
	list = built.BatchListResponse
	assert.Equal(t, []string{"b1"}, dataIDs(list))
	assert.False(t, list.HasMore)
	ownedOnly(t, list, ownedSet)

	// A foreign cursor id is still rejected to an empty page in the fallback.
	built, _ = p.listOwnedBatchesForCaller(listBatchCtx("sk-bf-owner", 2), schemas.Anthropic, owner, batchListCursor{id: "foreign"}, true, batchListFilters{})
	assert.Empty(t, built.BatchListResponse.Data)
}

// TestListOwnedBatchesForCaller_StatusFilter proves a Bedrock ?statusEquals=
// filter returns ONLY matching owned batches and that has_more / continuation
// reflect the FILTERED set (not raw keyset counts): with 2 Completed among 5
// owned and a page size of 1, the filter paginates across the filtered subset.
func TestListOwnedBatchesForCaller_StatusFilter(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "owner", true)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ledger := newFakeBatchLedger()
	ownedSet := map[string]bool{}
	for i, id := range []string{"b1", "b2", "b3", "b4", "b5"} {
		ledger.rows[ledgerKey("bedrock", id)] = &configstoreTables.TableManagedBatch{Provider: "bedrock", BatchID: id, OwnerVirtualKeyID: "vk-owner", CreatedAt: base.Add(time.Duration(i) * time.Minute)}
		ownedSet[id] = true
	}
	// Only b2 and b4 are Completed; the rest are InProgress. The client asks for
	// Bedrock's "Completed" (Bedrock vocabulary) — must map to bifrost "completed".
	statuses := map[string]schemas.BatchStatus{
		"b1": schemas.BatchStatusInProgress,
		"b2": schemas.BatchStatusCompleted,
		"b3": schemas.BatchStatusInProgress,
		"b4": schemas.BatchStatusCompleted,
		"b5": schemas.BatchStatusInProgress,
	}
	client := &statusBatchClient{statuses: statuses}
	p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
	p.batchClient = client
	owner := callerBatchOwner(ownerVK)
	filters := batchListFilters{statusEquals: "Completed"}

	// Whole filtered set at once (limit 20) => exactly the two Completed, NEWEST
	// first [b4,b2], has_more FALSE even though 5 owned rows exist — proving has_more
	// reflects the FILTERED result (all rows examined, no further match), not the
	// raw owned-row count.
	built, berr := p.listOwnedBatchesForCaller(listBatchCtx("sk-bf-owner", 20), schemas.Bedrock, owner, batchListCursor{}, false, filters)
	require.Nil(t, berr)
	assert.Equal(t, []string{"b4", "b2"}, dataIDs(built.BatchListResponse), "only Completed owned batches, newest-first")
	assert.False(t, built.BatchListResponse.HasMore, "all owned rows examined, no further match => has_more false")

	// Page the FILTERED set one match at a time and walk it to completion. Every
	// returned id is a Completed match; the continuation is always the last MATCH
	// (never a filtered-out id); and the walk terminates with no continuation —
	// never a foreign id.
	seen := []string{}
	cursor := batchListCursor{}
	hasCursor := false
	for page := 0; page < 10; page++ {
		built, berr := p.listOwnedBatchesForCaller(listBatchCtx("sk-bf-owner", 1), schemas.Bedrock, owner, cursor, hasCursor, filters)
		require.Nil(t, berr)
		list := built.BatchListResponse
		ownedOnly(t, list, ownedSet)
		for _, b := range list.Data {
			assert.Equal(t, schemas.BatchStatusCompleted, b.Status, "only Completed rows may be returned")
			seen = append(seen, b.ID)
		}
		if !list.HasMore {
			assert.Nil(t, list.LastID, "no continuation token on the terminal filtered page")
			assert.Nil(t, list.NextCursor)
			break
		}
		require.NotNil(t, list.LastID, "a has_more page must carry the continuation (last match) id")
		cursor = batchListCursor{id: *list.LastID}
		hasCursor = true
	}
	assert.Equal(t, []string{"b4", "b2"}, seen, "the filtered walk returns exactly the matches, newest-first, none skipped/foreign")
}

// TestBatchListFinalPageNoContinuation proves that on a terminal page (has_more
// false) NO continuation token is emitted, verified at the dialect converter seam
// for Bedrock (NextToken) and OpenAI (has_more + last_id), and that a non-terminal
// page DOES carry the token — the exact regression P1-3 fixed.
func TestBatchListFinalPageNoContinuation(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "owner", true)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ledger := newFakeBatchLedger()
	for i, id := range []string{"b1", "b2", "b3"} {
		ledger.rows[ledgerKey("bedrock", id)] = &configstoreTables.TableManagedBatch{Provider: "bedrock", BatchID: id, OwnerVirtualKeyID: "vk-owner", CreatedAt: base.Add(time.Duration(i) * time.Minute)}
	}
	p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
	p.batchClient = &fakeBatchClient{}
	owner := callerBatchOwner(ownerVK)

	// Non-terminal page (limit 2 of 3) => continuation present.
	firstResp, _ := p.listOwnedBatchesForCaller(listBatchCtx("sk-bf-owner", 2), schemas.Bedrock, owner, batchListCursor{}, false, batchListFilters{})
	first := firstResp.BatchListResponse
	require.True(t, first.HasMore)
	assert.NotNil(t, bedrock.ToBedrockBatchJobListResponse(first).NextToken, "a non-final Bedrock page must carry NextToken")

	// Terminal page (after b2, forward => only older b1 remains) => NO continuation,
	// at the converter seam.
	finalResp, _ := p.listOwnedBatchesForCaller(listBatchCtx("sk-bf-owner", 2), schemas.Bedrock, owner, batchListCursor{id: "b2"}, true, batchListFilters{})
	final := finalResp.BatchListResponse
	require.False(t, final.HasMore)
	assert.Nil(t, bedrock.ToBedrockBatchJobListResponse(final).NextToken, "Bedrock terminal page must NOT carry NextToken (no extra empty request)")
	// OpenAI serves the BifrostBatchListResponse verbatim: has_more=false + no last_id
	// is the terminal signal; last_id must be nil so a client does not page again.
	assert.False(t, final.HasMore, "OpenAI: has_more must be false on the terminal page")
	assert.Nil(t, final.LastID, "OpenAI: no last_id continuation on the terminal page")
	assert.Nil(t, final.NextCursor)
}

// statusBatchClient is a fan-out client whose retrieve returns a per-id status,
// so status-filter behavior can be exercised.
type statusBatchClient struct {
	statuses      map[string]schemas.BatchStatus
	retrieveCalls []string
}

func (c *statusBatchClient) BatchCancelRequest(ctx *schemas.BifrostContext, req *schemas.BifrostBatchCancelRequest) (*schemas.BifrostBatchCancelResponse, *schemas.BifrostError) {
	return &schemas.BifrostBatchCancelResponse{ID: req.BatchID}, nil
}
func (c *statusBatchClient) BatchRetrieveRequest(ctx *schemas.BifrostContext, req *schemas.BifrostBatchRetrieveRequest) (*schemas.BifrostBatchRetrieveResponse, *schemas.BifrostError) {
	c.retrieveCalls = append(c.retrieveCalls, req.BatchID)
	return &schemas.BifrostBatchRetrieveResponse{ID: req.BatchID, Status: c.statuses[req.BatchID]}, nil
}

// TestBatchListNoClientFallbackFilter proves that in a NO-lifecycle-client
// deployment (batchClient nil), the list is relayed upstream and the PostLLMHook
// owner-filter still scopes the single fetched page to the caller's batches —
// foreign rows are dropped. Isolation holds without the client; only deep
// pagination is unavailable.
func TestBatchListNoClientFallbackFilter(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "owner", true)
	ledger := newFakeBatchLedger()
	ledger.rows[ledgerKey("anthropic", "b-owned-1")] = &configstoreTables.TableManagedBatch{Provider: "anthropic", BatchID: "b-owned-1", OwnerVirtualKeyID: "vk-owner"}
	ledger.rows[ledgerKey("anthropic", "b-owned-2")] = &configstoreTables.TableManagedBatch{Provider: "anthropic", BatchID: "b-owned-2", OwnerVirtualKeyID: "vk-owner"}
	ledger.rows[ledgerKey("anthropic", "b-foreign")] = &configstoreTables.TableManagedBatch{Provider: "anthropic", BatchID: "b-foreign", OwnerVirtualKeyID: "vk-other"}

	p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
	require.Nil(t, p.batchClient, "this test models the client-absent (Go-SDK) deployment")

	// The upstream page the request already fetched: caller's two + a foreign one,
	// plus a foreign next-cursor that must be dropped.
	upstream := &schemas.BifrostResponse{BatchListResponse: &schemas.BifrostBatchListResponse{
		Data: []schemas.BifrostBatchRetrieveResponse{
			{ID: "b-owned-1"}, {ID: "b-foreign"}, {ID: "b-owned-2"},
		},
		HasMore:     true,
		NextCursor:  ptr("cursor-encoding-b-foreign"),
		ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.Anthropic, RawResponse: map[string]any{"data": "FULL UPSTREAM LIST"}},
	}}
	out, berr := p.applyBatchOwnershipPostHook(batchCtx("sk-bf-owner"), upstream, schemas.BatchListRequest, schemas.Anthropic, "sk-bf-owner")
	require.Nil(t, berr)
	list := out.BatchListResponse
	assert.Equal(t, []string{"b-owned-1", "b-owned-2"}, dataIDs(list), "only the caller's own batches survive; foreign dropped")
	assert.False(t, list.HasMore, "the foreign next-cursor is dropped (shallow single-page filter)")
	assert.Nil(t, list.NextCursor)
	assert.Nil(t, list.ExtraFields.RawResponse, "raw passthrough cleared so the converter rebuilds from filtered Data")
	require.NotNil(t, list.LastID)
	assert.Equal(t, "b-owned-2", *list.LastID, "last_id is one of the caller's own ids")
}

// TestListOwnedBatchesForCaller_FilteredSparseHasMoreFalse is the precise P1-3
// regression: a filter with ONE match followed by several non-matches and a page
// size of 1 must report has_more=FALSE with no continuation — the assembler probes
// past the match for a further MATCH (finds none) rather than trusting the raw
// count of remaining owned rows.
func TestListOwnedBatchesForCaller_FilteredSparseHasMoreFalse(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "owner", true)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ledger := newFakeBatchLedger()
	// Newest-first the match (b-hit, newest) is followed by 3 non-matches.
	ids := []string{"b-miss3", "b-miss2", "b-miss1", "b-hit"} // created oldest->newest
	for i, id := range ids {
		ledger.rows[ledgerKey("bedrock", id)] = &configstoreTables.TableManagedBatch{Provider: "bedrock", BatchID: id, OwnerVirtualKeyID: "vk-owner", CreatedAt: base.Add(time.Duration(i) * time.Minute)}
	}
	client := &statusBatchClient{statuses: map[string]schemas.BatchStatus{
		"b-hit":   schemas.BatchStatusCompleted,
		"b-miss1": schemas.BatchStatusInProgress,
		"b-miss2": schemas.BatchStatusInProgress,
		"b-miss3": schemas.BatchStatusInProgress,
	}}
	p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
	p.batchClient = client

	built, berr := p.listOwnedBatchesForCaller(listBatchCtx("sk-bf-owner", 1), schemas.Bedrock, callerBatchOwner(ownerVK), batchListCursor{}, false, batchListFilters{statusEquals: "Completed"})
	require.Nil(t, berr)
	list := built.BatchListResponse
	assert.Equal(t, []string{"b-hit"}, dataIDs(list), "the single match is returned")
	assert.False(t, list.HasMore, "no further MATCH exists => has_more must be false despite 3 remaining owned rows")
	assert.Nil(t, list.LastID, "no continuation token when there is no further match")
	assert.Nil(t, list.NextCursor)
	// The probe examined the trailing non-matches (that is how it learned has_more=false).
	assert.Equal(t, []string{"b-hit", "b-miss1", "b-miss2", "b-miss3"}, client.retrieveCalls)
}

// TestListOwnedBatchesForCaller_FilterScanCapFailsLoud proves the hard scan bound:
// a filter that matches nothing across a very large owned set fails loud rather
// than scanning unboundedly or lying about has_more.
func TestListOwnedBatchesForCaller_FilterScanCapFailsLoud(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "owner", true)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ledger := newFakeBatchLedger()
	statuses := map[string]schemas.BatchStatus{}
	for i := 0; i < maxBatchListOwnedScan+50; i++ {
		id := fmt.Sprintf("b-%05d", i)
		ledger.rows[ledgerKey("bedrock", id)] = &configstoreTables.TableManagedBatch{Provider: "bedrock", BatchID: id, OwnerVirtualKeyID: "vk-owner", CreatedAt: base.Add(time.Duration(i) * time.Second)}
		statuses[id] = schemas.BatchStatusInProgress // none match "Completed"
	}
	p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
	p.batchClient = &statusBatchClient{statuses: statuses}

	_, berr := p.listOwnedBatchesForCaller(listBatchCtx("sk-bf-owner", 1), schemas.Bedrock, callerBatchOwner(ownerVK), batchListCursor{}, false, batchListFilters{statusEquals: "Completed"})
	require.NotNil(t, berr, "a sparse filter over a huge owned set must fail loud, not scan unboundedly")
	require.NotNil(t, berr.StatusCode)
	assert.Equal(t, 400, *berr.StatusCode)
}

// Inert file stubs so statusBatchClient satisfies the shared BatchLifecycleClient
// interface (extended in Part B). The batch tests do not exercise file paths.
func (c *statusBatchClient) FileRetrieveRequest(_ *schemas.BifrostContext, req *schemas.BifrostFileRetrieveRequest) (*schemas.BifrostFileRetrieveResponse, *schemas.BifrostError) {
	return &schemas.BifrostFileRetrieveResponse{ID: req.FileID}, nil
}
func (c *statusBatchClient) FileDeleteRequest(_ *schemas.BifrostContext, req *schemas.BifrostFileDeleteRequest) (*schemas.BifrostFileDeleteResponse, *schemas.BifrostError) {
	return &schemas.BifrostFileDeleteResponse{ID: req.FileID, Deleted: true}, nil
}

// dataIDs extracts the ordered batch ids from a built list page.
func dataIDs(list *schemas.BifrostBatchListResponse) []string {
	ids := make([]string, 0, len(list.Data))
	for _, b := range list.Data {
		ids = append(ids, b.ID)
	}
	return ids
}

func TestParseBatchListCursor(t *testing.T) {
	cases := []struct {
		name       string
		req        *schemas.BifrostBatchListRequest
		wantID     string
		wantBefore bool
		wantHas    bool
	}{
		{"none", &schemas.BifrostBatchListRequest{}, "", false, false},
		{"openai after", &schemas.BifrostBatchListRequest{After: ptr("o1")}, "o1", false, true},
		{"anthropic after_id", &schemas.BifrostBatchListRequest{AfterID: ptr("a1")}, "a1", false, true},
		{"anthropic before_id", &schemas.BifrostBatchListRequest{BeforeID: ptr("bfr")}, "bfr", true, true},
		{"gemini page_token", &schemas.BifrostBatchListRequest{PageToken: ptr("pt")}, "pt", false, true},
		{"forward wins over before", &schemas.BifrostBatchListRequest{AfterID: ptr("a1"), BeforeID: ptr("bfr")}, "a1", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, has := parseBatchListCursor(c.req)
			assert.Equal(t, c.wantHas, has)
			assert.Equal(t, c.wantID, got.id)
			assert.Equal(t, c.wantBefore, got.before)
		})
	}
}

func TestResolveBatchListTarget(t *testing.T) {
	cases := []struct {
		name            string
		limit, pageSize int
		want            int
	}{
		{"limit preferred over pagesize", 10, 5, 10},
		{"pagesize when limit unset", 0, 5, 5},
		{"default when both unset", 0, 0, defaultBatchListLimit},
		{"absurd limit clamped", 1000000000000, 0, maxBatchListPageSize},
		{"absurd pagesize clamped", 0, 999999, maxBatchListPageSize},
		{"negative treated as unset", -3, 0, defaultBatchListLimit},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, resolveBatchListTarget(c.limit, c.pageSize))
		})
	}
}

// An absurd caller ?limit must be clamped before it reaches the keyset query, so
// it can never drive an unbounded page/allocation.
func TestListOwnedBatchesForCaller_AbsurdLimitIsClamped(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "owner", true)
	ledger := &limitCapturingLedger{fakeBatchLedger: newFakeBatchLedger()}
	ledger.rows[ledgerKey("openai", "O1")] = &configstoreTables.TableManagedBatch{Provider: "openai", BatchID: "O1", OwnerVirtualKeyID: "vk-owner"}
	p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
	p.batchClient = &fakeBatchClient{}

	ctx := batchCtx("sk-bf-owner")
	ctx.SetValue(batchListRequestedLimitKey, resolveBatchListTarget(1_000_000_000_000, 0)) // 1e12 from the transport, clamped
	built, _ := p.listOwnedBatchesForCaller(ctx, schemas.OpenAI, callerBatchOwner(ownerVK), batchListCursor{}, false, batchListFilters{})
	require.Equal(t, []string{"O1"}, dataIDs(built.BatchListResponse))
	// The clamped page size (<= maxBatchListPageSize) bounds the per-round-trip
	// fetch, so an absurd caller ?limit can never drive an unbounded query/allocation.
	assert.LessOrEqual(t, ledger.lastLimit, maxBatchListPageSize, "the keyset query fetch size must stay bounded")
}

// limitCapturingLedger records the page limit passed to ListOwnedBatchIDsPage so
// the clamp can be asserted at the query boundary.
type limitCapturingLedger struct {
	*fakeBatchLedger
	lastLimit int
}

func (l *limitCapturingLedger) ListOwnedBatchIDsPage(ctx context.Context, provider string, owner configstoreTables.ManagedBatchOwner, cursorCreatedAt time.Time, cursorID string, hasCursor, before bool, limit int, tx ...*gorm.DB) ([]configstoreTables.TableManagedBatch, bool, error) {
	l.lastLimit = limit
	return l.fakeBatchLedger.ListOwnedBatchIDsPage(ctx, provider, owner, cursorCreatedAt, cursorID, hasCursor, before, limit, tx...)
}

// An operator-set policy must flow through the plugin Config path and change the
// gate behavior (not just be a dead field). newBatchPlugin threads the policy via
// Config -> configureBatchOwnership, the same path plugins.go uses from ClientConfig.
func TestUnknownBatchIDPolicyConfigPath(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "owner", true)
	ghost := retrieveReq(schemas.Anthropic, "msgbatch_ghost") // no ledger row

	cases := []struct {
		name       string
		policy     string
		wantDenied bool
	}{
		{"default empty => deny (secure)", "", true},
		{"operator sets allow", "allow", false},
		{"operator sets deny", "deny", true},
		{"case-insensitive ALLOW", "ALLOW", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, newFakeBatchLedger(), nil, c.policy)
			err := p.enforceBatchOwnershipPreHook(batchCtx("sk-bf-owner"), ghost)
			if c.wantDenied {
				require.NotNil(t, err)
				assert.Equal(t, 404, *err.StatusCode)
			} else {
				assert.Nil(t, err)
			}
		})
	}
}

// The policy + admin list are read through LIVE pointers into ClientConfig, so an
// /api/config update (which mutates ClientConfig in place) takes effect WITHOUT a
// restart — mirroring how ReloadClientConfigFromConfigStore updates other
// governance settings. This is the exact copy-by-value liveness gap codex flagged.
func TestBatchOwnershipConfigLiveUpdate(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "owner", true)
	ghost := retrieveReq(schemas.Anthropic, "msgbatch_ghost")

	// Simulate the live ClientConfig fields the plugin reads via pointers.
	policy := UnknownBatchIDPolicyAllow
	admin := []string{}
	p := newBatchPluginWithConfig(t, []*configstoreTables.TableVirtualKey{ownerVK}, newFakeBatchLedger(), &Config{
		UnknownBatchIDPolicy:    &policy,
		BatchAdminVirtualKeyIDs: &admin,
	})

	// allow: a ledger-absent id falls through.
	assert.Nil(t, p.enforceBatchOwnershipPreHook(batchCtx("sk-bf-owner"), ghost))

	// Operator flips allow -> deny via /api/config; the in-place ClientConfig
	// mutation is modeled by writing through the same field the pointer targets.
	policy = UnknownBatchIDPolicyDeny
	err := p.enforceBatchOwnershipPreHook(batchCtx("sk-bf-owner"), ghost)
	require.NotNil(t, err, "deny must take effect without reconfiguring the plugin")
	assert.Equal(t, 404, *err.StatusCode)

	// Admin list is also live: adding the caller's VK now bypasses the gate.
	assert.NotNil(t, p.enforceBatchOwnershipPreHook(batchCtx("sk-bf-owner"), ghost))
	admin = append(admin, "vk-owner")
	assert.Nil(t, p.enforceBatchOwnershipPreHook(batchCtx("sk-bf-owner"), ghost), "admin bypass must take effect live")
}
