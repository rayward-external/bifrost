package governance

import (
	"context"
	"errors"
	"testing"

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

// fakeBatchClient is an in-process BatchLifecycleClient stand-in. It records the
// compensating-cancel calls and the sub-request context state, to assert the
// cancel targets the right id and does NOT inherit the caller's transport state.
type fakeBatchClient struct {
	cancelCalls        []string
	cancelErr          *schemas.BifrostError
	lastCancelURLPath  any
	lastCancelRawBody  any
	lastCancelSkipPipe any
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

func batch(id string) schemas.BifrostBatchRetrieveResponse {
	return schemas.BifrostBatchRetrieveResponse{ID: id}
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

// TestFilterBatchListForCaller covers the simplified single-page owner filter: it
// keeps only owned rows from the one upstream page, sets first_id/last_id to the
// caller's own ids, DROPS any cursor, and clears the raw passthrough. No second
// upstream page is fetched and no foreign id/cursor can appear in the response.
func TestFilterBatchListForCaller(t *testing.T) {
	ownerVKID, teamID := "vk-owner", "team-1"
	ownerVK := buildVirtualKey(ownerVKID, "sk-bf-owner", "owner", true)
	ownerVK.TeamID = &teamID
	adminVK := buildVirtualKey("vk-admin", "sk-bf-admin", "admin", true)
	allVKs := []*configstoreTables.TableVirtualKey{ownerVK, adminVK}

	seedLedger := func(provider string, ownedIDs ...string) *fakeBatchLedger {
		l := newFakeBatchLedger()
		for _, id := range ownedIDs {
			l.rows[ledgerKey(provider, id)] = &configstoreTables.TableManagedBatch{
				Provider: provider, BatchID: id, OwnerVirtualKeyID: ownerVKID, OwnerTeamID: teamID,
			}
		}
		return l
	}

	// noForeign asserts the sanitized response carries no foreign id anywhere and
	// no usable cursor.
	noForeign := func(t *testing.T, list *schemas.BifrostBatchListResponse, ownedIDs map[string]bool) {
		t.Helper()
		for _, b := range list.Data {
			assert.True(t, ownedIDs[b.ID], "data must contain only owned ids, got %q", b.ID)
		}
		if list.FirstID != nil {
			assert.True(t, ownedIDs[*list.FirstID], "first_id must be owned, got %q", *list.FirstID)
		}
		if list.LastID != nil {
			assert.True(t, ownedIDs[*list.LastID], "last_id must be owned, got %q", *list.LastID)
		}
		assert.Nil(t, list.NextCursor, "no cursor may be emitted")
		assert.False(t, list.HasMore, "has_more must be false (single-page)")
		assert.Nil(t, list.ExtraFields.RawResponse, "raw passthrough must be cleared")
	}

	// The upstream page carries a foreign NextCursor + HasMore=true (as OpenAI
	// would): both must be dropped, proving no foreign cursor leaks.
	mixedPage := func(provider schemas.ModelProvider) *schemas.BifrostResponse {
		return &schemas.BifrostResponse{BatchListResponse: &schemas.BifrostBatchListResponse{
			Data:        []schemas.BifrostBatchRetrieveResponse{batch("b-owned"), batch("b-foreign-1"), batch("b-foreign-2")},
			HasMore:     true,
			NextCursor:  ptr("cursor-encoding-b-foreign-2"),
			ExtraFields: schemas.BifrostResponseExtraFields{Provider: provider, RawResponse: map[string]any{"data": "FULL UPSTREAM LIST"}},
		}}
	}

	t.Run("anthropic single page: only owned, first_id/last_id own, cursor dropped", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, seedLedger("anthropic", "b-owned"), nil, "")
		result := mixedPage(schemas.Anthropic)
		out, berr := p.applyBatchOwnershipPostHook(batchCtx("sk-bf-owner"), result, schemas.BatchListRequest, schemas.Anthropic, "sk-bf-owner")
		assert.Nil(t, berr)
		require.Len(t, out.BatchListResponse.Data, 1)
		assert.Equal(t, "b-owned", out.BatchListResponse.Data[0].ID)
		require.NotNil(t, out.BatchListResponse.LastID)
		assert.Equal(t, "b-owned", *out.BatchListResponse.LastID)
		noForeign(t, out.BatchListResponse, map[string]bool{"b-owned": true})
	})

	t.Run("openai single page: no foreign id or cursor survives", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, seedLedger("openai", "b-owned"), nil, "")
		result := mixedPage(schemas.OpenAI)
		out, _ := p.applyBatchOwnershipPostHook(batchCtx("sk-bf-owner"), result, schemas.BatchListRequest, schemas.OpenAI, "sk-bf-owner")
		require.Len(t, out.BatchListResponse.Data, 1)
		noForeign(t, out.BatchListResponse, map[string]bool{"b-owned": true})
	})

	t.Run("admin sees full list and keeps raw", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, seedLedger("anthropic", "b-owned"), []string{"vk-admin"}, "")
		result := mixedPage(schemas.Anthropic)
		out, _ := p.applyBatchOwnershipPostHook(batchCtx("sk-bf-admin"), result, schemas.BatchListRequest, schemas.Anthropic, "sk-bf-admin")
		assert.Len(t, out.BatchListResponse.Data, 3)
		assert.NotNil(t, out.BatchListResponse.ExtraFields.RawResponse)
	})

	t.Run("no owned rows on the page => empty, cursor-less", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, seedLedger("anthropic", "b-owned-elsewhere"), nil, "")
		result := mixedPage(schemas.Anthropic)
		out, _ := p.applyBatchOwnershipPostHook(batchCtx("sk-bf-owner"), result, schemas.BatchListRequest, schemas.Anthropic, "sk-bf-owner")
		assert.Empty(t, out.BatchListResponse.Data)
		noForeign(t, out.BatchListResponse, map[string]bool{})
		assert.Nil(t, out.BatchListResponse.FirstID)
		assert.Nil(t, out.BatchListResponse.LastID)
	})

	t.Run("ledger error fails closed to empty list", func(t *testing.T) {
		l := seedLedger("anthropic", "b-owned")
		l.listErr = errors.New("db down")
		p := newBatchPlugin(t, allVKs, l, nil, "")
		result := mixedPage(schemas.Anthropic)
		out, _ := p.applyBatchOwnershipPostHook(batchCtx("sk-bf-owner"), result, schemas.BatchListRequest, schemas.Anthropic, "sk-bf-owner")
		assert.Empty(t, out.BatchListResponse.Data)
		noForeign(t, out.BatchListResponse, map[string]bool{})
	})

	t.Run("unknown VK fails closed to empty list", func(t *testing.T) {
		p := newBatchPlugin(t, allVKs, seedLedger("anthropic", "b-owned"), nil, "")
		result := mixedPage(schemas.Anthropic)
		out, _ := p.applyBatchOwnershipPostHook(batchCtx("sk-bf-nonexistent"), result, schemas.BatchListRequest, schemas.Anthropic, "sk-bf-nonexistent")
		assert.Empty(t, out.BatchListResponse.Data)
		noForeign(t, out.BatchListResponse, map[string]bool{})
	})
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

