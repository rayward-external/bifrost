package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const reasoningMappingProviderName = schemas.ModelProvider("custom-openai-compatible")

// glmStyleReasoningEffortMapping mirrors the shape an operator declares for an
// OpenAI-compatible upstream that exposes a `thinking` object instead of
// `reasoning_effort`.
func glmStyleReasoningEffortMapping() *schemas.ReasoningEffortMapping {
	return &schemas.ReasoningEffortMapping{
		Field:    "thinking",
		Enabled:  map[string]any{"type": "enabled"},
		Disabled: map[string]any{"type": "disabled"},
	}
}

func newReasoningMappingProvider(baseURL string, mapping *schemas.ReasoningEffortMapping) *OpenAIProvider {
	return NewOpenAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: baseURL},
		CustomProviderConfig: &schemas.CustomProviderConfig{
			CustomProviderKey:      string(reasoningMappingProviderName),
			BaseProviderType:       schemas.OpenAI,
			ReasoningEffortMapping: mapping,
		},
	}, testNoopLogger{})
}

func newReasoningMappingRequest(effort *string) *schemas.BifrostChatRequest {
	params := &schemas.ChatParameters{}
	if effort != nil {
		params.Reasoning = &schemas.ChatReasoning{Effort: effort}
	}
	return &schemas.BifrostChatRequest{
		Provider: reasoningMappingProviderName,
		Model:    "zai-org/GLM-5.2",
		Input: []schemas.ChatMessage{{
			Role:    schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hello")},
		}},
		Params: params,
	}
}

// newCapturingChatServer serves a single canned chat completion and records the
// outbound request body it was sent.
func newCapturingChatServer(t *testing.T, captured *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, captured))

		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write([]byte(`{
			"id": "chatcmpl-test",
			"object": "chat.completion",
			"created": 1234567890,
			"model": "zai-org/GLM-5.2",
			"choices": [{
				"index": 0,
				"message": {"role": "assistant", "content": "hi"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}
		}`))
		require.NoError(t, err)
	}))
}

// newCapturingChatStreamServer serves a minimal SSE stream and records the
// outbound request body it was sent.
func newCapturingChatStreamServer(t *testing.T, captured *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, captured))

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)

		_, err = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"zai-org/GLM-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n"))
		require.NoError(t, err)
		flusher.Flush()

		_, err = w.Write([]byte("data: [DONE]\n\n"))
		require.NoError(t, err)
		flusher.Flush()
	}))
}

func drainStream(t *testing.T, stream chan *schemas.BifrostStreamChunk) {
	t.Helper()
	for chunk := range stream {
		if chunk.BifrostError != nil {
			t.Fatalf("unexpected stream error: %s", chunk.BifrostError.Error.Message)
		}
	}
}

