package governance

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// fakeFileLedger is an in-memory fileLedger + filePager that mirrors the real
// store's semantics (idempotent register keyed by provider|file_id; OR-ownership;
// newest-first keyset) so the gate/capture/list logic can be exercised without a
// database.
type fakeFileLedger struct {
	rows        map[string]*configstoreTables.TableManagedFile // key: provider + "|" + fileID
	registerErr error
	getErr      error
	listErr     error
	registered  int
}

func newFakeFileLedger() *fakeFileLedger {
	return &fakeFileLedger{rows: map[string]*configstoreTables.TableManagedFile{}}
}

func fileLedgerKey(provider, fileID string) string { return provider + "|" + fileID }

func (f *fakeFileLedger) RegisterManagedFile(_ context.Context, file *configstoreTables.TableManagedFile, _ ...*gorm.DB) error {
	if f.registerErr != nil {
		return f.registerErr
	}
	key := fileLedgerKey(file.Provider, file.FileID)
	if _, exists := f.rows[key]; !exists { // DoNothing on conflict: first writer wins
		cp := *file
		f.rows[key] = &cp
	}
	f.registered++
	return nil
}

func (f *fakeFileLedger) GetManagedFile(_ context.Context, provider, fileID string, _ ...*gorm.DB) (*configstoreTables.TableManagedFile, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.rows[fileLedgerKey(provider, fileID)], nil
}

// ListOwnedFileIDsPage mirrors the RDB keyset: owner-matched rows for a provider,
// ordered NEWEST-FIRST by (created_at DESC, file_id DESC) by default — or
// OLDEST-FIRST when asc is set — seeked strictly PAST the cursor in the sort
// direction, with a limit+1 hasMore probe.
func (f *fakeFileLedger) ListOwnedFileIDsPage(_ context.Context, provider string, owner configstoreTables.ManagedBatchOwner, cursorCreatedAt time.Time, cursorID string, hasCursor, asc bool, limit int, _ ...*gorm.DB) ([]configstoreTables.TableManagedFile, bool, error) {
	if f.listErr != nil {
		return nil, false, f.listErr
	}
	matched := []configstoreTables.TableManagedFile{}
	for _, row := range f.rows {
		if row.Provider == provider && owner.Matches(row.OwnerVirtualKeyID, row.OwnerTeamID, row.OwnerCustomerID) {
			matched = append(matched, *row)
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			if asc {
				return matched[i].CreatedAt.Before(matched[j].CreatedAt)
			}
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}
		if asc {
			return matched[i].FileID < matched[j].FileID
		}
		return matched[i].FileID > matched[j].FileID
	})

	filtered := []configstoreTables.TableManagedFile{}
	for _, row := range matched {
		if hasCursor {
			var past bool
			if asc { // seek strictly NEWER than the cursor tuple
				past = row.CreatedAt.After(cursorCreatedAt) || (row.CreatedAt.Equal(cursorCreatedAt) && row.FileID > cursorID)
			} else { // seek strictly OLDER than the cursor tuple
				past = row.CreatedAt.Before(cursorCreatedAt) || (row.CreatedAt.Equal(cursorCreatedAt) && row.FileID < cursorID)
			}
			if !past {
				continue
			}
		}
		filtered = append(filtered, row)
	}

	if limit <= 0 {
		limit = 1
	}
	hasMore := len(filtered) > limit
	end := limit
	if end > len(filtered) {
		end = len(filtered)
	}
	return append([]configstoreTables.TableManagedFile{}, filtered[:end]...), hasMore, nil
}

// SetFileSourceBatch mirrors the RDB owner-guarded provenance update: it stamps
// SourceBatchID onto an EXISTING owner-matched row and NEVER mints or re-assigns.
func (f *fakeFileLedger) SetFileSourceBatch(_ context.Context, provider, fileID, sourceBatchID string, owner configstoreTables.ManagedBatchOwner, _ ...*gorm.DB) (bool, error) {
	if provider == "" || fileID == "" || sourceBatchID == "" || owner.VirtualKeyID == "" {
		return false, nil
	}
	row, exists := f.rows[fileLedgerKey(provider, fileID)]
	if !exists || !owner.Matches(row.OwnerVirtualKeyID, row.OwnerTeamID, row.OwnerCustomerID) {
		return false, nil
	}
	row.SourceBatchID = sourceBatchID
	return true, nil
}

// fakeFileClient is an in-process BatchLifecycleClient stand-in exercising only
// the file paths. It records the compensating-delete and retrieve fan-out calls
// and the sub-request context state, to assert isolation.
type fakeFileClient struct {
	// Compensating delete after a failed capture.
	deleteCalls        []string
	deleteErr          *schemas.BifrostError
	lastDeleteURLPath  any
	lastDeleteRawBody  any
	lastDeleteSkipPipe any

	// Retrieve fan-out (owner-scoped list assembly).
	retrieveCalls        []string
	retrieveErrIDs       map[string]bool                // ids whose retrieve errors (skipped from the page)
	purposes             map[string]schemas.FilePurpose // per-id purpose echoed on retrieve
	lastRetrieveURLPath  any
	lastRetrieveRawBody  any
	lastRetrieveSkipPipe any
}

// The batch methods are inert here — this fake is only wired into file paths.
func (c *fakeFileClient) BatchCancelRequest(_ *schemas.BifrostContext, req *schemas.BifrostBatchCancelRequest) (*schemas.BifrostBatchCancelResponse, *schemas.BifrostError) {
	return &schemas.BifrostBatchCancelResponse{ID: req.BatchID}, nil
}
func (c *fakeFileClient) BatchRetrieveRequest(_ *schemas.BifrostContext, req *schemas.BifrostBatchRetrieveRequest) (*schemas.BifrostBatchRetrieveResponse, *schemas.BifrostError) {
	return &schemas.BifrostBatchRetrieveResponse{ID: req.BatchID}, nil
}

