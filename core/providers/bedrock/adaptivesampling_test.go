package bedrock

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// bedrockSamplingParams is the request every case in this file sends: both
// sampling knobs set, plus a stop sequence so we can prove the suppression is
// scoped to temperature/topP and does not collaterally drop other fields.
func bedrockSamplingParams() *schemas.ChatParameters {
	return &schemas.ChatParameters{
		MaxCompletionTokens: schemas.Ptr(4096),
		Temperature:         schemas.Ptr(0.7),
		TopP:                schemas.Ptr(0.9),
		Stop:                []string{"STOP"},
	}
}

// TestConvertInferenceConfig_AdaptiveOnlyClaudeDropsSamplingParams is the
// Bedrock half of #351.
//
// Bedrock Converse forwards inferenceConfig straight through to the underlying
// Anthropic Messages request, so a Bedrock-served claude-opus-5 returned the
// same 400 the direct Anthropic provider did:
//
//	"`temperature` is deprecated for this model"
//
// Fixing only core/providers/anthropic left this path broken. The suppression
// must key off the SAME shared version predicate, and it must survive the id
// shapes Bedrock actually hands us: bare ids, us./eu./global. inference-profile
// prefixes, date + "-v1:0" suffixes, and inference-profile ARNs.
func TestConvertInferenceConfig_AdaptiveOnlyClaudeDropsSamplingParams(t *testing.T) {
	cases := []struct {
		name        string
		model       string
		wantDropped bool
	}{
		// --- adaptive-only Claude: temperature/topP MUST be dropped ---------
		{"opus5 bare", "claude-opus-5", true},
		{"opus5 bedrock vendor prefix", "anthropic.claude-opus-5", true},
		{"opus5 us inference profile", "us.anthropic.claude-opus-5", true},
		{"opus5 us profile dated", "us.anthropic.claude-opus-5-20260901-v1:0", true},
		{"opus5 eu inference profile", "eu.anthropic.claude-opus-5-20260901-v1:0", true},
		{"opus5 global inference profile", "global.anthropic.claude-opus-5", true},
		{
			"opus5 inference-profile ARN",
			"arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-opus-5-20260901-v1:0",
			true,
		},
		{"opus4.8", "us.anthropic.claude-opus-4-8-20260601-v1:0", true},
		{"opus4.7", "anthropic.claude-opus-4-7", true},
		{"sonnet5", "global.anthropic.claude-sonnet-5", true},
		{"fable5", "anthropic.claude-fable-5", true},
		// Future majors must be covered with no further code change.
		{"opus6", "us.anthropic.claude-opus-6", true},
		{"sonnet6", "us.anthropic.claude-sonnet-6-20270301-v1:0", true},

		// --- non-adaptive Claude: temperature/topP MUST be preserved -------
		{"opus4.6 still sampled", "anthropic.claude-opus-4-6", false},
		{"opus4.5 still sampled", "us.anthropic.claude-opus-4-5", false},
		{"sonnet4.6 still sampled", "global.anthropic.claude-sonnet-4-6", false},
		{"sonnet4.5 still sampled", "anthropic.claude-sonnet-4-5-20250929-v1:0", false},
		{"haiku4.5 still sampled", "anthropic.claude-haiku-4-5-20251001-v1:0", false},
		{"claude3.5 sonnet still sampled", "anthropic.claude-3-5-sonnet-20241022-v1:0", false},

		// --- non-Claude models are untouched -------------------------------
		{"nova", "amazon.nova-pro-v1:0", false},
		{"glm", "zai.glm-5", false},
		{"llama", "meta.llama3-1-70b-instruct-v1:0", false},
		{"mistral", "mistral.mistral-large-2407-v1:0", false},
		// An opaque application-inference-profile ARN carries no model name at
		// all; it parses to the zero value, so behaviour is unchanged (params
		// pass through exactly as before this fix).
		{
			"opaque application-inference-profile ARN",
			"arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/a1b2c3d4e5f6",
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := convertInferenceConfig(bedrockSamplingParams(), tc.model)
			if cfg == nil {
				t.Fatalf("convertInferenceConfig(%q) returned nil", tc.model)
			}

			if tc.wantDropped {
				if cfg.Temperature != nil {
					t.Errorf("model %q: expected inferenceConfig.temperature to be dropped (Bedrock 400s on it for adaptive-only Claude), got %v", tc.model, *cfg.Temperature)
				}
				if cfg.TopP != nil {
					t.Errorf("model %q: expected inferenceConfig.topP to be dropped, got %v", tc.model, *cfg.TopP)
				}
			} else {
				if cfg.Temperature == nil || *cfg.Temperature != 0.7 {
					t.Errorf("model %q: expected inferenceConfig.temperature 0.7 to be preserved, got %v", tc.model, cfg.Temperature)
				}
				if cfg.TopP == nil || *cfg.TopP != 0.9 {
					t.Errorf("model %q: expected inferenceConfig.topP 0.9 to be preserved, got %v", tc.model, cfg.TopP)
				}
			}

			// Suppression is scoped: unrelated inference fields survive in
			// every case, so a passing "dropped" assertion can't be an artifact
			// of the whole config being blanked.
			if cfg.MaxTokens == nil || *cfg.MaxTokens != 4096 {
				t.Errorf("model %q: expected maxTokens 4096 to survive, got %v", tc.model, cfg.MaxTokens)
			}
			// stopSequences has its own pre-existing carve-out (GLM rejects the
			// field); everywhere else it must be untouched by this change.
			if !schemas.IsGLMModel(tc.model) {
				if len(cfg.StopSequences) != 1 || cfg.StopSequences[0] != "STOP" {
					t.Errorf("model %q: expected stopSequences to survive, got %v", tc.model, cfg.StopSequences)
				}
			} else if cfg.StopSequences != nil {
				t.Errorf("model %q: expected the pre-existing GLM stopSequences carve-out to still apply, got %v", tc.model, cfg.StopSequences)
			}
		})
	}
}