// TestChatCompletion_ReasoningEffortMappedOntoProviderField asserts the wire
// body for every inbound reasoning_effort case. The upstream this models
// ignores reasoning_effort on one model and 400s on unsupported levels on
// another, so the raw field must never be emitted once a mapping exists.
func TestChatCompletion_ReasoningEffortMappedOntoProviderField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		effort         *string
		mapping        *schemas.ReasoningEffortMapping
		expectThinking any
	}{
		{
			name:           "high enables thinking",
			effort:         schemas.Ptr("high"),
			mapping:        glmStyleReasoningEffortMapping(),
			expectThinking: map[string]any{"type": "enabled"},
		},
		{
			// "low" is rejected outright by the upstream; it must still reach
			// the wire as the mapped enabled value, never as reasoning_effort.
			name:           "low enables thinking without leaking an invalid level",
			effort:         schemas.Ptr("low"),
			mapping:        glmStyleReasoningEffortMapping(),
			expectThinking: map[string]any{"type": "enabled"},
		},
		{
			// Only the disabled set means "off"; everything else enables.
			name:           "minimal enables thinking by default",
			effort:         schemas.Ptr("minimal"),
			mapping:        glmStyleReasoningEffortMapping(),
			expectThinking: map[string]any{"type": "enabled"},
		},
		{
			name:           "none disables thinking",
			effort:         schemas.Ptr("none"),
			mapping:        glmStyleReasoningEffortMapping(),
			expectThinking: map[string]any{"type": "disabled"},
		},
		{
			name:           "none is matched case-insensitively",
			effort:         schemas.Ptr("NONE"),
			mapping:        glmStyleReasoningEffortMapping(),
			expectThinking: map[string]any{"type": "disabled"},
		},
		{
			name:           "no reasoning params leaves the body untouched",
			effort:         nil,
			mapping:        glmStyleReasoningEffortMapping(),
			expectThinking: nil,
		},
		{
			name:   "off case with no disabled value omits the field entirely",
			effort: schemas.Ptr("none"),
			mapping: &schemas.ReasoningEffortMapping{
				Field:   "thinking",
				Enabled: map[string]any{"type": "enabled"},
			},
			expectThinking: nil,
		},
		{
			name:   "custom disabled_values list overrides the default",
			effort: schemas.Ptr("minimal"),
			mapping: &schemas.ReasoningEffortMapping{
				Field:          "thinking",
				Enabled:        map[string]any{"type": "enabled"},
				Disabled:       map[string]any{"type": "disabled"},
				DisabledValues: []string{"none", "minimal"},
			},
			expectThinking: map[string]any{"type": "disabled"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var captured map[string]any
			server := newCapturingChatServer(t, &captured)
			defer server.Close()

			provider := newReasoningMappingProvider(server.URL, tt.mapping)
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

			_, bifrostErr := provider.ChatCompletion(ctx, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, newReasoningMappingRequest(tt.effort))
			require.Nil(t, bifrostErr)
			require.NotNil(t, captured)

			assert.NotContains(t, captured, "reasoning_effort", "reasoning_effort must never reach an upstream that declares a mapping")
			assert.NotContains(t, captured, "reasoning")
			if tt.expectThinking == nil {
				assert.NotContains(t, captured, "thinking")
			} else {
				assert.Equal(t, tt.expectThinking, captured["thinking"])
			}
		})
	}
}

// TestChatCompletion_ReasoningEffortMappingStripsEffortDerivedFromMaxTokens is
// the sharp edge of requirement "reasoning_effort never reaches the wire":
// OpenAIChatRequest.resolveReasoningEffort re-derives an effort from
// reasoning.max_tokens whenever effort is nil, so clearing effort alone would
// silently put reasoning_effort back on the body.
func TestChatCompletion_ReasoningEffortMappingStripsEffortDerivedFromMaxTokens(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := newCapturingChatServer(t, &captured)
	defer server.Close()

	provider := newReasoningMappingProvider(server.URL, glmStyleReasoningEffortMapping())
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	request := newReasoningMappingRequest(schemas.Ptr("high"))
	request.Params.Reasoning.MaxTokens = schemas.Ptr(4096)
	request.Params.MaxCompletionTokens = schemas.Ptr(8192)

	_, bifrostErr := provider.ChatCompletion(ctx, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, request)
	require.Nil(t, bifrostErr)
	require.NotNil(t, captured)

	assert.NotContains(t, captured, "reasoning_effort")
	assert.NotContains(t, captured, "reasoning")
	assert.Equal(t, map[string]any{"type": "enabled"}, captured["thinking"])
}