func (c *fakeFileClient) FileRetrieveRequest(ctx *schemas.BifrostContext, req *schemas.BifrostFileRetrieveRequest) (*schemas.BifrostFileRetrieveResponse, *schemas.BifrostError) {
	c.retrieveCalls = append(c.retrieveCalls, req.FileID)
	c.lastRetrieveURLPath = ctx.Value(schemas.BifrostContextKeyURLPath)
	c.lastRetrieveRawBody = ctx.Value(schemas.BifrostContextKeyUseRawRequestBody)
	c.lastRetrieveSkipPipe = ctx.Value(schemas.BifrostContextKeySkipPluginPipeline)
	if c.retrieveErrIDs[req.FileID] {
		return nil, &schemas.BifrostError{Error: &schemas.ErrorField{Message: "retrieve failed"}}
	}
	return &schemas.BifrostFileRetrieveResponse{ID: req.FileID, Purpose: c.purposes[req.FileID]}, nil
}

func (c *fakeFileClient) FileDeleteRequest(ctx *schemas.BifrostContext, req *schemas.BifrostFileDeleteRequest) (*schemas.BifrostFileDeleteResponse, *schemas.BifrostError) {
	c.deleteCalls = append(c.deleteCalls, req.FileID)
	c.lastDeleteURLPath = ctx.Value(schemas.BifrostContextKeyURLPath)
	c.lastDeleteRawBody = ctx.Value(schemas.BifrostContextKeyUseRawRequestBody)
	c.lastDeleteSkipPipe = ctx.Value(schemas.BifrostContextKeySkipPluginPipeline)
	if c.deleteErr != nil {
		return nil, c.deleteErr
	}
	return &schemas.BifrostFileDeleteResponse{ID: req.FileID, Deleted: true}, nil
}

// newFilePlugin builds a governance plugin backed by an in-memory VK store, the
// given file ledger (also wired as the pager when it implements it), and the
// shared lifecycle admin/policy knobs (reused from the batch config path).
func newFilePlugin(t *testing.T, vks []*configstoreTables.TableVirtualKey, fileLdg fileLedger, adminIDs []string, policy string) *GovernancePlugin {
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
	// Reuse the shared lifecycle knobs via the batch config path (the file gate
	// reads p.batchAdminVKIDs / p.batchUnknownIDPolicy).
	p.configureBatchOwnership(&Config{
		BatchAdminVirtualKeyIDs: &adminIDs,
		UnknownBatchIDPolicy:    &policy,
	}, nil)
	p.fileLedger = fileLdg
	if pager, ok := fileLdg.(filePager); ok {
		p.filePager = pager
	}
	if prov, ok := fileLdg.(fileProvenanceLinker); ok {
		p.fileProvenance = prov
	}
	return p
}

func fileCtx(vkValue string) *schemas.BifrostContext {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if vkValue != "" {
		ctx.SetValue(schemas.BifrostContextKeyVirtualKey, vkValue)
	}
	return ctx
}

func contentReq(provider schemas.ModelProvider, fileID string) *schemas.BifrostRequest {
	return &schemas.BifrostRequest{
		RequestType:        schemas.FileContentRequest,
		FileContentRequest: &schemas.BifrostFileContentRequest{Provider: provider, FileID: fileID},
	}
}

func TestIsFilePerIDVerb(t *testing.T) {
	assert.True(t, isFilePerIDVerb(schemas.FileRetrieveRequest))
	assert.True(t, isFilePerIDVerb(schemas.FileDeleteRequest))
	assert.True(t, isFilePerIDVerb(schemas.FileContentRequest))
	assert.False(t, isFilePerIDVerb(schemas.FileUploadRequest))
	assert.False(t, isFilePerIDVerb(schemas.FileListRequest))
	assert.False(t, isFilePerIDVerb(schemas.BatchRetrieveRequest))
}

func TestFileTargetFromRequest(t *testing.T) {
	cases := []struct {
		name string
		req  *schemas.BifrostRequest
		want string
	}{
		{"content", contentReq(schemas.OpenAI, "file-c"), "file-c"},
		{"retrieve", &schemas.BifrostRequest{RequestType: schemas.FileRetrieveRequest, FileRetrieveRequest: &schemas.BifrostFileRetrieveRequest{Provider: schemas.OpenAI, FileID: "file-r"}}, "file-r"},
		{"delete", &schemas.BifrostRequest{RequestType: schemas.FileDeleteRequest, FileDeleteRequest: &schemas.BifrostFileDeleteRequest{Provider: schemas.OpenAI, FileID: "file-d"}}, "file-d"},
		{"non-per-id (upload)", &schemas.BifrostRequest{RequestType: schemas.FileUploadRequest}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, id := fileTargetFromRequest(c.req)
			assert.Equal(t, c.want, id)
		})
	}
}

