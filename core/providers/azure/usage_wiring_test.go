package azure_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/providers/azure"
	"github.com/maximhq/bifrost/core/schemas"
)

// Wiring tests: drive the real provider against a fake upstream that reports
// usage additively (completion excludes reasoning; total = prompt + completion
// + reasoning) and assert the usage a caller receives follows the OpenAI
// convention. The additive flag selects the upstream's usage shape.
func newUsageUpstream(t *testing.T, additive bool) *httptest.Server {
	t.Helper()
	completion, outputTokens := 3897, 3897
	if additive {
		completion, outputTokens = 368, 368
	}
	chatUsage := `"usage":{"prompt_tokens":11,"completion_tokens":` + itoa(completion) + `,"total_tokens":3908,"completion_tokens_details":{"reasoning_tokens":3529}}`
	respUsage := `"usage":{"input_tokens":11,"output_tokens":` + itoa(outputTokens) + `,"total_tokens":3908,"output_tokens_details":{"reasoning_tokens":3529}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		stream := strings.Contains(string(buf[:n]), `"stream":true`)
		switch r.URL.Path {
		case "/openai/v1/chat/completions":
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}` + "\n\n" +
					`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
					`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[],` + chatUsage + `}` + "\n\n" +
					"data: [DONE]\n\n"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` + chatUsage + `}`))
		case "/openai/v1/responses":
			resp := `{"id":"r","object":"response","created_at":1,"status":"completed","model":"m","output":[{"id":"msg","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}],` + respUsage + `}`
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("event: response.completed\ndata: " + `{"type":"response.completed","sequence_number":1,"response":` + resp + `}` + "\n\n"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(resp))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func TestAzureNormalizesAdditiveReasoningUsage(t *testing.T) {
	provider, err := azure.NewAzureProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{DefaultRequestTimeoutInSeconds: 10},
	}, routingTestLogger{})
	if err != nil {
		t.Fatalf("NewAzureProvider: %v", err)
	}
	key := func(endpoint string) schemas.Key {
		return schemas.Key{
			Value:          *schemas.NewSecretVar("test-key"),
			Models:         []string{"*"},
			AzureKeyConfig: &schemas.AzureKeyConfig{Endpoint: *schemas.NewSecretVar(endpoint)},
		}
	}
	chatReq := func() *schemas.BifrostChatRequest {
		return &schemas.BifrostChatRequest{
			Provider: schemas.Azure,
			Model:    "my-deployment",
			Input: []schemas.ChatMessage{{
				Role:    schemas.ChatMessageRoleUser,
				Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hi")},
			}},
		}
	}
	respReq := func() *schemas.BifrostResponsesRequest {
		return &schemas.BifrostResponsesRequest{
			Provider: schemas.Azure,
			Model:    "my-deployment",
			Input: []schemas.ResponsesMessage{{
				Role:    schemas.Ptr(schemas.ResponsesInputMessageRoleUser),
				Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("hi")},
			}},
		}
	}
	hook := func(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, e *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return resp, e
	}
	drain := func(t *testing.T, ch chan *schemas.BifrostStreamChunk) (chatUsage *schemas.BifrostLLMUsage, respUsage *schemas.ResponsesResponseUsage) {
		t.Helper()
		timeout := time.NewTimer(20 * time.Second)
		defer timeout.Stop()
		for {
			select {
			case c, ok := <-ch:
				if !ok {
					return
				}
				if c == nil {
					continue
				}
				if c.BifrostError != nil {
					t.Fatalf("stream error: %+v", c.BifrostError)
				}
				if c.BifrostChatResponse != nil && c.BifrostChatResponse.Usage != nil {
					chatUsage = c.BifrostChatResponse.Usage
				}
				if c.BifrostResponsesStreamResponse != nil && c.BifrostResponsesStreamResponse.Response != nil &&
					c.BifrostResponsesStreamResponse.Response.Usage != nil {
					respUsage = c.BifrostResponsesStreamResponse.Response.Usage
				}
			case <-timeout.C:
				t.Fatal("timed out draining stream")
			}
		}
	}
	newCtx := func() (*schemas.BifrostContext, context.CancelFunc) {
		return schemas.NewBifrostContextWithTimeout(context.Background(), 15*time.Second)
	}

	for _, additive := range []bool{true, false} {
		name := "openai_convention"
		if additive {
			name = "additive"
		}
		// Both shapes must reach the same client-visible usage: completion 3897 (text + reasoning), total 3908.
		t.Run(name+"/chat", func(t *testing.T) {
			ctx, cancel := newCtx()
			defer cancel()
			resp, e := provider.ChatCompletion(ctx, key(newUsageUpstream(t, additive).URL), chatReq())
			if e != nil {
				t.Fatalf("ChatCompletion: %+v", e)
			}
			if u := resp.Usage; u.CompletionTokens != 3897 || u.TotalTokens != 3908 || u.PromptTokens != 11 {
				t.Errorf("usage = %+v", u)
			}
		})
		t.Run(name+"/chat_stream", func(t *testing.T) {
			ctx, cancel := newCtx()
			defer cancel()
			ch, e := provider.ChatCompletionStream(ctx, hook, nil, key(newUsageUpstream(t, additive).URL), chatReq())
			if e != nil {
				t.Fatalf("ChatCompletionStream: %+v", e)
			}
			u, _ := drain(t, ch)
			if u == nil {
				t.Fatal("no usage in stream")
			}
			if u.CompletionTokens != 3897 || u.TotalTokens != 3908 {
				t.Errorf("usage = %+v", u)
			}
		})
		t.Run(name+"/responses", func(t *testing.T) {
			ctx, cancel := newCtx()
			defer cancel()
			resp, e := provider.Responses(ctx, key(newUsageUpstream(t, additive).URL), respReq())
			if e != nil {
				t.Fatalf("Responses: %+v", e)
			}
			if u := resp.Usage; u.OutputTokens != 3897 || u.TotalTokens != 3908 {
				t.Errorf("usage = %+v", u)
			}
		})
		t.Run(name+"/responses_stream", func(t *testing.T) {
			ctx, cancel := newCtx()
			defer cancel()
			ch, e := provider.ResponsesStream(ctx, hook, nil, key(newUsageUpstream(t, additive).URL), respReq())
			if e != nil {
				t.Fatalf("ResponsesStream: %+v", e)
			}
			_, u := drain(t, ch)
			if u == nil {
				t.Fatal("no usage in stream")
			}
			if u.OutputTokens != 3897 || u.TotalTokens != 3908 {
				t.Errorf("usage = %+v", u)
			}
		})
	}
}