// TestChatCompletion_ReasoningEffortMappingAppliesWhenCustomProviderFlagIsSet
// pins the mapping to the other side of the ToOpenAIChatRequest branch. Bifrost
// currently derives BifrostContextKeyIsCustomProvider from the *base* provider
// type, so an openai-based custom provider takes the filtered branch; the
// mapping must hold either way.
func TestChatCompletion_ReasoningEffortMappingAppliesWhenCustomProviderFlagIsSet(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := newCapturingChatServer(t, &captured)
	defer server.Close()

	provider := newReasoningMappingProvider(server.URL, glmStyleReasoningEffortMapping())
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyIsCustomProvider, true)

	_, bifrostErr := provider.ChatCompletion(ctx, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, newReasoningMappingRequest(schemas.Ptr("low")))
	require.Nil(t, bifrostErr)
	require.NotNil(t, captured)

	assert.NotContains(t, captured, "reasoning_effort")
	assert.Equal(t, map[string]any{"type": "enabled"}, captured["thinking"])
}

// TestChatCompletionStream_ReasoningEffortMappedOntoProviderField covers the
// streaming path, which builds its body through a separate call site.
func TestChatCompletionStream_ReasoningEffortMappedOntoProviderField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		effort         *string
		expectThinking any
	}{
		{"high enables thinking", schemas.Ptr("high"), map[string]any{"type": "enabled"}},
		{"low enables thinking", schemas.Ptr("low"), map[string]any{"type": "enabled"}},
		{"none disables thinking", schemas.Ptr("none"), map[string]any{"type": "disabled"}},
		{"no reasoning params leaves the body untouched", nil, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var captured map[string]any
			server := newCapturingChatStreamServer(t, &captured)
			defer server.Close()

			provider := newReasoningMappingProvider(server.URL, glmStyleReasoningEffortMapping())
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

			postHookRunner := func(_ *schemas.BifrostContext, response *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
				return response, err
			}

			stream, bifrostErr := provider.ChatCompletionStream(ctx, postHookRunner, nil, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, newReasoningMappingRequest(tt.effort))
			require.Nil(t, bifrostErr)
			drainStream(t, stream)

			require.NotNil(t, captured)
			assert.NotContains(t, captured, "reasoning_effort")
			if tt.expectThinking == nil {
				assert.NotContains(t, captured, "thinking")
			} else {
				assert.Equal(t, tt.expectThinking, captured["thinking"])
			}
		})
	}
}

// TestChatCompletion_NoMappingForwardsReasoningEffortVerbatim is the regression
// guard: a custom provider without a mapping must behave exactly as before, on
// both sides of the ToOpenAIChatRequest custom-provider branch.
func TestChatCompletion_NoMappingForwardsReasoningEffortVerbatim(t *testing.T) {
	t.Parallel()

	for _, isCustomProvider := range []bool{false, true} {
		t.Run(fmt.Sprintf("is_custom_provider=%t", isCustomProvider), func(t *testing.T) {
			t.Parallel()

			var captured map[string]any
			server := newCapturingChatServer(t, &captured)
			defer server.Close()

			provider := newReasoningMappingProvider(server.URL, nil)
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			ctx.SetValue(schemas.BifrostContextKeyIsCustomProvider, isCustomProvider)

			_, bifrostErr := provider.ChatCompletion(ctx, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, newReasoningMappingRequest(schemas.Ptr("low")))
			require.Nil(t, bifrostErr)
			require.NotNil(t, captured)

			assert.Equal(t, "low", captured["reasoning_effort"])
			assert.NotContains(t, captured, "thinking")
		})
	}
}

// TestChatCompletionStream_NoMappingForwardsReasoningEffortVerbatim is the
// streaming half of the regression guard.
func TestChatCompletionStream_NoMappingForwardsReasoningEffortVerbatim(t *testing.T) {
	t.Parallel()

	for _, isCustomProvider := range []bool{false, true} {
		t.Run(fmt.Sprintf("is_custom_provider=%t", isCustomProvider), func(t *testing.T) {
			t.Parallel()

			var captured map[string]any
			server := newCapturingChatStreamServer(t, &captured)
			defer server.Close()

			provider := newReasoningMappingProvider(server.URL, nil)
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			ctx.SetValue(schemas.BifrostContextKeyIsCustomProvider, isCustomProvider)

			postHookRunner := func(_ *schemas.BifrostContext, response *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
				return response, err
			}

			stream, bifrostErr := provider.ChatCompletionStream(ctx, postHookRunner, nil, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, newReasoningMappingRequest(schemas.Ptr("low")))
			require.Nil(t, bifrostErr)
			drainStream(t, stream)

			require.NotNil(t, captured)
			assert.Equal(t, "low", captured["reasoning_effort"])
			assert.NotContains(t, captured, "thinking")
		})
	}
}