func TestEnforceFileOwnershipPreHook(t *testing.T) {
	ownerVKID := "vk-owner"
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
	foreignVK := buildVK("vk-foreign", "sk-bf-foreign", nil, nil)
	adminVK := buildVK("vk-admin", "sk-bf-admin", nil, nil)

	newLedgerWithFile := func() *fakeFileLedger {
		l := newFakeFileLedger()
		l.rows[fileLedgerKey("openai", "file-owned")] = &configstoreTables.TableManagedFile{
			Provider: "openai", FileID: "file-owned",
			OwnerVirtualKeyID: ownerVKID, OwnerTeamID: teamID, OwnerCustomerID: custID,
		}
		return l
	}

	allVKs := []*configstoreTables.TableVirtualKey{ownerVK, teammateVK, custMateVK, foreignVK, adminVK}

	t.Run("owner is allowed", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, newLedgerWithFile(), nil, "deny")
		assert.Nil(t, p.enforceFileOwnershipPreHook(fileCtx("sk-bf-owner"), contentReq(schemas.OpenAI, "file-owned")))
	})

	t.Run("teammate (shared team) is allowed", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, newLedgerWithFile(), nil, "deny")
		assert.Nil(t, p.enforceFileOwnershipPreHook(fileCtx("sk-bf-teammate"), contentReq(schemas.OpenAI, "file-owned")))
	})

	t.Run("customer-mate (shared customer) is allowed", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, newLedgerWithFile(), nil, "deny")
		assert.Nil(t, p.enforceFileOwnershipPreHook(fileCtx("sk-bf-custmate"), contentReq(schemas.OpenAI, "file-owned")))
	})

	t.Run("foreign caller gets 404 existence-hiding", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, newLedgerWithFile(), nil, "deny")
		berr := p.enforceFileOwnershipPreHook(fileCtx("sk-bf-foreign"), contentReq(schemas.OpenAI, "file-owned"))
		require.NotNil(t, berr)
		assert.Equal(t, 404, *berr.StatusCode)
		assert.Equal(t, "file not found", berr.Error.Message)
	})

	t.Run("unknown id under deny policy => 404", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, newLedgerWithFile(), nil, "deny")
		berr := p.enforceFileOwnershipPreHook(fileCtx("sk-bf-owner"), contentReq(schemas.OpenAI, "file-unknown"))
		require.NotNil(t, berr)
		assert.Equal(t, 404, *berr.StatusCode)
	})

	t.Run("unknown id under allow policy => relayed", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, newLedgerWithFile(), nil, "allow")
		assert.Nil(t, p.enforceFileOwnershipPreHook(fileCtx("sk-bf-foreign"), contentReq(schemas.OpenAI, "file-unknown")))
	})

	t.Run("foreign-owned id is denied even under allow policy", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, newLedgerWithFile(), nil, "allow")
		berr := p.enforceFileOwnershipPreHook(fileCtx("sk-bf-foreign"), contentReq(schemas.OpenAI, "file-owned"))
		require.NotNil(t, berr)
		assert.Equal(t, 404, *berr.StatusCode)
	})

	t.Run("admin VK bypasses the gate", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, newLedgerWithFile(), []string{"vk-admin"}, "deny")
		assert.Nil(t, p.enforceFileOwnershipPreHook(fileCtx("sk-bf-admin"), contentReq(schemas.OpenAI, "file-owned")))
	})

	t.Run("ledger read error fails closed (404)", func(t *testing.T) {
		l := newLedgerWithFile()
		l.getErr = errors.New("db down")
		p := newFilePlugin(t, allVKs, l, nil, "allow") // even allow policy fails closed on error
		berr := p.enforceFileOwnershipPreHook(fileCtx("sk-bf-owner"), contentReq(schemas.OpenAI, "file-owned"))
		require.NotNil(t, berr)
		assert.Equal(t, 404, *berr.StatusCode)
	})

	t.Run("non-per-id verb is ignored", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, newLedgerWithFile(), nil, "deny")
		assert.Nil(t, p.enforceFileOwnershipPreHook(fileCtx("sk-bf-owner"), &schemas.BifrostRequest{RequestType: schemas.FileUploadRequest}))
	})

	t.Run("no ledger => no enforcement", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, newLedgerWithFile(), nil, "deny")
		p.fileLedger = nil
		assert.Nil(t, p.enforceFileOwnershipPreHook(fileCtx("sk-bf-foreign"), contentReq(schemas.OpenAI, "file-owned")))
	})

	t.Run("no virtual key => no enforcement (single-tenant path)", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, newLedgerWithFile(), nil, "deny")
		assert.Nil(t, p.enforceFileOwnershipPreHook(fileCtx(""), contentReq(schemas.OpenAI, "file-owned")))
	})
}

