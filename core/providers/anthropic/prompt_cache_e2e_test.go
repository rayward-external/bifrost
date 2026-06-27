package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

type promptCacheE2ELogger struct{}

func (promptCacheE2ELogger) Debug(string, ...any)                   {}
func (promptCacheE2ELogger) Info(string, ...any)                    {}
func (promptCacheE2ELogger) Warn(string, ...any)                    {}
func (promptCacheE2ELogger) Error(string, ...any)                   {}
func (promptCacheE2ELogger) Fatal(string, ...any)                   {}
func (promptCacheE2ELogger) SetLevel(schemas.LogLevel)              {}
func (promptCacheE2ELogger) SetOutputType(schemas.LoggerOutputType) {}
func (promptCacheE2ELogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

func TestAnthropicProviderChatCompletionE2EPromptCacheControl(t *testing.T) {
	t.Setenv(anthropicPromptCacheTTLEnv, "auto")

	var capturedBody []byte
	var capturedHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("upstream path = %q, want /v1/messages", r.URL.Path)
		}
		capturedHeaders = r.Header.Clone()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		capturedBody = body

		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg_cache_e2e",
			"type":"message",
			"role":"assistant",
			"model":"claude-sonnet-4-20250514",
			"content":[{"type":"text","text":"ok"}],
			"stop_reason":"end_turn",
			"usage":{
				"input_tokens":11,
				"cache_creation_input_tokens":64,
				"cache_read_input_tokens":32,
				"cache_creation":{"ephemeral_1h_input_tokens":64},
				"output_tokens":7
			}
		}`))
	}))
	defer upstream.Close()

	provider := NewAnthropicProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: upstream.URL},
	}, promptCacheE2ELogger{})

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{
		anthropicPromptCacheWorkloadHeader: "benchmark",
	})
	content := "stable evaluation prefix"
	resp, bifrostErr := provider.ChatCompletion(ctx, schemas.Key{
		Value: schemas.SecretVar{Val: "test-key"},
	}, &schemas.BifrostChatRequest{
		Provider: schemas.Anthropic,
		Model:    "claude-sonnet-4-20250514",
		Input: []schemas.ChatMessage{{
			Role: schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{
				ContentStr: &content,
			},
		}},
	})
	if bifrostErr != nil {
		t.Fatalf("ChatCompletion returned error: %v", bifrostErr)
	}

	var upstreamBody struct {
		CacheControl struct {
			Type string `json:"type"`
			TTL  string `json:"ttl"`
		} `json:"cache_control"`
	}
	if err := json.Unmarshal(capturedBody, &upstreamBody); err != nil {
		t.Fatalf("unmarshal upstream body %s: %v", string(capturedBody), err)
	}
	if upstreamBody.CacheControl.Type != "ephemeral" || upstreamBody.CacheControl.TTL != "1h" {
		t.Fatalf("cache_control = %#v, want ephemeral 1h", upstreamBody.CacheControl)
	}
	if capturedHeaders.Get(anthropicPromptCacheTTLHeader) != "" {
		t.Fatalf("%s was forwarded upstream", anthropicPromptCacheTTLHeader)
	}
	if capturedHeaders.Get(anthropicPromptCacheWorkloadHeader) != "" {
		t.Fatalf("%s was forwarded upstream", anthropicPromptCacheWorkloadHeader)
	}

	if resp == nil || resp.Usage == nil || resp.Usage.PromptTokensDetails == nil {
		t.Fatalf("missing usage in response: %#v", resp)
	}
	if resp.Usage.PromptTokensDetails.CachedReadTokens != 32 {
		t.Fatalf("cached read tokens = %d, want 32", resp.Usage.PromptTokensDetails.CachedReadTokens)
	}
	if resp.Usage.PromptTokensDetails.CachedWriteTokens != 64 {
		t.Fatalf("cached write tokens = %d, want 64", resp.Usage.PromptTokensDetails.CachedWriteTokens)
	}
	if resp.Usage.PromptTokensDetails.CachedWriteTokenDetails == nil ||
		resp.Usage.PromptTokensDetails.CachedWriteTokenDetails.CachedWriteTokens1h != 64 {
		t.Fatalf("cached write token details = %#v, want 1h write tokens", resp.Usage.PromptTokensDetails.CachedWriteTokenDetails)
	}
}
