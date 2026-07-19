package handlers

import (
	"strings"
	"testing"

	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// TestResolveBatchProvider covers the three resolution paths introduced to make
// model optional on POST /v1/batches (OpenAI spec: model lives in the JSONL body).
func TestResolveBatchProvider(t *testing.T) {
	config := &lib.Config{}

	cases := []struct {
		name         string
		model        string
		header       string // x-model-provider; empty = unset
		query        string // ?provider=; empty = unset
		wantProvider string
		wantModel    string
		wantErrMsg   string // non-empty = error expected, substring match
	}{
		{
			name:         "model field: provider+model parsed",
			model:        "openai/gpt-4o-mini",
			wantProvider: "openai",
			wantModel:    "gpt-4o-mini",
		},
		{
			name:         "no model, x-model-provider header",
			header:       "openai",
			wantProvider: "openai",
			wantModel:    "",
		},
		{
			name:         "no model, ?provider= query param",
			query:        "anthropic",
			wantProvider: "anthropic",
			wantModel:    "",
		},
		{
			name:       "no model, no provider → error",
			wantErrMsg: "provider query parameter or x-model-provider header is required",
		},
		// Bare model names (no provider prefix) must consult the explicit
		// ?provider=/x-model-provider signal instead of silently resolving to
		// an empty provider — previously the model != "" branch short-circuited
		// before the query/header check.
		{
			name:         "bare model + ?provider= query param",
			model:        "gpt-5.4-batch",
			query:        "azure",
			wantProvider: "azure",
			wantModel:    "gpt-5.4-batch",
		},
		{
			name:         "bare model + x-model-provider header",
			model:        "gpt-5.4-batch",
			header:       "azure",
			wantProvider: "azure",
			wantModel:    "gpt-5.4-batch",
		},
		{
			name:         "prefixed model wins over query param",
			model:        "openai/gpt-4o-mini",
			query:        "azure",
			wantProvider: "openai",
			wantModel:    "gpt-4o-mini",
		},
		{
			name:         "bare model, no signal → empty provider (routing pipeline resolves)",
			model:        "gpt-5.4-batch",
			wantProvider: "",
			wantModel:    "gpt-5.4-batch",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &fasthttp.RequestCtx{}
			if tc.header != "" {
				ctx.Request.Header.Set("x-model-provider", tc.header)
			}
			if tc.query != "" {
				ctx.QueryArgs().Set("provider", tc.query)
			}

			provider, modelName, err := resolveBatchProvider(ctx, config, tc.model)

			if tc.wantErrMsg != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrMsg)
				}
				if !strings.Contains(err.Error(), tc.wantErrMsg) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(provider) != tc.wantProvider {
				t.Fatalf("provider = %q, want %q", provider, tc.wantProvider)
			}
			if modelName != tc.wantModel {
				t.Fatalf("modelName = %q, want %q", modelName, tc.wantModel)
			}
		})
	}
}
