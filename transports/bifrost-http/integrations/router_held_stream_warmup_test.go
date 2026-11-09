package integrations

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/tracing"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// Fork-only test (see .github/fork-patches.txt).
//
// The held-stream tests in router_test.go bound how soon a held stream commits (hold/2 of a
// scripted upstream pause, or a 1s first-token deadline). The first held stream in a test
// binary also pays one-time costs inside that window: on amd64, sonic JIT-compiles a codec per
// Go type on its first use (the provider's stream event decoder, the request decoders, the
// response encoders), and under -race that took more than a second on a hosted runner.
// Whichever held-stream test ran first missed its bound, and skipping it by name only moved
// the failure to the next one.
//
// This test sends one held stream down each path those tests use, untimed, so their bounds
// measure the held stream and not process warm-up. It must run before them: go runs a
// package's tests in file name order, then source order, and this file sorts before
// router_test.go (check with `go test -list HeldStream`).
func TestHeldStreamWarmUp(t *testing.T) {
	const hold = 100 * time.Millisecond
	routes := []struct {
		name, uri, body, end string
	}{
		{
			name: "anthropic messages",
			uri:  "/anthropic/v1/messages",
			body: `{"model":"anthropic/claude-test","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			end:  "event: message_stop",
		},
		{
			name: "openai chat completions",
			uri:  "/openai/v1/chat/completions",
			body: `{"model":"anthropic/claude-test","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			end:  "data: [DONE]",
		},
	}
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			res := runHeldStream(t, &holdUntilFinalPlugin{}, hold, route.uri, route.body)
			require.Equal(t, fasthttp.StatusOK, res.status, "body: %s", res.body)
			require.Contains(t, res.body, route.end, "the held stream was not committed and released")
		})
	}

	// TestHeldStreamFirstTokenDeadline streams from an OpenAI upstream through the core,
	// which decodes different types than the routes above.
	t.Run("openai upstream", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, data := range []string{
				`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"warm"}}]}`,
				`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				"[DONE]",
			} {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
				w.(http.Flusher).Flush()
			}
		}))
		t.Cleanup(upstream.Close)

		store := tracing.NewTraceStore(5*time.Minute, nil)
		t.Cleanup(store.Stop)
		tracer := tracing.NewTracer(store, nil, nil)
		t.Cleanup(tracer.Stop)
		client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
			Account:    heldDeadlineAccount{primaryURL: upstream.URL, fallbackURL: upstream.URL},
			LLMPlugins: []schemas.LLMPlugin{&holdUntilFinalPlugin{}},
			Tracer:     tracer,
			Logger:     bifrost.NewDefaultLogger(schemas.LogLevelError),
		})
		require.NoError(t, err)
		t.Cleanup(client.Shutdown)

		ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(30*time.Second))
		stream, bifrostErr := client.ChatCompletionStreamRequest(ctx, &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-4o-mini",
			Input:    []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hi")}}},
		})
		require.Nil(t, bifrostErr, "stream failed: %+v", bifrostErr)
		chunks := 0
		ended := time.After(30 * time.Second) // a hang guard, not a timing bound
		for open := true; open; {
			select {
			case chunk, ok := <-stream:
				if !ok {
					open = false
					break
				}
				require.Nil(t, chunk.BifrostError, "stream error")
				chunks++
			case <-ended:
				t.Fatal("the held stream never ended")
			}
		}
		require.NotZero(t, chunks, "the held stream was not released")
	})
}
