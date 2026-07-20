package handlers

import (
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// TestResolveLifecycleProvider covers provider resolution for the batch/file
// lifecycle endpoints (list/retrieve/cancel/results/content), which carry an id
// but no model: explicit signal first, then id shape, then the sole configured
// OpenAI-dialect provider.
func TestResolveLifecycleProvider(t *testing.T) {
	azureOnly := map[schemas.ModelProvider]configstore.ProviderConfig{
		schemas.Azure:     {},
		schemas.Anthropic: {},
		schemas.Bedrock:   {},
	}
	bothOpenAIDialects := map[schemas.ModelProvider]configstore.ProviderConfig{
		schemas.Azure:  {},
		schemas.OpenAI: {},
	}

	cases := []struct {
		name         string
		providers    map[schemas.ModelProvider]configstore.ProviderConfig
		id           string
		query        string // ?provider=; empty = unset
		header       string // x-model-provider; empty = unset
		wantProvider schemas.ModelProvider
		wantErrMsg   string // non-empty = error expected, substring match
	}{
		{
			name:         "explicit query param wins",
			providers:    azureOnly,
			id:           "msgbatch_abc",
			query:        "openai",
			wantProvider: schemas.OpenAI,
		},
		{
			name:         "explicit header wins over id shape",
			providers:    azureOnly,
			id:           "arn:aws:bedrock:us-east-1:123:job/abc",
			header:       "azure",
			wantProvider: schemas.Azure,
		},
		{
			name:         "msgbatch_ id infers anthropic",
			providers:    azureOnly,
			id:           "msgbatch_01ABC",
			wantProvider: schemas.Anthropic,
		},
		{
			name:         "arn: id infers bedrock",
			providers:    azureOnly,
			id:           "arn:aws:bedrock:us-east-1:123:model-invocation-job/abc",
			wantProvider: schemas.Bedrock,
		},
		{
			name:         "s3:// id infers bedrock",
			providers:    azureOnly,
			id:           "s3://bucket/prefix/file.jsonl",
			wantProvider: schemas.Bedrock,
		},
		{
			name:         "gs:// id infers vertex",
			providers:    azureOnly,
			id:           "gs://bucket/file.jsonl",
			wantProvider: schemas.Vertex,
		},
		{
			name:         "ambiguous batch_ id, single OpenAI-dialect provider",
			providers:    azureOnly,
			id:           "batch_537ba5d0",
			wantProvider: schemas.Azure,
		},
		{
			name:         "no id (list), single OpenAI-dialect provider",
			providers:    azureOnly,
			id:           "",
			wantProvider: schemas.Azure,
		},
		{
			name:       "ambiguous id, two OpenAI-dialect providers → error",
			providers:  bothOpenAIDialects,
			id:         "batch_537ba5d0",
			wantErrMsg: "provider is ambiguous",
		},
		{
			name:       "no providers configured → error",
			providers:  nil,
			id:         "file-abc123",
			wantErrMsg: "provider is ambiguous",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &CompletionHandler{config: &lib.Config{Providers: tc.providers}}

			ctx := &fasthttp.RequestCtx{}
			if tc.query != "" {
				ctx.QueryArgs().Set("provider", tc.query)
			}
			if tc.header != "" {
				ctx.Request.Header.Set("x-model-provider", tc.header)
			}

			provider, err := h.resolveLifecycleProvider(ctx, tc.id)
			if tc.wantErrMsg != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got provider %q", tc.wantErrMsg, provider)
				}
				if !strings.Contains(err.Error(), tc.wantErrMsg) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if provider != tc.wantProvider {
				t.Fatalf("provider = %q, want %q", provider, tc.wantProvider)
			}
		})
	}
}