// TestChatCompletion_NoMappingLeavesExtraParamsPassthroughOff guards the other
// half of "unchanged when no mapping": the provider must not switch on
// ExtraParams passthrough for providers that never opted into it.
func TestChatCompletion_NoMappingLeavesExtraParamsPassthroughOff(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := newCapturingChatServer(t, &captured)
	defer server.Close()

	provider := newReasoningMappingProvider(server.URL, nil)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	request := newReasoningMappingRequest(schemas.Ptr("high"))
	request.Params.ExtraParams = map[string]interface{}{"custom_knob": "value"}

	_, bifrostErr := provider.ChatCompletion(ctx, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, request)
	require.Nil(t, bifrostErr)
	require.NotNil(t, captured)

	assert.NotContains(t, captured, "custom_knob")
	assert.Nil(t, ctx.Value(schemas.BifrostContextKeyPassthroughExtraParams))
}

// TestChatCompletion_ReasoningEffortMappingDoesNotMutateCallerRequest keeps a
// fallback or retry to another provider seeing the caller's original
// reasoning_effort and extra params.
func TestChatCompletion_ReasoningEffortMappingDoesNotMutateCallerRequest(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := newCapturingChatServer(t, &captured)
	defer server.Close()

	provider := newReasoningMappingProvider(server.URL, glmStyleReasoningEffortMapping())
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	request := newReasoningMappingRequest(schemas.Ptr("high"))
	callerExtraParams := map[string]interface{}{"custom_knob": "value"}
	request.Params.ExtraParams = callerExtraParams

	_, bifrostErr := provider.ChatCompletion(ctx, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, request)
	require.Nil(t, bifrostErr)

	require.NotNil(t, request.Params.Reasoning)
	require.NotNil(t, request.Params.Reasoning.Effort)
	assert.Equal(t, "high", *request.Params.Reasoning.Effort, "caller request must keep its reasoning_effort")
	assert.Equal(t, map[string]interface{}{"custom_knob": "value"}, callerExtraParams, "caller ExtraParams map must not gain the mapped field")

	// The caller's own extra params still ride along on the wire.
	assert.Equal(t, "value", captured["custom_knob"])
	assert.Equal(t, map[string]any{"type": "enabled"}, captured["thinking"])
}

func TestReasoningEffortMapping_Resolve(t *testing.T) {
	t.Parallel()

	mapping := glmStyleReasoningEffortMapping()

	t.Run("nil mapping is inert", func(t *testing.T) {
		t.Parallel()
		var nilMapping *schemas.ReasoningEffortMapping
		assert.False(t, nilMapping.IsConfigured())
		_, ok := nilMapping.Resolve("high")
		assert.False(t, ok)
	})

	t.Run("empty field is inert", func(t *testing.T) {
		t.Parallel()
		empty := &schemas.ReasoningEffortMapping{Enabled: map[string]any{"type": "enabled"}}
		assert.False(t, empty.IsConfigured())
		_, ok := empty.Resolve("high")
		assert.False(t, ok)
	})

	t.Run("empty effort writes nothing", func(t *testing.T) {
		t.Parallel()
		_, ok := mapping.Resolve("   ")
		assert.False(t, ok)
	})

	t.Run("returned value is a copy", func(t *testing.T) {
		t.Parallel()
		value, ok := mapping.Resolve("high")
		require.True(t, ok)
		value["type"] = "tampered"
		assert.Equal(t, map[string]any{"type": "enabled"}, mapping.Enabled)
	})
}
