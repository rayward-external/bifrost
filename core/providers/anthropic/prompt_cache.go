package anthropic

import (
	"encoding/json"
	"os"
	"strings"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/sjson"
)

const (
	anthropicPromptCacheTTLEnv         = "ANTHROPIC_PROMPT_CACHE_TTL"
	anthropicPromptCacheTTLHeader      = "x-anthropic-prompt-cache-ttl"
	anthropicPromptCacheWorkloadHeader = "x-anthropic-prompt-cache-workload"
)

// ApplyAnthropicPromptCacheControl adds a single top-level Anthropic automatic
// prompt-cache directive when gateway policy requests it and the client did not
// already provide any cache_control marker.
func ApplyAnthropicPromptCacheControl(ctx *schemas.BifrostContext, req *AnthropicMessageRequest) bool {
	if req == nil {
		return false
	}
	ttl := resolveAnthropicPromptCacheTTL(ctx)
	if ttl == "" || anthropicRequestHasCacheControl(req) {
		return false
	}
	req.CacheControl = newAnthropicPromptCacheControl(ttl)
	return true
}

func ApplyAnthropicPromptCacheControlToRawBody(ctx *schemas.BifrostContext, body []byte) ([]byte, bool, error) {
	ttl := resolveAnthropicPromptCacheTTL(ctx)
	if ttl == "" || rawJSONHasCacheControl(body) {
		return body, false, nil
	}
	cc, err := providerUtils.MarshalSorted(newAnthropicPromptCacheControl(ttl))
	if err != nil {
		return body, false, err
	}
	out, err := sjson.SetRawBytes(body, "cache_control", cc)
	if err != nil {
		return body, false, err
	}
	return out, true, nil
}

func StripAnthropicPromptCacheControlHeaders(headers map[string][]string) {
	for k := range headers {
		if isAnthropicPromptCacheControlHeader(k) {
			delete(headers, k)
		}
	}
}

func newAnthropicPromptCacheControl(ttl string) *schemas.CacheControl {
	return &schemas.CacheControl{
		Type: schemas.CacheControlTypeEphemeral,
		TTL:  schemas.Ptr(ttl),
	}
}

func anthropicRequestHasCacheControl(req *AnthropicMessageRequest) bool {
	if req.CacheControl != nil {
		return true
	}
	data, err := providerUtils.MarshalSorted(req)
	if err != nil {
		return false
	}
	return rawJSONHasCacheControl(data)
}

func rawJSONHasCacheControl(body []byte) bool {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return false
	}
	return hasCacheControlKey(value)
}

func hasCacheControlKey(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if key == "cache_control" && child != nil {
				return true
			}
			if hasCacheControlKey(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if hasCacheControlKey(child) {
				return true
			}
		}
	}
	return false
}

func resolveAnthropicPromptCacheTTL(ctx *schemas.BifrostContext) string {
	headers, _ := ctx.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	rawTTL := firstHeaderValue(headers, anthropicPromptCacheTTLHeader)
	if rawTTL == "" {
		rawTTL = os.Getenv(anthropicPromptCacheTTLEnv)
	}
	return normalizeAnthropicPromptCacheTTL(rawTTL, firstHeaderValue(headers, anthropicPromptCacheWorkloadHeader))
}

func firstHeaderValue(headers map[string]string, name string) string {
	if len(headers) == 0 {
		return ""
	}
	return headers[strings.ToLower(name)]
}

func normalizeAnthropicPromptCacheTTL(value, workload string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "off", "false", "none", "disabled":
		return ""
	case "5m":
		return "5m"
	case "1h":
		return "1h"
	case "auto":
		if isLongRunningAnthropicWorkload(workload) {
			return "1h"
		}
		return "5m"
	default:
		return ""
	}
}

func isLongRunningAnthropicWorkload(workload string) bool {
	switch strings.ToLower(strings.TrimSpace(workload)) {
	case "eval", "evaluation", "benchmark", "bench", "batch", "pipeline", "long", "long-running":
		return true
	default:
		return false
	}
}

func isAnthropicPromptCacheControlHeader(name string) bool {
	switch strings.ToLower(name) {
	case anthropicPromptCacheTTLHeader, anthropicPromptCacheWorkloadHeader:
		return true
	default:
		return false
	}
}