func TestCaptureFileOwnership(t *testing.T) {
	teamID := "team-1"
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "vk", true)
	ownerVK.TeamID = &teamID

	uploadResult := func(id string, purpose schemas.FilePurpose) *schemas.BifrostResponse {
		return &schemas.BifrostResponse{
			FileUploadResponse: &schemas.BifrostFileUploadResponse{
				ID:          id,
				Purpose:     purpose,
				ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.OpenAI},
			},
		}
	}

	t.Run("success records the owner tuple + purpose", func(t *testing.T) {
		ledger := newFakeFileLedger()
		p := newFilePlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
		out, berr := p.applyFileOwnershipPostHook(fileCtx("sk-bf-owner"), uploadResult("file-new", schemas.FilePurposeBatch), schemas.FileUploadRequest, schemas.OpenAI, "sk-bf-owner")
		assert.Nil(t, berr)
		row := ledger.rows[fileLedgerKey("openai", "file-new")]
		require.NotNil(t, row)
		assert.Equal(t, "vk-owner", row.OwnerVirtualKeyID)
		assert.Equal(t, teamID, row.OwnerTeamID)
		assert.Equal(t, "batch", row.Purpose)
		require.NotNil(t, out)
	})

	t.Run("ledger fail + compensating delete ok => retryable 503, file deleted, isolated sub-request", func(t *testing.T) {
		ledger := newFakeFileLedger()
		ledger.registerErr = errors.New("db down")
		client := &fakeFileClient{}
		p := newFilePlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
		p.batchClient = client
		parent := fileCtx("sk-bf-owner")
		parent.SetValue(schemas.BifrostContextKeyURLPath, "/v1/files")
		parent.SetValue(schemas.BifrostContextKeyUseRawRequestBody, true)
		out, berr := p.applyFileOwnershipPostHook(parent, uploadResult("file-x", schemas.FilePurposeBatch), schemas.FileUploadRequest, schemas.OpenAI, "sk-bf-owner")
		require.NotNil(t, berr)
		assert.Nil(t, out)
		assert.Equal(t, 503, *berr.StatusCode)
		assert.Equal(t, []string{"file-x"}, client.deleteCalls, "the just-uploaded file must be deleted by id")
		// Sub-request isolation: no inherited transport state, pipeline skipped.
		assert.Nil(t, client.lastDeleteURLPath, "delete must not inherit the caller's URL path")
		assert.Nil(t, client.lastDeleteRawBody, "delete must not inherit the caller's raw-body passthrough")
		assert.Equal(t, true, client.lastDeleteSkipPipe, "delete must skip the plugin pipeline")
	})

	t.Run("ledger fail + compensating delete fail => 500 orphan warning", func(t *testing.T) {
		ledger := newFakeFileLedger()
		ledger.registerErr = errors.New("db down")
		client := &fakeFileClient{deleteErr: &schemas.BifrostError{Error: &schemas.ErrorField{Message: "delete failed"}}}
		p := newFilePlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
		p.batchClient = client
		out, berr := p.applyFileOwnershipPostHook(fileCtx("sk-bf-owner"), uploadResult("file-y", schemas.FilePurposeBatch), schemas.FileUploadRequest, schemas.OpenAI, "sk-bf-owner")
		require.NotNil(t, berr)
		assert.Nil(t, out)
		assert.Equal(t, 500, *berr.StatusCode)
	})

	t.Run("no virtual key => no-op (nothing to protect)", func(t *testing.T) {
		ledger := newFakeFileLedger()
		p := newFilePlugin(t, []*configstoreTables.TableVirtualKey{ownerVK}, ledger, nil, "")
		out, berr := p.applyFileOwnershipPostHook(fileCtx(""), uploadResult("file-z", schemas.FilePurposeBatch), schemas.FileUploadRequest, schemas.OpenAI, "")
		assert.Nil(t, berr)
		require.NotNil(t, out)
		assert.Empty(t, ledger.rows)
	})
}

