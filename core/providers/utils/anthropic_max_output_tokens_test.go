package utils

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// servedClaudeModels are the Claude models the gateway fronts. Each must resolve
// to its real ceiling from the static table, because that table is the only
// backstop when the datasheet cache and the DB miss handler both come up empty
// — a cold start, an un-synced datasheet, or a model newer than the last sync.
//
// claude-opus-5 shipped absent from the table and hit exactly that gap in
// production on 2026-08-01: a /v1/messages request with no max_tokens was
// silently capped at AnthropicDefaultMaxTokens (4096) and returned
// stop_reason "max_tokens" against the model's own 128000 ceiling. The caller
// could not tell that from a finished answer. claude-opus-4-7 and
// claude-opus-4-8 were missing from the table too and were one datasheet miss
// away from the same failure.
//
// Ceilings are the values Anthropic itself reports when max_tokens is
// overshot ("max_tokens: N > 128000, which is the maximum allowed number of
// output tokens for <model>"), measured 2026-08-01 — not copied from a docs page.
var servedClaudeModels = map[string]int{
	"claude-opus-5":     128000,
	"claude-opus-4-8":   128000,
	"claude-opus-4-7":   128000,
	"claude-sonnet-5":   128000,
	"claude-fable-5":    128000,
	"claude-sonnet-4-6": 64000,
	"claude-haiku-4-5":  64000,
}

func TestServedClaudeModelsResolveRealMaxOutputTokens(t *testing.T) {
	const sentinel = 4096 // AnthropicDefaultMaxTokens

	for model, want := range servedClaudeModels {
		got := GetMaxOutputTokensOrDefault(schemas.Anthropic, model, sentinel)
		if got == sentinel && want != sentinel {
			t.Errorf("%s fell through to the %d default: a request omitting max_tokens "+
				"would be silently truncated at %d instead of %d", model, sentinel, sentinel, want)
			continue
		}
		if got != want {
			t.Errorf("%s: max output tokens = %d, want %d", model, got, want)
		}
	}
}
