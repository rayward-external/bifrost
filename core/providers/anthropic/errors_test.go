package anthropic

import (
	"testing"

	schemas "github.com/maximhq/bifrost/core/schemas"
)

func TestToAnthropicChatCompletionError(t *testing.T) {
	strPtr := func(s string) *string { return &s }

	tests := []struct {
		name         string
		input        *schemas.BifrostError
		expectNil    bool
		expectedType string
	}{
		{
			name:      "nil BifrostError returns nil",
			input:     nil,
			expectNil: true,
		},
		{
			name: "nil ErrorField.Type defaults to api_error",
			input: &schemas.BifrostError{
				Error: &schemas.ErrorField{
					Type:    nil,
					Message: "connection failed",
				},
			},
			expectedType: "api_error",
		},
		{
			name: "empty string Type defaults to api_error",
			input: &schemas.BifrostError{
				Error: &schemas.ErrorField{
					Type:    strPtr(""),
					Message: "rate limited",
				},
			},
			expectedType: "api_error",
		},
		{
			name: "valid Type is preserved",
			input: &schemas.BifrostError{
				Error: &schemas.ErrorField{
					Type:    strPtr("rate_limit_error"),
					Message: "rate limited",
				},
			},
			expectedType: "rate_limit_error",
		},
		{
			name: "internal Type is preserved",
			input: &schemas.BifrostError{
				Error: &schemas.ErrorField{
					Type:    strPtr("request_cancelled"),
					Message: "cancelled",
				},
			},
			expectedType: "request_cancelled",
		},
		{
			name: "nil Error field defaults to api_error",
			input: &schemas.BifrostError{
				Error: nil,
			},
			expectedType: "api_error",
		},
		{
			name: "top-level Type is used when Error.Type is absent",
			input: &schemas.BifrostError{
				Type: strPtr("virtual_key_required"),
				Error: &schemas.ErrorField{
					Message: "virtual key is required. Provide a virtual key via the x-bf-vk header.",
				},
			},
			expectedType: "virtual_key_required",
		},
		{
			name: "Error.Type wins over top-level Type so upstream fidelity survives",
			input: &schemas.BifrostError{
				Type: strPtr("gemini_api_error"),
				Error: &schemas.ErrorField{
					Type:    strPtr("overloaded_error"),
					Message: "the model is overloaded",
				},
			},
			expectedType: "overloaded_error",
		},
		{
			name: "empty-string Error.Type still falls through to top-level Type",
			input: &schemas.BifrostError{
				Type: strPtr("virtual_key_not_found"),
				Error: &schemas.ErrorField{
					Type:    strPtr(""),
					Message: "virtual key not found.",
				},
			},
			expectedType: "virtual_key_not_found",
		},
		{
			name: "top-level Type used when Error field is nil",
			input: &schemas.BifrostError{
				Type:  strPtr("model_blocked"),
				Error: nil,
			},
			expectedType: "model_blocked",
		},
		{
			name:         "both Type sources unset defaults to api_error",
			input:        &schemas.BifrostError{},
			expectedType: "api_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ToAnthropicChatCompletionError(tt.input)

			if tt.expectNil {
				if result != nil {
					t.Fatalf("expected nil, got %+v", result)
				}
				return
			}

			if result == nil {
				t.Fatal("expected non-nil result")
			}

			if result.Type != "error" {
				t.Errorf("expected top-level Type %q, got %q", "error", result.Type)
			}

			if result.Error.Type != tt.expectedType {
				t.Errorf("expected error Type %q, got %q", tt.expectedType, result.Error.Type)
			}
		})
	}
}
