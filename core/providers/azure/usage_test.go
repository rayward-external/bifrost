package azure

import (
	"testing"

	schemas "github.com/maximhq/bifrost/core/schemas"
)

func chatUsage(prompt, completion, reasoning, total int) *schemas.BifrostLLMUsage {
	return &schemas.BifrostLLMUsage{
		PromptTokens:            prompt,
		CompletionTokens:        completion,
		TotalTokens:             total,
		CompletionTokensDetails: &schemas.ChatCompletionTokensDetails{ReasoningTokens: reasoning},
	}
}

func TestNormalizeAdditiveReasoningChatUsage(t *testing.T) {
	tests := []struct {
		name           string
		usage          *schemas.BifrostLLMUsage
		wantCompletion int
		wantTotal      int
	}{
		{"additive usage is folded", chatUsage(11, 368, 3529, 3908), 3897, 3908},
		{"openai convention is untouched", chatUsage(11, 3897, 3529, 3908), 3897, 3908},
		{"zero reasoning is untouched", chatUsage(11, 368, 0, 379), 368, 379},
		{"reasoning larger than completion under openai convention is untouched", chatUsage(10, 100, 90, 110), 100, 110},
		{"inconsistent total is untouched", chatUsage(11, 368, 3529, 4000), 368, 4000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			NormalizeAdditiveReasoningChatUsage(tt.usage)
			if tt.usage.CompletionTokens != tt.wantCompletion {
				t.Errorf("completion_tokens = %d, want %d", tt.usage.CompletionTokens, tt.wantCompletion)
			}
			if tt.usage.TotalTokens != tt.wantTotal {
				t.Errorf("total_tokens = %d, want %d", tt.usage.TotalTokens, tt.wantTotal)
			}
		})
	}
}

func TestNormalizeAdditiveReasoningChatUsage_NilSafe(t *testing.T) {
	NormalizeAdditiveReasoningChatUsage(nil)
	NormalizeAdditiveReasoningChatUsage(&schemas.BifrostLLMUsage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3})
}

func TestNormalizeAdditiveReasoningResponsesUsage(t *testing.T) {
	mk := func(in, out, reasoning, total int) *schemas.ResponsesResponseUsage {
		return &schemas.ResponsesResponseUsage{
			InputTokens: in, OutputTokens: out, TotalTokens: total,
			OutputTokensDetails: &schemas.ResponsesResponseOutputTokens{ReasoningTokens: reasoning},
		}
	}
	u := mk(11, 368, 3529, 3908)
	NormalizeAdditiveReasoningResponsesUsage(u)
	if u.OutputTokens != 3897 || u.TotalTokens != 3908 {
		t.Errorf("additive: got output=%d total=%d", u.OutputTokens, u.TotalTokens)
	}
	u = mk(11, 3897, 3529, 3908)
	NormalizeAdditiveReasoningResponsesUsage(u)
	if u.OutputTokens != 3897 {
		t.Errorf("openai convention changed: output=%d", u.OutputTokens)
	}
	u = mk(11, 368, 0, 379)
	NormalizeAdditiveReasoningResponsesUsage(u)
	if u.OutputTokens != 368 {
		t.Errorf("zero reasoning changed: output=%d", u.OutputTokens)
	}
	NormalizeAdditiveReasoningResponsesUsage(nil)
}

const additiveChatBody = `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
"usage":{"prompt_tokens":11,"completion_tokens":368,"total_tokens":3908,"completion_tokens_details":{"reasoning_tokens":3529}}}`

const openAIChatBody = `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
"usage":{"prompt_tokens":11,"completion_tokens":3897,"total_tokens":3908,"completion_tokens_details":{"reasoning_tokens":3529}}}`

func TestChatResponseHandler_NonStreaming(t *testing.T) {
	var resp schemas.BifrostChatResponse
	if _, _, err := chatResponseHandler([]byte(additiveChatBody), &resp, nil, false, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Usage.CompletionTokens != 3897 || resp.Usage.TotalTokens != 3908 || resp.Usage.PromptTokens != 11 {
		t.Errorf("got %+v", resp.Usage)
	}
	if resp.Usage.CompletionTokensDetails.ReasoningTokens != 3529 {
		t.Errorf("reasoning detail must be preserved, got %d", resp.Usage.CompletionTokensDetails.ReasoningTokens)
	}

	var oa schemas.BifrostChatResponse
	if _, _, err := chatResponseHandler([]byte(openAIChatBody), &oa, nil, false, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if oa.Usage.CompletionTokens != 3897 || oa.Usage.TotalTokens != 3908 {
		t.Errorf("openai-convention usage changed: %+v", oa.Usage)
	}
}

func TestChatResponseHandler_StreamingFinalChunk(t *testing.T) {
	content := `{"id":"x","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}`
	final := `{"id":"x","object":"chat.completion.chunk","model":"m","choices":[],
"usage":{"prompt_tokens":11,"completion_tokens":368,"total_tokens":3908,"completion_tokens_details":{"reasoning_tokens":3529}}}`

	var c schemas.BifrostChatResponse
	if _, _, err := chatResponseHandler([]byte(content), &c, nil, false, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Usage != nil {
		t.Errorf("content chunk must carry no usage, got %+v", c.Usage)
	}

	var f schemas.BifrostChatResponse
	if _, _, err := chatResponseHandler([]byte(final), &f, nil, false, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Usage.CompletionTokens != 3897 || f.Usage.TotalTokens != 3908 {
		t.Errorf("final chunk not normalized: %+v", f.Usage)
	}
}

func TestResponsesHandlers(t *testing.T) {
	body := `{"id":"r","object":"response","model":"m","status":"completed","output":[],
"usage":{"input_tokens":11,"output_tokens":368,"total_tokens":3908,"output_tokens_details":{"reasoning_tokens":3529}}}`
	var resp schemas.BifrostResponsesResponse
	if _, _, err := responsesResponseHandler([]byte(body), &resp, nil, false, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Usage.OutputTokens != 3897 || resp.Usage.TotalTokens != 3908 {
		t.Errorf("got %+v", resp.Usage)
	}

	ev := &schemas.BifrostResponsesStreamResponse{Response: &schemas.BifrostResponsesResponse{Usage: &schemas.ResponsesResponseUsage{
		InputTokens: 11, OutputTokens: 368, TotalTokens: 3908,
		OutputTokensDetails: &schemas.ResponsesResponseOutputTokens{ReasoningTokens: 3529},
	}}}
	if out := responsesStreamEventConverter(ev); out.Response.Usage.OutputTokens != 3897 {
		t.Errorf("stream event not normalized: %+v", out.Response.Usage)
	}
	if out := responsesStreamEventConverter(&schemas.BifrostResponsesStreamResponse{}); out == nil {
		t.Error("event without a response must pass through")
	}
}