// An absurd caller ?limit must not drive a huge slice pre-allocation.
func TestFilterBatchListForCaller_AbsurdLimitIsClamped(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "owner", true)
	ledger := newFakeBatchLedger()
	ledger.rows[ledgerKey("openai", "O1")] = &configstoreTables.TableManagedBatch{Provider: "openai", BatchID: "O1", OwnerVirtualKeyID: "vk-owner"}
	p := newBatchPlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")

	ctx := batchCtx("sk-bf-owner")
	ctx.SetValue(batchListRequestedLimitKey, 1_000_000_000_000) // 1e12, unclamped from the transport
	result := &schemas.BifrostResponse{BatchListResponse: &schemas.BifrostBatchListResponse{
		Data:        []schemas.BifrostBatchRetrieveResponse{batch("O1"), batch("F1")},
		HasMore:     false,
		ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.OpenAI},
	}}
	// Must complete without OOM (pre-alloc capacity is clamped to maxBatchListPageSize).
	out, berr := p.applyBatchOwnershipPostHook(ctx, result, schemas.BatchListRequest, schemas.OpenAI, "sk-bf-owner")
	assert.Nil(t, berr)
	require.Len(t, out.BatchListResponse.Data, 1)
	assert.Equal(t, "O1", out.BatchListResponse.Data[0].ID)
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