// TestConvertChatParameters_AdaptiveOnlyClaudeDropsSamplingParams walks the
// full Chat → Converse conversion (not just the inference-config helper) to
// prove nothing downstream re-populates temperature/topP.
func TestConvertChatParameters_AdaptiveOnlyClaudeDropsSamplingParams(t *testing.T) {
	cases := []struct {
		model       string
		wantDropped bool
	}{
		{"us.anthropic.claude-opus-5-20260901-v1:0", true},
		{"global.anthropic.claude-sonnet-5", true},
		{"anthropic.claude-fable-5", true},
		{"anthropic.claude-sonnet-4-5-20250929-v1:0", false},
		{"amazon.nova-pro-v1:0", false},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			bifrostReq := &schemas.BifrostChatRequest{
				Provider: schemas.Bedrock,
				Model:    tc.model,
				Input: []schemas.ChatMessage{
					{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hi")}},
				},
				Params: bedrockSamplingParams(),
			}
			bedrockReq := &BedrockConverseRequest{}
			if err := convertChatParameters(ctx, bifrostReq, bedrockReq); err != nil {
				t.Fatalf("convertChatParameters(%q): %v", tc.model, err)
			}
			if bedrockReq.InferenceConfig == nil {
				t.Fatalf("model %q: expected InferenceConfig to be set", tc.model)
			}
			gotTemp := bedrockReq.InferenceConfig.Temperature != nil
			gotTopP := bedrockReq.InferenceConfig.TopP != nil
			if tc.wantDropped && (gotTemp || gotTopP) {
				t.Errorf("model %q: expected temperature/topP to be dropped, got temperature=%v topP=%v",
					tc.model, bedrockReq.InferenceConfig.Temperature, bedrockReq.InferenceConfig.TopP)
			}
			if !tc.wantDropped && (!gotTemp || !gotTopP) {
				t.Errorf("model %q: expected temperature/topP to be preserved, got temperature=%v topP=%v",
					tc.model, bedrockReq.InferenceConfig.Temperature, bedrockReq.InferenceConfig.TopP)
			}
		})
	}
}

// TestToBedrockResponsesRequest_AdaptiveOnlyClaudeDropsSamplingParams covers
// the second site that builds a Converse request. The /v1/responses path
// assembles its own BedrockInferenceConfig rather than calling
// convertInferenceConfig, so fixing only the Chat path would leave the same
// 400 reachable through Responses.
func TestToBedrockResponsesRequest_AdaptiveOnlyClaudeDropsSamplingParams(t *testing.T) {
	cases := []struct {
		model       string
		wantDropped bool
	}{
		{"us.anthropic.claude-opus-5-20260901-v1:0", true},
		{"anthropic.claude-opus-5", true},
		{"global.anthropic.claude-sonnet-5", true},
		{"us.anthropic.claude-opus-6", true},
		{"anthropic.claude-sonnet-4-5-20250929-v1:0", false},
		{"anthropic.claude-opus-4-6", false},
		{"amazon.nova-pro-v1:0", false},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			req := &schemas.BifrostResponsesRequest{
				Model: tc.model,
				Input: []schemas.ResponsesMessage{glmUserMessage("hello")},
				Params: &schemas.ResponsesParameters{
					MaxOutputTokens: schemas.Ptr(4096),
					Temperature:     schemas.Ptr(0.7),
					TopP:            schemas.Ptr(0.9),
				},
			}

			bedrockReq, err := ToBedrockResponsesRequest(ctx, req)
			if err != nil {
				t.Fatalf("ToBedrockResponsesRequest(%q): %v", tc.model, err)
			}
			if bedrockReq.InferenceConfig == nil {
				t.Fatalf("model %q: expected InferenceConfig to be set", tc.model)
			}
			gotTemp := bedrockReq.InferenceConfig.Temperature != nil
			gotTopP := bedrockReq.InferenceConfig.TopP != nil
			if tc.wantDropped && (gotTemp || gotTopP) {
				t.Errorf("model %q: expected temperature/topP to be dropped, got temperature=%v topP=%v",
					tc.model, bedrockReq.InferenceConfig.Temperature, bedrockReq.InferenceConfig.TopP)
			}
			if !tc.wantDropped && (!gotTemp || !gotTopP) {
				t.Errorf("model %q: expected temperature/topP to be preserved, got temperature=%v topP=%v",
					tc.model, bedrockReq.InferenceConfig.Temperature, bedrockReq.InferenceConfig.TopP)
			}
			// maxTokens must survive regardless — proves the suppression is
			// narrow rather than blanking the whole inference config.
			if bedrockReq.InferenceConfig.MaxTokens == nil || *bedrockReq.InferenceConfig.MaxTokens != 4096 {
				t.Errorf("model %q: expected maxTokens 4096 to survive, got %v", tc.model, bedrockReq.InferenceConfig.MaxTokens)
			}
		})
	}
}
