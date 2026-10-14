package azure

import (
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	schemas "github.com/maximhq/bifrost/core/schemas"
)

// Some models served through Azure AI Foundry (e.g. xAI Grok) report usage
// additively: completion_tokens covers visible text only, reasoning tokens are
// reported separately in completion_tokens_details.reasoning_tokens, and
// total_tokens = prompt + completion + reasoning.
//
// Everything downstream (cost calculation, client-visible usage) follows the
// OpenAI convention, where reasoning tokens are already inside
// completion_tokens, so additive usage is normalized here, at the provider
// boundary, by folding reasoning into completion. total_tokens is untouched.

// isAdditiveReasoningUsage reports whether the counters are additive. The check
// is exact: under the OpenAI convention total == prompt + completion, so with
// reasoning > 0 the equation below cannot hold.
func isAdditiveReasoningUsage(prompt, completion, reasoning, total int) bool {
	return reasoning > 0 && total == prompt+completion+reasoning
}

// NormalizeAdditiveReasoningChatUsage folds additively-reported reasoning tokens
// into CompletionTokens. It is a no-op for any other usage shape.
func NormalizeAdditiveReasoningChatUsage(usage *schemas.BifrostLLMUsage) {
	if usage == nil || usage.CompletionTokensDetails == nil {
		return
	}
	reasoning := usage.CompletionTokensDetails.ReasoningTokens
	if isAdditiveReasoningUsage(usage.PromptTokens, usage.CompletionTokens, reasoning, usage.TotalTokens) {
		usage.CompletionTokens += reasoning
	}
}

// NormalizeAdditiveReasoningResponsesUsage is the Responses API counterpart,
// operating on input_tokens / output_tokens.
func NormalizeAdditiveReasoningResponsesUsage(usage *schemas.ResponsesResponseUsage) {
	if usage == nil || usage.OutputTokensDetails == nil {
		return
	}
	reasoning := usage.OutputTokensDetails.ReasoningTokens
	if isAdditiveReasoningUsage(usage.InputTokens, usage.OutputTokens, reasoning, usage.TotalTokens) {
		usage.OutputTokens += reasoning
	}
}

// chatResponseHandler parses a chat completion body (or stream chunk) like the
// default handler, then normalizes its usage. Used for both the non-streaming
// and streaming paths, including the Responses-to-chat fallback.
func chatResponseHandler(body []byte, response *schemas.BifrostChatResponse, requestBody []byte, sendBackRawRequest bool, sendBackRawResponse bool) (interface{}, interface{}, *schemas.BifrostError) {
	rawRequest, rawResponse, err := providerUtils.HandleProviderResponse(body, response, requestBody, sendBackRawRequest, sendBackRawResponse)
	if err != nil {
		return rawRequest, rawResponse, err
	}
	NormalizeAdditiveReasoningChatUsage(response.Usage)
	return rawRequest, rawResponse, nil
}

// responsesResponseHandler is the non-streaming Responses API equivalent.
func responsesResponseHandler(body []byte, response *schemas.BifrostResponsesResponse, requestBody []byte, sendBackRawRequest bool, sendBackRawResponse bool) (interface{}, interface{}, *schemas.BifrostError) {
	rawRequest, rawResponse, err := providerUtils.HandleProviderResponse(body, response, requestBody, sendBackRawRequest, sendBackRawResponse)
	if err != nil {
		return rawRequest, rawResponse, err
	}
	NormalizeAdditiveReasoningResponsesUsage(response.Usage)
	return rawRequest, rawResponse, nil
}

// responsesStreamEventConverter normalizes the usage carried by terminal
// Responses stream events (response.completed / response.incomplete).
func responsesStreamEventConverter(event *schemas.BifrostResponsesStreamResponse) *schemas.BifrostResponsesStreamResponse {
	if event != nil && event.Response != nil {
		NormalizeAdditiveReasoningResponsesUsage(event.Response.Usage)
	}
	return event
}
