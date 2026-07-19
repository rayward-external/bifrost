package schemas

import "testing"

// Routing plugins (governance rules, virtual-key LB, the model-catalog
// resolver) write their provider pick back via SetProvider. The batch-create
// and file-upload union cases were missing, so for those request types every
// plugin resolution was a silent no-op and bare-model requests failed
// post-pipeline validation despite a successful pick.
func TestSetProviderCoversBatchAndFileRequests(t *testing.T) {
	batch := &BifrostRequest{BatchCreateRequest: &BifrostBatchCreateRequest{}}
	batch.SetProvider(Azure)
	if batch.BatchCreateRequest.Provider != Azure {
		t.Errorf("BatchCreateRequest.Provider = %q, want %q", batch.BatchCreateRequest.Provider, Azure)
	}
	if p, _, _ := batch.GetRequestFields(); p != Azure {
		t.Errorf("GetRequestFields provider = %q after SetProvider, want %q", p, Azure)
	}

	file := &BifrostRequest{FileUploadRequest: &BifrostFileUploadRequest{}}
	file.SetProvider(Vertex)
	if file.FileUploadRequest.Provider != Vertex {
		t.Errorf("FileUploadRequest.Provider = %q, want %q", file.FileUploadRequest.Provider, Vertex)
	}
}

func TestSetModelCoversFileUploadRequest(t *testing.T) {
	m := "gpt-5.4-batch"
	file := &BifrostRequest{FileUploadRequest: &BifrostFileUploadRequest{Model: &m}}
	file.SetModel("gpt-5.4")
	if file.FileUploadRequest.Model == nil || *file.FileUploadRequest.Model != "gpt-5.4" {
		t.Errorf("FileUploadRequest.Model = %v, want gpt-5.4", file.FileUploadRequest.Model)
	}
}