func TestBuildFileListShortCircuit_DeepOwnerScoped(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "vk", true)
	foreignVK := buildVirtualKey("vk-foreign", "sk-bf-foreign", "vk-foreign", true)
	adminVK := buildVirtualKey("vk-admin", "sk-bf-admin", "vk-admin", true)
	allVKs := []*configstoreTables.TableVirtualKey{ownerVK, foreignVK, adminVK}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seedLedger := func() *fakeFileLedger {
		l := newFakeFileLedger()
		for i, id := range []string{"f1", "f2", "f3", "f4", "f5"} {
			l.rows[fileLedgerKey("openai", id)] = &configstoreTables.TableManagedFile{
				Provider: "openai", FileID: id, OwnerVirtualKeyID: "vk-owner", CreatedAt: base.Add(time.Duration(i) * time.Minute),
			}
		}
		// Foreign row that must never surface.
		l.rows[fileLedgerKey("openai", "fx")] = &configstoreTables.TableManagedFile{
			Provider: "openai", FileID: "fx", OwnerVirtualKeyID: "vk-foreign", CreatedAt: base.Add(90 * time.Second),
		}
		return l
	}

	listReq := func(after *string, purpose schemas.FilePurpose) *schemas.BifrostFileListRequest {
		return &schemas.BifrostFileListRequest{Provider: schemas.OpenAI, After: after, Purpose: purpose}
	}

	// Helper: run the short-circuit as PreLLMHook would, with the limit set.
	runList := func(p *GovernancePlugin, ctx *schemas.BifrostContext, req *schemas.BifrostFileListRequest, limit int) *schemas.LLMPluginShortCircuit {
		ctx.SetValue(fileListRequestedLimitKey, resolveFileListTarget(limit))
		return p.buildFileListShortCircuit(ctx, req)
	}

	t.Run("multi-page keyset, newest-first, no foreign id, final page has no continuation", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, seedLedger(), nil, "deny")
		client := &fakeFileClient{}
		p.batchClient = client

		// Page 1 (limit 2): newest-first f5, f4, has_more, after=f4.
		sc := runList(p, fileCtx("sk-bf-owner"), listReq(nil, ""), 2)
		require.NotNil(t, sc)
		list := sc.Response.FileListResponse
		assert.Equal(t, []string{"f5", "f4"}, fileDataIDs(list))
		assert.True(t, list.HasMore)
		require.NotNil(t, list.After)
		assert.Equal(t, "f4", *list.After)
		assert.NotContains(t, fileDataIDs(list), "fx")

		// Page 2 (after f4, limit 2): f3, f2.
		sc = runList(p, fileCtx("sk-bf-owner"), listReq(ptr("f4"), ""), 2)
		require.NotNil(t, sc)
		list = sc.Response.FileListResponse
		assert.Equal(t, []string{"f3", "f2"}, fileDataIDs(list))
		assert.True(t, list.HasMore)

		// Page 3 (after f2, limit 2): only f1 remains, no continuation.
		sc = runList(p, fileCtx("sk-bf-owner"), listReq(ptr("f2"), ""), 2)
		require.NotNil(t, sc)
		list = sc.Response.FileListResponse
		assert.Equal(t, []string{"f1"}, fileDataIDs(list))
		assert.False(t, list.HasMore)
		assert.Nil(t, list.After, "the final page must not hand back a continuation cursor")
	})

	t.Run("foreign / unknown cursor yields an empty page (no leak, no confirmation)", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, seedLedger(), nil, "deny")
		p.batchClient = &fakeFileClient{}
		// The owner pages with the FOREIGN file's id as the cursor.
		sc := runList(p, fileCtx("sk-bf-owner"), listReq(ptr("fx"), ""), 10)
		require.NotNil(t, sc)
		assert.Empty(t, fileDataIDs(sc.Response.FileListResponse))
	})

	t.Run("purpose filter is re-applied against retrieved state", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, seedLedger(), nil, "deny")
		client := &fakeFileClient{purposes: map[string]schemas.FilePurpose{
			"f5": schemas.FilePurposeBatch,
			"f4": schemas.FilePurposeUserData,
			"f3": schemas.FilePurposeBatch,
			"f2": schemas.FilePurposeUserData,
			"f1": schemas.FilePurposeBatch,
		}}
		p.batchClient = client
		sc := runList(p, fileCtx("sk-bf-owner"), listReq(nil, schemas.FilePurposeBatch), 10)
		require.NotNil(t, sc)
		// Only the batch-purpose files, newest-first.
		assert.Equal(t, []string{"f5", "f3", "f1"}, fileDataIDs(sc.Response.FileListResponse))
	})

	t.Run("admin caller relays upstream (nil short-circuit)", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, seedLedger(), []string{"vk-admin"}, "deny")
		p.batchClient = &fakeFileClient{}
		assert.Nil(t, runList(p, fileCtx("sk-bf-admin"), listReq(nil, ""), 10))
	})

	t.Run("no virtual key relays upstream (single-tenant)", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, seedLedger(), nil, "deny")
		p.batchClient = &fakeFileClient{}
		assert.Nil(t, runList(p, fileCtx(""), listReq(nil, ""), 10))
	})

	t.Run("unknown VK fails closed to an empty short-circuit page", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, seedLedger(), nil, "deny")
		p.batchClient = &fakeFileClient{}
		sc := runList(p, fileCtx("sk-bf-ghost"), listReq(nil, ""), 10)
		require.NotNil(t, sc)
		assert.Empty(t, fileDataIDs(sc.Response.FileListResponse))
	})

	t.Run("no pager or no client => fallback: relay + flag for post-hook filter", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, seedLedger(), nil, "deny")
		p.filePager = nil // pager unavailable
		p.batchClient = &fakeFileClient{}
		ctx := fileCtx("sk-bf-owner")
		assert.Nil(t, runList(p, ctx, listReq(nil, ""), 10), "must relay upstream (nil short-circuit)")
		assert.Equal(t, true, ctx.Value(fileListFilterInPostKey), "must flag the request for the single-page post-hook filter")

		// Also: pager present but no client => same fallback.
		p2 := newFilePlugin(t, allVKs, seedLedger(), nil, "deny")
		p2.batchClient = nil
		ctx2 := fileCtx("sk-bf-owner")
		assert.Nil(t, runList(p2, ctx2, listReq(nil, ""), 10))
		assert.Equal(t, true, ctx2.Value(fileListFilterInPostKey))
	})

	// P2-A: order=asc walks the owned set OLDEST-FIRST with a matching cursor.
	t.Run("order=asc returns oldest-first with a matching cursor", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, seedLedger(), nil, "deny")
		p.batchClient = &fakeFileClient{}
		ascReq := func(after *string) *schemas.BifrostFileListRequest {
			r := listReq(after, "")
			r.Order = ptr("asc")
			return r
		}
		// Page 1 asc (limit 2): oldest-first f1, f2, has_more, after=f2.
		sc := runList(p, fileCtx("sk-bf-owner"), ascReq(nil), 2)
		require.NotNil(t, sc)
		list := sc.Response.FileListResponse
		assert.Equal(t, []string{"f1", "f2"}, fileDataIDs(list))
		assert.True(t, list.HasMore)
		require.NotNil(t, list.After)
		assert.Equal(t, "f2", *list.After)
		assert.NotContains(t, fileDataIDs(list), "fx")

		// Page 2 asc (after f2, limit 2): f3, f4.
		sc = runList(p, fileCtx("sk-bf-owner"), ascReq(ptr("f2")), 2)
		require.NotNil(t, sc)
		assert.Equal(t, []string{"f3", "f4"}, fileDataIDs(sc.Response.FileListResponse))
	})

	// P2-C: has_more must reflect a genuine next MATCH under a purpose filter, not
	// merely another owned row. A full page followed by no further match => no next
	// page (and no dangling cursor).
	t.Run("filtered has_more is false when no further purpose match exists", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, seedLedger(), nil, "deny")
		p.batchClient = &fakeFileClient{purposes: map[string]schemas.FilePurpose{
			"f5": schemas.FilePurposeBatch,    // match
			"f4": schemas.FilePurposeUserData, // non-match rows still exist BEYOND the page
			"f3": schemas.FilePurposeBatch,    // match (fills page at limit 2)
			"f2": schemas.FilePurposeUserData,
			"f1": schemas.FilePurposeUserData,
		}}
		sc := runList(p, fileCtx("sk-bf-owner"), listReq(nil, schemas.FilePurposeBatch), 2)
		require.NotNil(t, sc)
		list := sc.Response.FileListResponse
		assert.Equal(t, []string{"f5", "f3"}, fileDataIDs(list))
		assert.False(t, list.HasMore, "further owned rows exist but none MATCH, so has_more must be false")
		assert.Nil(t, list.After, "a terminal page must not hand back a continuation cursor")
	})

	t.Run("filtered has_more is true only when a further purpose match exists", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, seedLedger(), nil, "deny")
		p.batchClient = &fakeFileClient{purposes: map[string]schemas.FilePurpose{
			"f5": schemas.FilePurposeBatch,    // match
			"f4": schemas.FilePurposeBatch,    // match (fills page at limit 2)
			"f3": schemas.FilePurposeUserData, // probe skips
			"f2": schemas.FilePurposeBatch,    // probe finds a further match => has_more
			"f1": schemas.FilePurposeUserData,
		}}
		sc := runList(p, fileCtx("sk-bf-owner"), listReq(nil, schemas.FilePurposeBatch), 2)
		require.NotNil(t, sc)
		list := sc.Response.FileListResponse
		assert.Equal(t, []string{"f5", "f4"}, fileDataIDs(list))
		assert.True(t, list.HasMore)
		require.NotNil(t, list.After)
		assert.Equal(t, "f4", *list.After, "continuation cursor is the last MATCH on the page")
	})
}

