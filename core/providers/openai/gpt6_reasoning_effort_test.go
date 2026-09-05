package openai

import (
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// gpt6ChatRequest builds the chat request shape the HTTP transport hands the
// provider: a function tool plus whatever reasoning the caller sent (none when
// effort is empty).
func gpt6ChatRequest(provider schemas.ModelProvider, model, effort string) *schemas.BifrostChatRequest {
	params := &schemas.ChatParameters{
		Tools: []schemas.ChatTool{{
			Type: schemas.ChatToolTypeFunction,
			Function: &schemas.ChatToolFunction{
				Name:        "get_weather",
				Description: schemas.Ptr("Get weather"),
				Parameters: &schemas.ToolFunctionParameters{
					Type: "object",
					Properties: schemas.NewOrderedMapFromPairs(
						schemas.KV("city", map[string]interface{}{"type": "string"}),
					),
					Required: []string{"city"},
				},
			},
		}},
	}
	if effort != "" {
		params.Reasoning = &schemas.ChatReasoning{Effort: schemas.Ptr(effort)}
	}
	return &schemas.BifrostChatRequest{
		Provider: provider,
		Model:    model,
		Input: []schemas.ChatMessage{{
			Role:    schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("what is the weather in Paris?")},
		}},
		Params: params,
	}
}

func gpt6WireBody(t *testing.T, req *schemas.BifrostChatRequest) string {
	t.Helper()
	out := ToOpenAIChatRequest(schemas.NewBifrostContext(nil, schemas.NoDeadline), req)
	if out == nil {
		t.Fatal("expected OpenAI chat request")
	}
	body, err := out.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal outgoing body: %v", err)
	}
	return string(body)
}

// TestGPT6XHighReasoningEffortIsForwarded pins that a caller's "xhigh" survives
// to the wire on the gpt-6 family.
//
// The effort ladder is name-derived (defaultEffortControl -> acceptsXHighEffort):
// the datasheet carries supports_xhigh_reasoning_effort as a boolean, and
// ModelCaps.ReasoningEffortLevels only reads an explicit reasoning_effort_levels
// array, so a family missing from acceptsXHighEffort gets the base low/medium/high
// ladder and NormalizeReasoningEffort silently snaps "xhigh" down to "high".
//
// Measured against the provider directly on 2026-09-05, bypassing the gateway
// layer: gpt-6-astra answers 200 to reasoning_effort="xhigh" and spends
// measurably more reasoning than "high" on the identical prompt (256 vs 140
// reasoning tokens), while "max" is rejected with
// "Unsupported value: 'reasoning_effort' does not support 'max' with this model.
// Supported values are: 'low', 'medium', 'high', and 'xhigh'." So xhigh must be
// forwarded and max must still clamp.
func TestGPT6XHighReasoningEffortIsForwarded(t *testing.T) {
	for _, provider := range []schemas.ModelProvider{schemas.OpenAI, schemas.Azure} {
		for _, model := range []string{"gpt-6-astra", "gpt-6-astra-2026-09-03"} {
			t.Run(string(provider)+"/"+model, func(t *testing.T) {
				caps := schemas.ResolveModelCaps(provider, model)
				if got := caps.NormalizeReasoningEffort("xhigh", defaultEffortControl(model)); got != "xhigh" {
					t.Fatalf("NormalizeReasoningEffort(xhigh) = %q, want %q", got, "xhigh")
				}
				body := gpt6WireBody(t, gpt6ChatRequest(provider, model, "xhigh"))
				if !strings.Contains(body, `"reasoning_effort":"xhigh"`) {
					t.Fatalf("outgoing body dropped the caller's xhigh effort: %s", body)
				}
			})
		}
	}
}

// TestGPT6MaxReasoningEffortStillClamps keeps the widening narrow: "max" is not
// in gpt-6-astra's accepted vocabulary (measured, see the test above), so it must
// still resolve down to the strongest level the model does accept.
func TestGPT6MaxReasoningEffortStillClamps(t *testing.T) {
	caps := schemas.ResolveModelCaps(schemas.Azure, "gpt-6-astra")
	if got := caps.NormalizeReasoningEffort("max", defaultEffortControl("gpt-6-astra")); got != "xhigh" {
		t.Fatalf("NormalizeReasoningEffort(max) = %q, want %q", got, "xhigh")
	}
}

// TestGPT6NoReasoningEffortInjectedWhenCallerSentNone is a regression guard: a
// chat request carrying function tools and no reasoning field at all must reach
// the provider with no reasoning_effort key.
//
// Some providers reject function tools combined with a non-"none" reasoning
// effort on /v1/chat/completions and report the failure against the
// reasoning_effort parameter, which reads like the gateway injected one. It does
// not, and this test keeps it that way.
func TestGPT6NoReasoningEffortInjectedWhenCallerSentNone(t *testing.T) {
	for _, provider := range []schemas.ModelProvider{schemas.OpenAI, schemas.Azure} {
		t.Run(string(provider), func(t *testing.T) {
			body := gpt6WireBody(t, gpt6ChatRequest(provider, "gpt-6-astra", ""))
			if strings.Contains(body, "reasoning_effort") || strings.Contains(body, `"reasoning"`) {
				t.Fatalf("outgoing body invented a reasoning field: %s", body)
			}
		})
	}
}
