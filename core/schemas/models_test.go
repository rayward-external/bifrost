package schemas

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Raw types that mirror the real KeyStatus → BifrostError → ExtraFields → KeyStatus
// chain but without any custom MarshalJSON. Used to reproduce the cycle error
// that would occur without the fix.
type rawKeyStatusExtraFields struct {
	KeyStatuses []rawKeyStatus `json:"key_statuses,omitempty"`
}

type rawKeyStatusBifrostError struct {
	IsBifrostError bool                    `json:"is_bifrost_error"`
	Error          *ErrorField             `json:"error"`
	ExtraFields    rawKeyStatusExtraFields `json:"extra_fields"`
}

type rawKeyStatus struct {
	KeyID    string                    `json:"key_id"`
	Status   KeyStatusType             `json:"status"`
	Provider ModelProvider             `json:"provider"`
	Error    *rawKeyStatusBifrostError `json:"error,omitempty"`
}

// TestKeyStatusMarshalJSON_ReproduceCycle proves that without the custom MarshalJSON,
// the circular reference between KeyStatus and BifrostError causes a marshaling failure.
func TestKeyStatusMarshalJSON_ReproduceCycle(t *testing.T) {
	bifrostErr := &rawKeyStatusBifrostError{
		IsBifrostError: true,
		Error:          &ErrorField{Message: "test error"},
	}
	keyStatus := rawKeyStatus{
		KeyID:    "key-1",
		Status:   KeyStatusListModelsFailed,
		Provider: "test-provider",
		Error:    bifrostErr,
	}
	// Create the same cycle that HandleKeylessListModelsRequest creates
	bifrostErr.ExtraFields.KeyStatuses = []rawKeyStatus{keyStatus}

	// Without any custom MarshalJSON, this must fail with a cycle error
	_, err := json.Marshal(keyStatus)
	require.Error(t, err, "expected cycle error without the MarshalJSON fix")
	assert.Contains(t, err.Error(), "cycle", "error should mention a cycle")
}

func TestKeyStatusMarshalJSON_NoCycle(t *testing.T) {
	bifrostErr := &BifrostError{
		IsBifrostError: true,
		Error:          &ErrorField{Message: "test error"},
	}
	keyStatus := KeyStatus{
		KeyID:    "key-1",
		Status:   KeyStatusListModelsFailed,
		Provider: "test-provider",
		Error:    bifrostErr,
	}
	// Create the same cycle that HandleKeylessListModelsRequest creates
	bifrostErr.ExtraFields.KeyStatuses = []KeyStatus{keyStatus}

	data, err := Marshal(keyStatus)
	require.NoError(t, err, "Marshal should not fail on circular KeyStatus")

	// Verify the output doesn't contain nested key_statuses
	assert.False(t, bytes.Contains(data, []byte(`"key_statuses"`)),
		"expected key_statuses to be omitted from nested error")
}

func TestKeyStatusMarshalJSON_NilError(t *testing.T) {
	keyStatus := KeyStatus{
		KeyID:    "key-2",
		Status:   "success",
		Provider: "test-provider",
	}

	data, err := Marshal(keyStatus)
	require.NoError(t, err, "Marshal should not fail on KeyStatus with nil error")
	assert.Contains(t, string(data), `"key_id":"key-2"`)
	assert.NotContains(t, string(data), `"error"`)
}

func TestKeyStatusMarshalJSON_PreservesErrorFields(t *testing.T) {
	statusCode := 401
	bifrostErr := &BifrostError{
		IsBifrostError: true,
		StatusCode:     &statusCode,
		Error:          &ErrorField{Message: "unauthorized"},
		ExtraFields: BifrostErrorExtraFields{
			Provider:               "openai",
			OriginalModelRequested: "gpt-4",
		},
	}
	keyStatus := KeyStatus{
		KeyID:    "key-3",
		Status:   KeyStatusListModelsFailed,
		Provider: "openai",
		Error:    bifrostErr,
	}
	// Create cycle
	bifrostErr.ExtraFields.KeyStatuses = []KeyStatus{keyStatus}

	data, err := Marshal(keyStatus)
	require.NoError(t, err)

	// Error fields other than key_statuses should be preserved
	dataStr := string(data)
	assert.Contains(t, dataStr, `"unauthorized"`)
	assert.Contains(t, dataStr, `"original_model_requested":"gpt-4"`)
	assert.Contains(t, dataStr, `"status_code":401`)
}

func TestParseFallbacks_KeyAwareSpec(t *testing.T) {
	fallbacks := ParseFallbacks([]string{
		"azure/gpt-4o?key_id=azure-east%2Fprod",
		"openai/gpt-4o",
	})

	require.Len(t, fallbacks, 2)
	assert.Equal(t, Azure, fallbacks[0].Provider)
	assert.Equal(t, "gpt-4o", fallbacks[0].Model)
	assert.Equal(t, "azure-east/prod", fallbacks[0].KeyID)
	assert.Equal(t, OpenAI, fallbacks[1].Provider)
	assert.Equal(t, "gpt-4o", fallbacks[1].Model)
	assert.Empty(t, fallbacks[1].KeyID)
}

func TestFormatFallback_KeyAwareRoundTrip(t *testing.T) {
	spec := FormatFallback(Azure, "gpt-4o", "azure-east/prod")

	assert.Equal(t, "azure/gpt-4o?key_id=azure-east%2Fprod", spec)
	parsed := ParseFallbacks([]string{spec})
	require.Len(t, parsed, 1)
	assert.Equal(t, Azure, parsed[0].Provider)
	assert.Equal(t, "gpt-4o", parsed[0].Model)
	assert.Equal(t, "azure-east/prod", parsed[0].KeyID)
}

func TestParseModelString_StripsQuerySuffix(t *testing.T) {
	// A primary model field carrying a routing-hint query (e.g. a caller that
	// appends "?key_id=") must resolve to the clean model identity — otherwise
	// it pollutes the logs "Models" filter and breaks provider catalog lookups.
	provider, model := ParseModelString("gpt-4.1-mini?key_id=5ca02987-3cf9", "")
	assert.Empty(t, provider)
	assert.Equal(t, "gpt-4.1-mini", model)

	// Provider prefix and query together.
	provider, model = ParseModelString("openai/gpt-4o?key_id=abc", "")
	assert.Equal(t, OpenAI, provider)
	assert.Equal(t, "gpt-4o", model)

	// No query suffix — model is returned unchanged.
	provider, model = ParseModelString("anthropic/claude-3.5-sonnet", "")
	assert.Equal(t, Anthropic, provider)
	assert.Equal(t, "claude-3.5-sonnet", model)
}
