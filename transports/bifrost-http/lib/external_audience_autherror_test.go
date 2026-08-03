package lib

import (
	"strings"
	"testing"
)

// The four bodies below are ACTUAL responses measured on router2.trueward.ai on
// 2026-08-04, verbatim. Real payloads rather than hand-written stubs for the
// reason the sibling fixtures give: a synthetic body would be written from the
// same understanding as the fix, so it could not catch a misunderstanding of
// the shape. Both dialects AND both auth-failure classes are represented,
// because they differ in where the disclosure sits.

// Missing / unparseable key. Reachable with NO CREDENTIAL AT ALL.
const prodAuthRequiredOpenAI = `{"type":"virtual_key_required","status_code":401,"error":{"message":"virtual key is required. Provide a virtual key via the x-bf-vk header."}}`

// Same failure, Anthropic dialect. Note the root `type` is a generic "error"
// and the internal name is NOT in it — the message is the only signal here, so
// a fix keyed on the type alone would leave this one leaking.
const prodAuthRequiredAnthropic = `{"type":"error","error":{"type":"api_error","message":"virtual key is required. Provide a virtual key via the x-bf-vk header."}}`

// Well-formed but REVOKED key — a routine event whenever we rotate a customer's
// credential, not an edge case.
const prodAuthNotFoundOpenAI = `{"type":"virtual_key_not_found","status_code":401,"error":{"message":"virtual key not found. The provided virtual key does not exist or has been revoked."}}`

const prodAuthNotFoundAnthropic = `{"type":"error","error":{"type":"api_error","message":"virtual key not found. The provided virtual key does not exist or has been revoked."}}`

// authLeakMarkers are asserted individually so a failure names WHICH disclosure
// escaped rather than just "bodies differ".
var authLeakMarkers = []string{
	"x-bf-vk",               // names the gateway software (bf = Bifrost)
	"virtual key",           // our governance primitive, in prose
	"virtual_key_required",  // internal error vocabulary, as a type
	"virtual_key_not_found", // ditto
}

func assertNoAuthLeak(t *testing.T, body string) {
	t.Helper()
	lower := strings.ToLower(body)
	for _, marker := range authLeakMarkers {
		if strings.Contains(lower, strings.ToLower(marker)) {
			t.Errorf("external body still discloses %q\nbody: %s", marker, body)
		}
	}
}

func TestEveryProdAuthErrorIsNeutralized(t *testing.T) {
	for name, body := range map[string]string{
		"openai/missing-key":    prodAuthRequiredOpenAI,
		"anthropic/missing-key": prodAuthRequiredAnthropic,
		"openai/revoked-key":    prodAuthNotFoundOpenAI,
		"anthropic/revoked-key": prodAuthNotFoundAnthropic,
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := StripExtraFields([]byte(body))
			if !ok {
				t.Fatalf("policy reported failure on a well-formed auth error: %s", body)
			}
			assertNoAuthLeak(t, string(got))

			// The caller must still learn it is an AUTH problem and what to do.
			// Silently returning a contentless 401 would trade a disclosure for a
			// support ticket — the over-sanitization failure this policy is
			// otherwise careful to avoid.
			if !strings.Contains(string(got), "authentication") {
				t.Errorf("scrub left the caller unable to tell this was an auth failure\nbody: %s", got)
			}
			if !strings.Contains(string(got), "API key") {
				t.Errorf("scrub left the caller no actionable next step\nbody: %s", got)
			}
		})
	}
}

// The status code is the caller's primary signal and lives in the body for the
// OpenAI dialect. Neutralizing the vocabulary must not disturb it.
func TestNeutralizationPreservesTheStatusCode(t *testing.T) {
	got, ok := StripExtraFields([]byte(prodAuthRequiredOpenAI))
	if !ok {
		t.Fatal("policy reported failure")
	}
	if !strings.Contains(string(got), "401") {
		t.Errorf("the 401 was lost, so the caller cannot tell auth from a 500\nbody: %s", got)
	}
}

// THE SAFETY PROPERTY for a value-level rewrite. A model asked about gateway
// auth will write "virtual key" and "x-bf-vk" into its COMPLETION. That is the
// caller's own answer and must survive byte-for-byte; only the `error` envelope
// is in scope. A blanket value substitution would silently corrupt answers,
// which is why neutralizeAuthErrorIn is scoped to `error` and never walks.
func TestAModelsOwnProseAboutVirtualKeysIsUntouched(t *testing.T) {
	completion := `{"id":"chatcmpl-x","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"To authenticate, send your virtual key in the x-bf-vk header."}}],"usage":{"total_tokens":30}}`
	got, ok := StripExtraFields([]byte(completion))
	if !ok {
		t.Fatal("policy reported failure on a clean completion")
	}
	if string(got) != completion {
		t.Errorf("the policy rewrote a model's own answer.\n want: %s\n got:  %s", completion, got)
	}
}