func TestFilterFileListForCaller_SinglePageFallback(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "vk", true)
	foreignVK := buildVirtualKey("vk-foreign", "sk-bf-foreign", "vk-foreign", true)
	allVKs := []*configstoreTables.TableVirtualKey{ownerVK, foreignVK}

	ledger := func() *fakeFileLedger {
		l := newFakeFileLedger()
		l.rows[fileLedgerKey("openai", "mine1")] = &configstoreTables.TableManagedFile{Provider: "openai", FileID: "mine1", OwnerVirtualKeyID: "vk-owner"}
		l.rows[fileLedgerKey("openai", "mine2")] = &configstoreTables.TableManagedFile{Provider: "openai", FileID: "mine2", OwnerVirtualKeyID: "vk-owner"}
		l.rows[fileLedgerKey("openai", "theirs")] = &configstoreTables.TableManagedFile{Provider: "openai", FileID: "theirs", OwnerVirtualKeyID: "vk-foreign"}
		return l
	}

	// An upstream list page carrying owned, foreign, AND a ledger-absent id.
	upstreamPage := func() *schemas.BifrostResponse {
		return &schemas.BifrostResponse{
			FileListResponse: &schemas.BifrostFileListResponse{
				Object: "list",
				Data: []schemas.FileObject{
					{ID: "mine1"}, {ID: "theirs"}, {ID: "mine2"}, {ID: "absent"},
				},
				ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.OpenAI},
			},
		}
	}

	runFallback := func(p *GovernancePlugin, vkValue string, result *schemas.BifrostResponse) {
		ctx := fileCtx(vkValue)
		ctx.SetValue(fileListFilterInPostKey, true) // simulate the PreLLMHook fallback flag
		_, berr := p.applyFileOwnershipPostHook(ctx, result, schemas.FileListRequest, schemas.OpenAI, vkValue)
		require.Nil(t, berr)
	}

	t.Run("deny policy keeps only owned ids, drops foreign AND absent", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, ledger(), nil, "deny")
		result := upstreamPage()
		runFallback(p, "sk-bf-owner", result)
		assert.Equal(t, []string{"mine1", "mine2"}, fileDataIDs(result.FileListResponse))
	})

	t.Run("allow policy keeps owned + absent, still drops foreign", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, ledger(), nil, "allow")
		result := upstreamPage()
		runFallback(p, "sk-bf-owner", result)
		assert.Equal(t, []string{"mine1", "mine2", "absent"}, fileDataIDs(result.FileListResponse))
	})

	t.Run("filter is NOT applied without the flag (admin / relay path)", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, ledger(), nil, "deny")
		result := upstreamPage()
		ctx := fileCtx("sk-bf-owner") // no fileListFilterInPostKey set
		_, berr := p.applyFileOwnershipPostHook(ctx, result, schemas.FileListRequest, schemas.OpenAI, "sk-bf-owner")
		require.Nil(t, berr)
		assert.Equal(t, []string{"mine1", "theirs", "mine2", "absent"}, fileDataIDs(result.FileListResponse), "unflagged page must pass through untouched")
	})

	// P1-1: the filtered page must clear the raw upstream passthrough and the
	// continuation state. Otherwise the OpenAI file-list converter returns
	// RawResponse verbatim (the COMPLETE unfiltered workspace list) before ever
	// reading the filtered Data, and `after` leaks a foreign continuation id.
	t.Run("filtered page clears RawResponse + resets after/has_more", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, ledger(), nil, "deny")
		result := upstreamPage()
		result.FileListResponse.ExtraFields.RawResponse = map[string]any{"data": "UNFILTERED UPSTREAM PAGE"}
		result.FileListResponse.HasMore = true
		result.FileListResponse.After = ptr("theirs") // foreign continuation id
		runFallback(p, "sk-bf-owner", result)
		assert.Equal(t, []string{"mine1", "mine2"}, fileDataIDs(result.FileListResponse))
		assert.Nil(t, result.FileListResponse.ExtraFields.RawResponse, "raw passthrough must be cleared so the converter rebuilds from filtered Data")
		assert.False(t, result.FileListResponse.HasMore)
		assert.Nil(t, result.FileListResponse.After, "the foreign continuation id must not be echoed")
	})

	t.Run("unknown VK fails closed with no raw and no cursor", func(t *testing.T) {
		p := newFilePlugin(t, allVKs, ledger(), nil, "deny")
		result := upstreamPage()
		result.FileListResponse.ExtraFields.RawResponse = map[string]any{"data": "UNFILTERED UPSTREAM PAGE"}
		result.FileListResponse.After = ptr("theirs")
		result.FileListResponse.HasMore = true
		runFallback(p, "sk-bf-ghost", result)
		assert.Empty(t, fileDataIDs(result.FileListResponse))
		assert.Nil(t, result.FileListResponse.ExtraFields.RawResponse)
		assert.Nil(t, result.FileListResponse.After)
		assert.False(t, result.FileListResponse.HasMore)
	})
}

