package lib

import (
	"strings"
	"testing"

	"github.com/bytedance/sonic/ast"
)

// The OpenAI-dialect bodies below are ACTUAL responses measured on
// router2.trueward.ai on 2026-08-04, verbatim. The Anthropic-dialect bodies are
// the shape #497 produces post-fix (core/providers/anthropic/errors.go now
// passes BifrostError.Type through as nested `error.type` instead of collapsing
// it to generic "api_error" — see errors_test.go for that unit coverage) rather
// than a second live measurement, since as of this fix the Anthropic dialect no
// longer independently discloses anything: it derives from the same
// BifrostError.Type governance already sets. Both dialects AND both
// auth-failure classes are represented, because pre-#497 they differed in
// where the disclosure sat.

// Missing / unparseable key. Reachable with NO CREDENTIAL AT ALL.
const prodAuthRequiredOpenAI = `{"type":"virtual_key_required","status_code":401,"error":{"message":"virtual key is required. Provide a virtual key via the x-bf-vk header."}}`

// Same failure, Anthropic dialect. Pre-#497 the nested `error.type` was the
// generic "api_error" and the message was the only signal; #497 makes the
// Anthropic serializer fall back to BifrostError.Type (same value governance
// set) whenever the upstream-fidelity `Error.Type` field is absent, so this
// now carries the same internal identity the OpenAI dialect always did.
const prodAuthRequiredAnthropic = `{"type":"error","error":{"type":"virtual_key_required","message":"virtual key is required. Provide a virtual key via the x-bf-vk header."}}`

// Well-formed but REVOKED key — a routine event whenever we rotate a customer's
// credential, not an edge case.
const prodAuthNotFoundOpenAI = `{"type":"virtual_key_not_found","status_code":401,"error":{"message":"virtual key not found. The provided virtual key does not exist or has been revoked."}}`

const prodAuthNotFoundAnthropic = `{"type":"error","error":{"type":"virtual_key_not_found","message":"virtual key not found. The provided virtual key does not exist or has been revoked."}}`

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
//
// The fixture deliberately mentions "virtual_key_required" (the literal type
// string, post-#497 the only thing the raw-bytes probe looks for) so this still
// exercises the trigger-fires-but-is-not-fail-closed path rather than skipping
// the probe entirely via the fast path.
func TestANonJSONFileMentioningTheVocabularySurvives(t *testing.T) {
	file := []byte("Onboarding notes\n\nStep 3: our gateway raises virtual_key_required " +
		"when a caller sends no credential; provide one via x-bf-vk on internal calls.\n")
	got, ok := StripExtraFields(file)
	if !ok {
		t.Fatal("a plain-text file was failed closed and replaced with an error JSON")
	}
	if string(got) != string(file) {
		t.Errorf("a plain-text file download was corrupted.\n want: %q\n got:  %q", file, got)
	}
}

// The ENTERPRISE missing-credential message (governance main.go:1007, behind
// config.IsEnterprise) shares no prose prefix with the two standard auth
// failures — a message-matching predicate had to special-case it. Governance
// sets the SAME top-level Type ("virtual_key_required") for both the standard
// and enterprise message, only the message text differs by config flag, so a
// type-only predicate covers this variant for free: nothing to special-case,
// nothing that can be missed by a future message rewording behind the flag.
func TestTheEnterpriseAuthMessageIsAlsoNeutralized(t *testing.T) {
	const enterpriseMsg = "authentication is required. Provide a virtual key (x-bf-vk), API key, or user token."
	for name, body := range map[string]string{
		"openai":    `{"type":"virtual_key_required","status_code":401,"error":{"message":"` + enterpriseMsg + `"}}`,
		"anthropic": `{"type":"error","error":{"type":"virtual_key_required","message":"` + enterpriseMsg + `"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := StripExtraFields([]byte(body))
			if !ok {
				t.Fatalf("policy reported failure: %s", body)
			}
			assertNoAuthLeak(t, string(got))
		})
	}
}

// The neutral replacement type must not re-trigger its own detection on a
// second pass — otherwise an already-scrubbed body would keep being flagged
// and reprocessed for no reason.
func TestTheNeutralTypeDoesNotMatchItsOwnDetector(t *testing.T) {
	if _, internal := internalAuthErrorTypes[strings.ToLower(neutralAuthErrorType)]; internal {
		t.Errorf("the neutral replacement type %q matches the internal-auth-type set, so every "+
			"already-scrubbed body would be re-flagged and reprocessed", neutralAuthErrorType)
	}
}

// An UPSTREAM provider's own auth error must survive verbatim. Round 2 added
// "authentication is required" as a prefix, which would have overwritten this
// with our generic text — reintroducing the exact false-positive class round 1
// had just removed. Provider messages are preserved deliberately
// (core/providers/openai/errors.go), so this is a live path, not a hypothetical.
func TestAnUpstreamProvidersOwnAuthErrorIsNotOverwritten(t *testing.T) {
	for name, body := range map[string]string{
		"generic phrase": `{"error":{"message":"Authentication is required to access this resource","type":"invalid_request_error"}}`,
		"upstream 401":   `{"error":{"message":"Authentication is required. Check the API key configured for this deployment.","type":"authentication_error"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := StripExtraFields([]byte(body))
			if !ok {
				t.Fatalf("policy reported failure on an upstream error: %s", body)
			}
			if string(got) != body {
				t.Errorf("an UPSTREAM auth error was overwritten with our generic text, "+
					"destroying the caller's actionable detail.\n want: %s\n got:  %s", body, got)
			}
		})
	}
}

// The discriminator that makes the above safe: our own TYPE vocabulary, never
// English prose. No upstream provider mints "virtual_key_required" or
// "virtual_key_not_found" as an error type, so their presence is unambiguous
// proof of authorship in a way no shared-English phrase (e.g. "authentication
// is required...") ever was — that phrase overlap is exactly what caused the
// round-1 and round-3 false positives before #497.
func TestTypeIsWhatIdentifiesOurErrors(t *testing.T) {
	ours := ast.NewString("virtual_key_required")
	if !typeNodeIsInternalAuthError(&ours) {
		t.Error("our own internal type is no longer recognised, so it leaks again")
	}
	upstream := ast.NewString("authentication_error")
	if typeNodeIsInternalAuthError(&upstream) {
		t.Error("a generic upstream type is being claimed as ours; that overwrites " +
			"provider detail — the round-1 and round-3 defect, twice over")
	}
}