// A NON-auth error must pass through with all detail intact. This is the #466 /
// #468 body — the trigger is now wider (it fires on auth vocabulary too), so
// this guards the widening against collateral damage.
func TestANonAuthErrorKeepsEveryActionableDetail(t *testing.T) {
	got, ok := StripExtraFields([]byte(prodErrorBody))
	if !ok {
		t.Fatal("policy reported failure on a well-formed error body")
	}
	for _, keep := range []string{
		"invalid_request_error",
		"invalid_value",
		"Invalid value: 'wizard'",
		"messages[0].role",
		"400",
	} {
		if !strings.Contains(string(got), keep) {
			t.Errorf("the auth-error widening destroyed unrelated error detail %q\nbody: %s", keep, got)
		}
	}
}

// A truncated auth error can carry the gateway name in the bytes that did
// arrive while no longer parsing. Failing open there would ship the exact
// disclosure this file removes.
func TestTruncatedAuthErrorFailsClosed(t *testing.T) {
	truncated := []byte(`{"type":"virtual_key_required","error":{"message":"provide a virtual key via the x-bf-v`)
	if _, ok := StripExtraFields(truncated); ok {
		t.Error("an unparseable body naming the gateway software was passed through")
	}
}

// NOTE: "internal callers keep the real message" is deliberately NOT tested
// here. ApplyExternalBodyPolicy does not gate on audience — the middleware does
// (external_audience_middleware.go: `if !isExternalAudience(ctx)`), so this
// function is only ever reached for external callers. Asserting it at this layer
// would have tested a gate that does not live here; it is covered in the
// handlers package against the real middleware instead.

// The neutralized body must remain valid JSON of the same shape a client parses.
func TestNeutralizedBodyKeepsTheErrorEnvelopeShape(t *testing.T) {
	got, ok := StripExtraFields([]byte(prodAuthRequiredAnthropic))
	if !ok {
		t.Fatal("policy reported failure")
	}
	// Anthropic clients read error.type; it must still be present and a string.
	if !strings.Contains(string(got), `"error"`) || !strings.Contains(string(got), `"message"`) {
		t.Errorf("the error envelope lost its shape, so a standard client cannot read it\nbody: %s", got)
	}
}

// ── regressions caught in review, before this ever shipped ──────────────────

// FIVE routine governance denials also contain the phrase "virtual key":
// model_blocked, provider_blocked, model-level rate-limit, budget-exceeded and
// MCP-tool-denied. The first draft matched that phrase anywhere and rewrote all
// of them to "authentication required: provide a valid API key" — telling a
// caller who hit a BUDGET CAP to go fix their credentials, and destroying the
// one detail they needed. Worse remediation than the disclosure being removed.
func TestRoutineGovernanceDenialsAreNotMistakenForAuthFailures(t *testing.T) {
	for name, body := range map[string]string{
		"model_blocked":    `{"type":"model_blocked","status_code":403,"error":{"message":"Model 'gpt-4o' is not allowed for this virtual key"}}`,
		"provider_blocked": `{"type":"provider_blocked","status_code":403,"error":{"message":"Provider 'openai' is not allowed for this virtual key"}}`,
		"budget_exceeded":  `{"type":"budget_exceeded","status_code":429,"error":{"message":"Model-level budget exceeded (virtual key scope): daily cap reached"}}`,
		"rate_limited":     `{"type":"rate_limited","status_code":429,"error":{"message":"Model-level rate limit check failed (virtual key scope): 60 rpm"}}`,
		"mcp_tool_denied":  `{"type":"mcp_tool_blocked","status_code":403,"error":{"message":"MCP tool 'shell' is not allowed for virtual key 'acme'"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := StripExtraFields([]byte(body))
			if !ok {
				t.Fatalf("policy reported failure on a routine denial: %s", body)
			}
			if string(got) != body {
				t.Errorf("a routine %s denial was rewritten as an auth failure.\n"+
					"the caller now gets the wrong remediation and loses the reason.\n"+
					" want: %s\n got:  %s", name, body, got)
			}
		})
	}
}

// A downloaded FILE is not a Bifrost-marshaled response. The first draft made
// `carriesAuthDisclosure` a fail-closed trigger, so any non-JSON body whose
// bytes matched was replaced wholesale with an internal-error JSON — corrupting
// a SUCCESSFUL file download. Caught in review.
func TestANonJSONFileMentioningTheVocabularySurvives(t *testing.T) {
	file := []byte("Onboarding notes\n\nStep 3: request a virtual key from the platform team.\n" +
		"Send it as x-bf-vk on internal calls.\n")
	got, ok := StripExtraFields(file)
	if !ok {
		t.Fatal("a plain-text file was failed closed and replaced with an error JSON")
	}
	if string(got) != string(file) {
		t.Errorf("a plain-text file download was corrupted.\n want: %q\n got:  %q", file, got)
	}
}