// TestBatchFileLinkage proves a batch owner (and only the batch owner) can reach
// its batch's input/output/error files through the file gate, via the ledger
// linkage written in the batch post-hooks.
func TestBatchFileLinkage(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "vk", true)
	foreignVK := buildVirtualKey("vk-foreign", "sk-bf-foreign", "vk-foreign", true)
	allVKs := []*configstoreTables.TableVirtualKey{ownerVK, foreignVK}

	// The batch ledger records the batch owner; the file ledger receives the links.
	batchLdg := newFakeBatchLedger()
	batchLdg.rows[ledgerKey("openai", "batch_1")] = &configstoreTables.TableManagedBatch{
		Provider: "openai", BatchID: "batch_1", OwnerVirtualKeyID: "vk-owner",
	}
	fileLdg := newFakeFileLedger()
	// The input file already owns a ledger row from its upload (captureFileOwnership).
	// Batch-create must NOT mint the input file; it only stamps provenance onto this
	// pre-existing owner-matched row.
	fileLdg.rows[fileLedgerKey("openai", "file-input")] = &configstoreTables.TableManagedFile{
		Provider: "openai", FileID: "file-input", OwnerVirtualKeyID: "vk-owner", Purpose: "batch",
	}

	p := newFilePlugin(t, allVKs, fileLdg, nil, "deny")
	p.batchLedger = batchLdg

	// Batch create carries the input file id; retrieve carries output + error.
	createResult := &schemas.BifrostResponse{
		BatchCreateResponse: &schemas.BifrostBatchCreateResponse{
			ID: "batch_1", InputFileID: "file-input",
			ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.OpenAI},
		},
	}
	_, berr := p.applyFileOwnershipPostHook(fileCtx("sk-bf-owner"), createResult, schemas.BatchCreateRequest, schemas.OpenAI, "sk-bf-owner")
	require.Nil(t, berr)

	retrieveResult := &schemas.BifrostResponse{
		BatchRetrieveResponse: &schemas.BifrostBatchRetrieveResponse{
			ID: "batch_1", OutputFileID: ptr("file-output"), ErrorFileID: ptr("file-error"),
			ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.OpenAI},
		},
	}
	_, berr = p.applyFileOwnershipPostHook(fileCtx("sk-bf-owner"), retrieveResult, schemas.BatchRetrieveRequest, schemas.OpenAI, "sk-bf-owner")
	require.Nil(t, berr)

	// All three files are now owned by the batch owner, with provenance recorded —
	// the input file via a provenance stamp on its upload row, output/error minted.
	for _, id := range []string{"file-input", "file-output", "file-error"} {
		row := fileLdg.rows[fileLedgerKey("openai", id)]
		require.NotNil(t, row, "file %s must be linked", id)
		assert.Equal(t, "vk-owner", row.OwnerVirtualKeyID)
		assert.Equal(t, "batch_1", row.SourceBatchID)
	}

	// The batch owner can fetch the output file content; a foreign caller gets 404.
	assert.Nil(t, p.enforceFileOwnershipPreHook(fileCtx("sk-bf-owner"), contentReq(schemas.OpenAI, "file-output")))
	berr = p.enforceFileOwnershipPreHook(fileCtx("sk-bf-foreign"), contentReq(schemas.OpenAI, "file-output"))
	require.NotNil(t, berr)
	assert.Equal(t, 404, *berr.StatusCode)

	// First-owner: re-creating a batch that references an already-owned input file
	// only re-stamps provenance on the existing row; it never re-assigns ownership.
	stealResult := &schemas.BifrostResponse{
		BatchCreateResponse: &schemas.BifrostBatchCreateResponse{
			ID: "batch_1", InputFileID: "file-input",
			ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.OpenAI},
		},
	}
	_, _ = p.applyFileOwnershipPostHook(fileCtx("sk-bf-foreign"), stealResult, schemas.BatchCreateRequest, schemas.OpenAI, "sk-bf-foreign")
	assert.Equal(t, "vk-owner", fileLdg.rows[fileLedgerKey("openai", "file-input")].OwnerVirtualKeyID)
}

