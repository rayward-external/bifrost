package anthropic

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func TestApplyAnthropicPromptCacheControlInjectsTopLevelOneHour(t *testing.T) {
	t.Setenv(anthropicPromptCacheTTLEnv, "1h")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	req := &AnthropicMessageRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{{
			Role: "user",
			Content: AnthropicContent{
				ContentStr: schemas.Ptr("hello"),
			},
		}},
	}

	changed := ApplyAnthropicPromptCacheControl(ctx, req)

	if !changed {
		t.Fatal("expected cache_control to be injected")
	}
	if req.CacheControl == nil {
		t.Fatal("expected top-level cache_control")
	}
	if req.CacheControl.Type != schemas.CacheControlTypeEphemeral {
		t.Fatalf("cache_control type = %q, want ephemeral", req.CacheControl.Type)
	}
	if req.CacheControl.TTL == nil || *req.CacheControl.TTL != "1h" {
		t.Fatalf("cache_control ttl = %v, want 1h", req.CacheControl.TTL)
	}
}

func TestApplyAnthropicPromptCacheControlPreservesClientControl(t *testing.T) {
	t.Setenv(anthropicPromptCacheTTLEnv, "1h")
	ttl := "5m"
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	req := &AnthropicMessageRequest{
		Model:        "claude-sonnet-4-20250514",
		MaxTokens:    1024,
		CacheControl: &schemas.CacheControl{Type: schemas.CacheControlTypeEphemeral, TTL: &ttl},
		Messages: []AnthropicMessage{{
			Role: "user",
			Content: AnthropicContent{
				ContentStr: schemas.Ptr("hello"),
			},
		}},
	}

	changed := ApplyAnthropicPromptCacheControl(ctx, req)

	if changed {
		t.Fatal("expected existing cache_control to be preserved")
	}
	if req.CacheControl == nil || req.CacheControl.TTL == nil || *req.CacheControl.TTL != "5m" {
		t.Fatalf("cache_control = %#v, want client 5m value", req.CacheControl)
	}
}

func TestApplyAnthropicPromptCacheControlHeaderCanDisableEnvDefault(t *testing.T) {
	t.Setenv(anthropicPromptCacheTTLEnv, "1h")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{
		anthropicPromptCacheTTLHeader: "off",
	})
	req := &AnthropicMessageRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{{
			Role: "user",
			Content: AnthropicContent{
				ContentStr: schemas.Ptr("hello"),
			},
		}},
	}

	changed := ApplyAnthropicPromptCacheControl(ctx, req)

	if changed {
		t.Fatal("expected header off override to disable injection")
	}
	if req.CacheControl != nil {
		t.Fatalf("cache_control = %#v, want nil", req.CacheControl)
	}
}

func TestApplyAnthropicPromptCacheControlAutoUsesWorkloadHeader(t *testing.T) {
	t.Setenv(anthropicPromptCacheTTLEnv, "auto")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{
		anthropicPromptCacheWorkloadHeader: "benchmark",
	})
	req := &AnthropicMessageRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{{
			Role: "user",
			Content: AnthropicContent{
				ContentStr: schemas.Ptr("hello"),
			},
		}},
	}

	changed := ApplyAnthropicPromptCacheControl(ctx, req)

	if !changed {
		t.Fatal("expected cache_control to be injected")
	}
	if req.CacheControl == nil || req.CacheControl.TTL == nil || *req.CacheControl.TTL != "1h" {
		t.Fatalf("cache_control = %#v, want 1h for benchmark workload", req.CacheControl)
	}
}

func TestApplyAnthropicPromptCacheControlToRawBody(t *testing.T) {
	t.Setenv(anthropicPromptCacheTTLEnv, "1h")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	body := []byte(`{"model":"claude-sonnet-4-20250514","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`)

	out, changed, err := ApplyAnthropicPromptCacheControlToRawBody(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatal("expected raw body to be changed")
	}
	var got struct {
		CacheControl struct {
			Type string `json:"type"`
			TTL  string `json:"ttl"`
		} `json:"cache_control"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal raw body: %v", err)
	}
	if got.CacheControl.Type != "ephemeral" || got.CacheControl.TTL != "1h" {
		t.Fatalf("cache_control = %#v, want ephemeral 1h", got.CacheControl)
	}
}

func TestApplyAnthropicPromptCacheControlToRawBodyPreservesNestedClientControl(t *testing.T) {
	t.Setenv(anthropicPromptCacheTTLEnv, "1h")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	body := []byte(`{"model":"claude-sonnet-4-20250514","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}]}`)

	out, changed, err := ApplyAnthropicPromptCacheControlToRawBody(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Fatal("expected nested client cache_control to prevent gateway injection")
	}
	if string(out) != string(body) {
		t.Fatalf("body changed unexpectedly: %s", string(out))
	}
}
