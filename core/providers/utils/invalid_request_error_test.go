package utils

import (
	"errors"
	"fmt"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// A caller-input failure must surface as 400 invalid_request_error, not the
// default 500. Before this, requesting reasoning on an Anthropic model with
// max_tokens below the 1024 minimum returned:
//
//	HTTP 500 {"is_bifrost_error":true,"error":{
//	  "error":"max_tokens must be greater than 1024 for reasoning",
//	  "message":"failed to convert bifrost request to the expected provider request body"}}
//
// which SDKs treat as a retryable server fault, so they retry a request that can
// never succeed instead of telling the caller to raise max_tokens.
func TestNewBifrostOperationErrorMapsInvalidRequestTo400(t *testing.T) {
	err := NewInvalidRequestError("max_tokens must be greater than %d for reasoning", 1024)

	bifrostErr := NewBifrostOperationError(schemas.ErrRequestBodyConversion, err)

	if bifrostErr.StatusCode == nil {
		t.Fatal("StatusCode must be set for a caller-input failure; nil renders as 500")
	}
	if *bifrostErr.StatusCode != 400 {
		t.Fatalf("StatusCode = %d, want 400", *bifrostErr.StatusCode)
	}
	if bifrostErr.Type == nil || *bifrostErr.Type != schemas.InvalidRequestErrorType {
		t.Fatalf("Type = %v, want %q", bifrostErr.Type, schemas.InvalidRequestErrorType)
	}
	if bifrostErr.Error.Type == nil || *bifrostErr.Error.Type != schemas.InvalidRequestErrorType {
		t.Fatalf("Error.Type = %v, want %q", bifrostErr.Error.Type, schemas.InvalidRequestErrorType)
	}
	// The actionable message must be the surfaced one, not the generic banner.
	want := "max_tokens must be greater than 1024 for reasoning"
	if bifrostErr.Error.Message != want {
		t.Fatalf("Error.Message = %q, want %q", bifrostErr.Error.Message, want)
	}
}

// The 400 mapping must NOT swallow genuine internal failures — those stay 500.
// Without this, a real marshalling bug would start reporting as the caller's fault.
func TestNewBifrostOperationErrorLeavesInternalErrorsAt500(t *testing.T) {
	bifrostErr := NewBifrostOperationError(
		schemas.ErrRequestBodyConversion,
		errors.New("json: unsupported type chan int"),
	)

	if bifrostErr.StatusCode != nil {
		t.Fatalf("StatusCode = %d, want nil (renders as 500) for an internal failure", *bifrostErr.StatusCode)
	}
	if bifrostErr.Type != nil {
		t.Fatalf("Type = %q, want nil for an internal failure", *bifrostErr.Type)
	}
	if bifrostErr.Error.Message != schemas.ErrRequestBodyConversion {
		t.Fatalf("Error.Message = %q, want the generic banner", bifrostErr.Error.Message)
	}
}

// Classification must survive wrapping — conversion helpers routinely add context
// with %w, and an errors.As-based check has to see through that.
func TestInvalidRequestErrorSurvivesWrapping(t *testing.T) {
	base := NewInvalidRequestError("max_tokens must be greater than %d for reasoning", 1024)
	wrapped := fmt.Errorf("converting anthropic chat request: %w", base)

	if !IsInvalidRequestError(wrapped) {
		t.Fatal("IsInvalidRequestError must see through %w wrapping")
	}

	bifrostErr := NewBifrostOperationError(schemas.ErrRequestBodyConversion, wrapped)
	if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 {
		t.Fatal("a wrapped InvalidRequestError must still map to 400")
	}
}

// Mutation guard: assert the helper is what actually drives the classification.
// A plain fmt.Errorf carrying the identical message must NOT be classified as 400,
// otherwise the test above would pass even if the mapping were keyed on message text.
func TestClassificationIsTypeBasedNotMessageBased(t *testing.T) {
	lookalike := fmt.Errorf("max_tokens must be greater than %d for reasoning", 1024)

	if IsInvalidRequestError(lookalike) {
		t.Fatal("classification must be type-based; an untyped error with the same text must not qualify")
	}

	bifrostErr := NewBifrostOperationError(schemas.ErrRequestBodyConversion, lookalike)
	if bifrostErr.StatusCode != nil {
		t.Fatalf("untyped lookalike mapped to %d; classification is leaking to message text", *bifrostErr.StatusCode)
	}
}

// The real call path: GetBudgetTokensFromReasoningEffort is what the Anthropic
// legs invoke, so the type has to originate there, not just at the mapping layer.
func TestGetBudgetTokensFromReasoningEffortReturnsInvalidRequest(t *testing.T) {
	_, err := GetBudgetTokensFromReasoningEffort("high", 1024, 200)
	if err == nil {
		t.Fatal("expected an error when max_tokens is below the reasoning minimum")
	}
	if !IsInvalidRequestError(err) {
		t.Fatalf("err %T must be an InvalidRequestError so it maps to 400", err)
	}

	// And the success path must stay clean.
	if _, err := GetBudgetTokensFromReasoningEffort("high", 1024, 4096); err != nil {
		t.Fatalf("valid budget returned an error: %v", err)
	}
}