// TestBatchCreateNeverMintsUnownedInputFile is the P1-2 regression: because all
// tenants share upstream credentials, an attacker can create a batch that names an
// input file id it does not own. Batch-create must NEVER mint ownership of that
// file — an untracked id must stay untracked (404 to everyone), and a tracked
// victim file must keep its original owner.
func TestBatchCreateNeverMintsUnownedInputFile(t *testing.T) {
	attackerVK := buildVirtualKey("vk-attacker", "sk-bf-attacker", "vk-attacker", true)
	victimVK := buildVirtualKey("vk-victim", "sk-bf-victim", "vk-victim", true)
	allVKs := []*configstoreTables.TableVirtualKey{attackerVK, victimVK}

	// The attacker's batch is owned by the attacker in the batch ledger.
	batchLdg := newFakeBatchLedger()
	batchLdg.rows[ledgerKey("openai", "batch_evil")] = &configstoreTables.TableManagedBatch{
		Provider: "openai", BatchID: "batch_evil", OwnerVirtualKeyID: "vk-attacker",
	}

	// A tracked victim file (owned by the victim) and an untracked id the attacker
	// will both try to claim via a batch-create input reference.
	fileLdg := newFakeFileLedger()
	fileLdg.rows[fileLedgerKey("openai", "victim-tracked")] = &configstoreTables.TableManagedFile{
		Provider: "openai", FileID: "victim-tracked", OwnerVirtualKeyID: "vk-victim",
	}

	p := newFilePlugin(t, allVKs, fileLdg, nil, "deny")
	p.batchLedger = batchLdg

	for _, inputID := range []string{"victim-tracked", "untracked-file"} {
		create := &schemas.BifrostResponse{
			BatchCreateResponse: &schemas.BifrostBatchCreateResponse{
				ID: "batch_evil", InputFileID: inputID,
				ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.OpenAI},
			},
		}
		_, berr := p.applyFileOwnershipPostHook(fileCtx("sk-bf-attacker"), create, schemas.BatchCreateRequest, schemas.OpenAI, "sk-bf-attacker")
		require.Nil(t, berr)
	}

	// The untracked id was NOT minted for the attacker: no row exists, so the gate
	// 404s it to everyone (including the attacker).
	assert.Nil(t, fileLdg.rows[fileLedgerKey("openai", "untracked-file")], "batch-create must not mint ownership of an untracked input file")
	berr := p.enforceFileOwnershipPreHook(fileCtx("sk-bf-attacker"), contentReq(schemas.OpenAI, "untracked-file"))
	require.NotNil(t, berr)
	assert.Equal(t, 404, *berr.StatusCode)

	// The tracked victim file kept its owner; the attacker still gets a 404.
	assert.Equal(t, "vk-victim", fileLdg.rows[fileLedgerKey("openai", "victim-tracked")].OwnerVirtualKeyID)
	berr = p.enforceFileOwnershipPreHook(fileCtx("sk-bf-attacker"), contentReq(schemas.OpenAI, "victim-tracked"))
	require.NotNil(t, berr)
	assert.Equal(t, 404, *berr.StatusCode)
	// The victim still reaches its own file.
	assert.Nil(t, p.enforceFileOwnershipPreHook(fileCtx("sk-bf-victim"), contentReq(schemas.OpenAI, "victim-tracked")))
}

// TestLinkBatchListFiles is the P2-B regression: an output/error file first seen
// via a batch LIST (not a retrieve) must get an owner-resolved ledger row, so the
// batch owner's own GET .../content succeeds without waiting for a later retrieve
// to self-heal the link.
func TestLinkBatchListFiles(t *testing.T) {
	ownerVK := buildVirtualKey("vk-owner", "sk-bf-owner", "vk", true)
	foreignVK := buildVirtualKey("vk-foreign", "sk-bf-foreign", "vk-foreign", true)
	allVKs := []*configstoreTables.TableVirtualKey{ownerVK, foreignVK}

	batchLdg := newFakeBatchLedger()
	batchLdg.rows[ledgerKey("openai", "batch_a")] = &configstoreTables.TableManagedBatch{
		Provider: "openai", BatchID: "batch_a", OwnerVirtualKeyID: "vk-owner",
	}
	batchLdg.rows[ledgerKey("openai", "batch_b")] = &configstoreTables.TableManagedBatch{
		Provider: "openai", BatchID: "batch_b", OwnerVirtualKeyID: "vk-owner",
	}
	fileLdg := newFakeFileLedger()

	p := newFilePlugin(t, allVKs, fileLdg, nil, "deny")
	p.batchLedger = batchLdg

	listResult := &schemas.BifrostResponse{
		BatchListResponse: &schemas.BifrostBatchListResponse{
			Object: "list",
			Data: []schemas.BifrostBatchRetrieveResponse{
				{ID: "batch_a", OutputFileID: ptr("out-a"), ErrorFileID: ptr("err-a")},
				{ID: "batch_b", OutputFileID: ptr("out-b")},
			},
			ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.OpenAI},
		},
	}
	_, berr := p.applyFileOwnershipPostHook(fileCtx("sk-bf-owner"), listResult, schemas.BatchListRequest, schemas.OpenAI, "sk-bf-owner")
	require.Nil(t, berr)

	for _, id := range []string{"out-a", "err-a", "out-b"} {
		row := fileLdg.rows[fileLedgerKey("openai", id)]
		require.NotNil(t, row, "listed batch output/error file %s must be linked", id)
		assert.Equal(t, "vk-owner", row.OwnerVirtualKeyID)
		assert.Equal(t, "batch_output", row.Purpose)
	}
	// The batch owner can now fetch the output content seen via list; foreign 404s.
	assert.Nil(t, p.enforceFileOwnershipPreHook(fileCtx("sk-bf-owner"), contentReq(schemas.OpenAI, "out-a")))
	berr = p.enforceFileOwnershipPreHook(fileCtx("sk-bf-foreign"), contentReq(schemas.OpenAI, "out-a"))
	require.NotNil(t, berr)
	assert.Equal(t, 404, *berr.StatusCode)
}

// fileDataIDs extracts the ordered file ids from a built list page.
func fileDataIDs(list *schemas.BifrostFileListResponse) []string {
	ids := make([]string, 0, len(list.Data))
	for _, f := range list.Data {
		ids = append(ids, f.ID)
	}
	return ids
}
