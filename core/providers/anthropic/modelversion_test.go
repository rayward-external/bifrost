package anthropic

import (
	"context"
	"testing"

	schemas "github.com/maximhq/bifrost/core/schemas"
)

// TestParseClaudeModel pins the id shapes the parser must understand: both "4-7"
// and "4.7" separators, date-stamped ids, Bedrock inference-profile prefixes and
// ARNs, Vertex "@" suffixes, the legacy version-before-family naming, and bare
// aliases with no "claude-" segment.
func TestParseClaudeModel(t *testing.T) {
	cases := []struct {
		model      string
		wantFamily claudeFamily
		wantMajor  int
		wantMinor  int
		wantHasVer bool
	}{
		// Current naming, both separators.
		{"claude-opus-5", claudeFamilyOpus, 5, 0, true},
		{"claude-opus-4-8", claudeFamilyOpus, 4, 8, true},
		{"claude-opus-4.8", claudeFamilyOpus, 4, 8, true},
		{"claude-sonnet-5", claudeFamilySonnet, 5, 0, true},
		{"claude-sonnet-4-6", claudeFamilySonnet, 4, 6, true},
		{"claude-haiku-4-5", claudeFamilyHaiku, 4, 5, true},
		// Case-insensitive.
		{"Claude-Opus-5", claudeFamilyOpus, 5, 0, true},
		{"CLAUDE-SONNET-4-6", claudeFamilySonnet, 4, 6, true},
		// Date-stamped ids: the 8-digit stamp must never be read as a version.
		{"claude-opus-5-20260901", claudeFamilyOpus, 5, 0, true},
		{"claude-haiku-4-5-20251001", claudeFamilyHaiku, 4, 5, true},
		{"claude-opus-4-1-20250805", claudeFamilyOpus, 4, 1, true},
		// Bedrock ids: vendor segment, inference-profile prefixes, "-v1:0" suffix.
		{"anthropic.claude-haiku-4-5-20251001-v1:0", claudeFamilyHaiku, 4, 5, true},
		{"us.anthropic.claude-opus-5-20260901-v1:0", claudeFamilyOpus, 5, 0, true},
		{"eu.anthropic.claude-opus-4-8", claudeFamilyOpus, 4, 8, true},
		{"apac.anthropic.claude-sonnet-5", claudeFamilySonnet, 5, 0, true},
		{"global.anthropic.claude-sonnet-4-6", claudeFamilySonnet, 4, 6, true},
		{
			"arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-opus-5-20260901-v1:0",
			claudeFamilyOpus, 5, 0, true,
		},
		// Vertex ids.
		{"claude-opus-5@default", claudeFamilyOpus, 5, 0, true},
		{"claude-sonnet-4-5@20250929", claudeFamilySonnet, 4, 5, true},
		// Legacy naming: version precedes the family token.
		{"claude-3-5-sonnet-20241022", claudeFamilySonnet, 3, 5, true},
		{"claude-3-7-sonnet-latest", claudeFamilySonnet, 3, 7, true},
		{"claude-3-opus-20240229", claudeFamilyOpus, 3, 0, true},
		{"claude-3-haiku-20240307", claudeFamilyHaiku, 3, 0, true},
		// Bare aliases with no "claude" segment.
		{"opus-5", claudeFamilyOpus, 5, 0, true},
		{"sonnet-4-6", claudeFamilySonnet, 4, 6, true},
		// Fable / Mythos: family known, version optional.
		{"claude-fable-5", claudeFamilyFable, 5, 0, true},
		{"claude-mythos-5", claudeFamilyMythos, 5, 0, true},
		{"claude-mythos-preview", claudeFamilyMythos, 0, 0, false},
		{"anthropic.claude-mythos-5-v1", claudeFamilyMythos, 5, 0, true},
		// Family with no parseable version.
		{"claude-opus-latest", claudeFamilyOpus, 0, 0, false},
		// Unknown / defensive.
		{"", claudeFamilyUnknown, 0, 0, false},
		{"some-non-claude-model", claudeFamilyUnknown, 0, 0, false},
		{"gpt-4o-mini", claudeFamilyUnknown, 0, 0, false},
		{"claude-2.1", claudeFamilyUnknown, 0, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			got := parseClaudeModel(tc.model)
			if got.Family != tc.wantFamily || got.Major != tc.wantMajor ||
				got.Minor != tc.wantMinor || got.HasVersion != tc.wantHasVer {
				t.Errorf("parseClaudeModel(%q) = {family:%q major:%d minor:%d hasVersion:%v}, want {family:%q major:%d minor:%d hasVersion:%v}",
					tc.model, got.Family, got.Major, got.Minor, got.HasVersion,
					tc.wantFamily, tc.wantMajor, tc.wantMinor, tc.wantHasVer)
			}
		})
	}
}

// TestClaudeVersionOrdering proves the comparison is numeric, not lexical:
// "opus-5" must outrank "opus-4.8" and "opus-4.10" must outrank "opus-4.9".
func TestClaudeVersionOrdering(t *testing.T) {
	cases := []struct {
		model        string
		major, minor int
		want         bool
	}{
		{"claude-opus-5", 4, 7, true},
		{"claude-opus-5", 4, 8, true},
		{"claude-opus-4-8", 4, 7, true},
		{"claude-opus-4-7", 4, 8, false},
		{"claude-opus-4-10", 4, 9, true},  // numeric, not lexical ("10" < "9" as strings)
		{"claude-opus-4-9", 4, 10, false}, // and the converse
		{"claude-opus-5", 5, 0, true},
		{"claude-opus-6", 5, 0, true},
		{"claude-opus-latest", 4, 7, false}, // no version parsed => never claims a floor
		{"some-non-claude-model", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			if got := parseClaudeModel(tc.model).atLeast(tc.major, tc.minor); got != tc.want {
				t.Errorf("parseClaudeModel(%q).atLeast(%d,%d) = %v, want %v", tc.model, tc.major, tc.minor, got, tc.want)
			}
		})
	}
}

// TestClaudeCapabilityMatrix is the anti-version-rot guard for issue #351.
//
// It asserts every model-version predicate at once for:
//   - the models that already behaved correctly (must not regress):
//     opus 4.8, sonnet 5, fable 5, sonnet 4.6, haiku 4.5, and the older ladder;
//   - claude-opus-5, the model the substring whitelists silently mis-classified;
//   - hypothetical future majors (opus 6, sonnet 6, haiku 6, opus 5.1), which
//     must resolve correctly with NO further code change. That last group is the
//     entire point: this bug existed because a new model shipped.
//
// supportsFastMode is the one column that is NOT forward-inclusive: it is a
// closed set of {4.6, 4.7, 4.8} because there a wrong "true" causes a 400 rather
// than preventing one. See SupportsFastMode and
// TestSupportsFastMode_BoundedWindow.
func TestClaudeCapabilityMatrix(t *testing.T) {
	const (
		newGen = ComputerUseGen20251124
		oldGen = ComputerUseGen20250124
	)

	cases := []struct {
		model             string
		opus47Plus        bool
		sonnet5Plus       bool
		fableFamily       bool
		adaptiveOnly      bool
		supportsAdaptive  bool
		supportsEffort    bool
		supportsNativeEff bool
		supportsFastMode  bool
		midConvSystem     bool
		dynamicWebSearch  bool
		computerUseGen    string
		textEditorGen     string
	}{
		// --- the regression that motivated the fix -------------------------
		// supportsFastMode is false here on purpose: fast mode is documented for
		// Opus 4.6/4.7/4.8 only, and forwarding speed:"fast" to a model that
		// lacks it is itself a 400.
		{
			model:      "claude-opus-5",
			opus47Plus: true, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: true, supportsAdaptive: true, supportsEffort: true,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: true,
			dynamicWebSearch: true, computerUseGen: newGen, textEditorGen: newGen,
		},
		{
			model:      "claude-opus-5-20260901",
			opus47Plus: true, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: true, supportsAdaptive: true, supportsEffort: true,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: true,
			dynamicWebSearch: true, computerUseGen: newGen, textEditorGen: newGen,
		},
		{
			model:      "us.anthropic.claude-opus-5-20260901-v1:0",
			opus47Plus: true, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: true, supportsAdaptive: true, supportsEffort: true,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: true,
			dynamicWebSearch: true, computerUseGen: newGen, textEditorGen: newGen,
		},
		{
			model:      "claude-opus-5@default",
			opus47Plus: true, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: true, supportsAdaptive: true, supportsEffort: true,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: true,
			dynamicWebSearch: true, computerUseGen: newGen, textEditorGen: newGen,
		},

		// --- must not regress: models that behave correctly today ----------
		{
			model:      "claude-opus-4-8",
			opus47Plus: true, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: true, supportsAdaptive: true, supportsEffort: true,
			supportsNativeEff: false, supportsFastMode: true, midConvSystem: true,
			dynamicWebSearch: true, computerUseGen: newGen, textEditorGen: newGen,
		},
		{
			model:      "claude-opus-4.8-20260601",
			opus47Plus: true, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: true, supportsAdaptive: true, supportsEffort: true,
			supportsNativeEff: false, supportsFastMode: true, midConvSystem: true,
			dynamicWebSearch: true, computerUseGen: newGen, textEditorGen: newGen,
		},
		{
			model:      "claude-opus-4-7",
			opus47Plus: true, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: true, supportsAdaptive: true, supportsEffort: true,
			supportsNativeEff: false, supportsFastMode: true, midConvSystem: false,
			dynamicWebSearch: true, computerUseGen: newGen, textEditorGen: newGen,
		},
		{
			model:      "claude-sonnet-5",
			opus47Plus: false, sonnet5Plus: true, fableFamily: false,
			adaptiveOnly: true, supportsAdaptive: true, supportsEffort: true,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: false,
			dynamicWebSearch: true, computerUseGen: newGen, textEditorGen: newGen,
		},
		{
			model:      "global.anthropic.claude-sonnet-5",
			opus47Plus: false, sonnet5Plus: true, fableFamily: false,
			adaptiveOnly: true, supportsAdaptive: true, supportsEffort: true,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: false,
			dynamicWebSearch: true, computerUseGen: newGen, textEditorGen: newGen,
		},
		{
			model:      "claude-fable-5",
			opus47Plus: false, sonnet5Plus: false, fableFamily: true,
			adaptiveOnly: true, supportsAdaptive: true, supportsEffort: true,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: true,
			dynamicWebSearch: true, computerUseGen: newGen, textEditorGen: newGen,
		},
		{
			model:      "claude-mythos-preview",
			opus47Plus: false, sonnet5Plus: false, fableFamily: true,
			adaptiveOnly: true, supportsAdaptive: true, supportsEffort: true,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: true,
			dynamicWebSearch: true, computerUseGen: newGen, textEditorGen: newGen,
		},
		{
			model:      "claude-sonnet-4-6",
			opus47Plus: false, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: false, supportsAdaptive: true, supportsEffort: true,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: false,
			dynamicWebSearch: true, computerUseGen: newGen, textEditorGen: newGen,
		},
		{
			model:      "claude-opus-4-6",
			opus47Plus: false, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: false, supportsAdaptive: true, supportsEffort: true,
			supportsNativeEff: true, supportsFastMode: true, midConvSystem: false,
			dynamicWebSearch: true, computerUseGen: newGen, textEditorGen: newGen,
		},
		{
			model:      "claude-opus-4-5",
			opus47Plus: false, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: false, supportsAdaptive: false, supportsEffort: true,
			supportsNativeEff: true, supportsFastMode: false, midConvSystem: false,
			dynamicWebSearch: false, computerUseGen: newGen, textEditorGen: newGen,
		},
		{
			model:      "claude-haiku-4-5",
			opus47Plus: false, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: false, supportsAdaptive: false, supportsEffort: false,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: false,
			dynamicWebSearch: false, computerUseGen: oldGen, textEditorGen: oldGen,
		},
		{
			model:      "anthropic.claude-haiku-4-5-20251001-v1:0",
			opus47Plus: false, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: false, supportsAdaptive: false, supportsEffort: false,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: false,
			dynamicWebSearch: false, computerUseGen: oldGen, textEditorGen: oldGen,
		},
		{
			model:      "claude-sonnet-4-5-20250929",
			opus47Plus: false, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: false, supportsAdaptive: false, supportsEffort: false,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: false,
			dynamicWebSearch: false, computerUseGen: oldGen, textEditorGen: newGen,
		},
		{
			model:      "claude-opus-4-1-20250805",
			opus47Plus: false, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: false, supportsAdaptive: false, supportsEffort: false,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: false,
			dynamicWebSearch: false, computerUseGen: oldGen, textEditorGen: oldGen,
		},
		{
			model:      "claude-3-5-sonnet-20241022",
			opus47Plus: false, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: false, supportsAdaptive: false, supportsEffort: false,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: false,
			dynamicWebSearch: false, computerUseGen: oldGen, textEditorGen: oldGen,
		},
		{
			model:      "claude-3-opus-20240229",
			opus47Plus: false, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: false, supportsAdaptive: false, supportsEffort: false,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: false,
			dynamicWebSearch: false, computerUseGen: oldGen, textEditorGen: oldGen,
		},

		// --- future majors: must resolve with NO code change ---------------
		// Note supportsFastMode: false throughout this group — it is the single
		// column that must NOT be inherited by an unreleased model.
		{
			model:      "claude-opus-6",
			opus47Plus: true, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: true, supportsAdaptive: true, supportsEffort: true,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: true,
			dynamicWebSearch: true, computerUseGen: newGen, textEditorGen: newGen,
		},
		{
			model:      "claude-opus-5-1-20270301",
			opus47Plus: true, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: true, supportsAdaptive: true, supportsEffort: true,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: true,
			dynamicWebSearch: true, computerUseGen: newGen, textEditorGen: newGen,
		},
		{
			model:      "claude-sonnet-6",
			opus47Plus: false, sonnet5Plus: true, fableFamily: false,
			adaptiveOnly: true, supportsAdaptive: true, supportsEffort: true,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: false,
			dynamicWebSearch: true, computerUseGen: newGen, textEditorGen: newGen,
		},
		{
			model:      "us.anthropic.claude-sonnet-6-20270301-v1:0",
			opus47Plus: false, sonnet5Plus: true, fableFamily: false,
			adaptiveOnly: true, supportsAdaptive: true, supportsEffort: true,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: false,
			dynamicWebSearch: true, computerUseGen: newGen, textEditorGen: newGen,
		},
		// Haiku has never carried these surfaces; a future Haiku must not
		// inherit them from an Opus/Sonnet-shaped version number.
		{
			model:      "claude-haiku-6",
			opus47Plus: false, sonnet5Plus: false, fableFamily: false,
			adaptiveOnly: false, supportsAdaptive: false, supportsEffort: false,
			supportsNativeEff: false, supportsFastMode: false, midConvSystem: false,
			dynamicWebSearch: false, computerUseGen: oldGen, textEditorGen: oldGen,
		},

		// --- defensive ------------------------------------------------------
		{
			model:          "",
			computerUseGen: oldGen, textEditorGen: oldGen,
		},
		{
			model:          "some-non-claude-model",
			computerUseGen: oldGen, textEditorGen: oldGen,
		},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			checkBool(t, "IsOpus47Plus", tc.model, IsOpus47Plus(tc.model), tc.opus47Plus)
			checkBool(t, "IsSonnet5Plus", tc.model, IsSonnet5Plus(tc.model), tc.sonnet5Plus)
			checkBool(t, "IsFableFamily", tc.model, IsFableFamily(tc.model), tc.fableFamily)
			checkBool(t, "IsAdaptiveOnlyThinkingModel", tc.model, IsAdaptiveOnlyThinkingModel(tc.model), tc.adaptiveOnly)
			checkBool(t, "SupportsAdaptiveThinking", tc.model, SupportsAdaptiveThinking(tc.model), tc.supportsAdaptive)
			checkBool(t, "SupportsEffortParameter", tc.model, SupportsEffortParameter(tc.model), tc.supportsEffort)
			checkBool(t, "SupportsNativeEffort", tc.model, SupportsNativeEffort(tc.model), tc.supportsNativeEff)
			checkBool(t, "SupportsFastMode", tc.model, SupportsFastMode(tc.model), tc.supportsFastMode)
			checkBool(t, "SupportsDynamicWebSearch", tc.model, SupportsDynamicWebSearch(tc.model), tc.dynamicWebSearch)
			checkBool(t, "SupportsMidConversationSystem", tc.model,
				SupportsMidConversationSystem(schemas.Anthropic, tc.model), tc.midConvSystem)

			if got := ComputerUseGeneration(tc.model); got != tc.computerUseGen {
				t.Errorf("ComputerUseGeneration(%q) = %q, want %q", tc.model, got, tc.computerUseGen)
			}
			if got := TextEditorGeneration(tc.model); got != tc.textEditorGen {
				t.Errorf("TextEditorGeneration(%q) = %q, want %q", tc.model, got, tc.textEditorGen)
			}
		})
	}
}

func checkBool(t *testing.T, fn, model string, got, want bool) {
	t.Helper()
	if got != want {
		t.Errorf("%s(%q) = %v, want %v", fn, model, got, want)
	}
}

// TestSupportsFastMode_BoundedWindow guards the one predicate in this file that
// is deliberately NOT forward-inclusive.
//
// Every other gate uses a ">= floor" because returning false is what causes a
// 400 there. Fast mode inverts that: false merely strips speed:"fast" (the
// request still succeeds, minus the research-preview speedup) while true
// forwards speed:"fast" AND the fast-mode-2026-02-01 beta header, which is a
// hard 400 on any model that does not have it. Anthropic documents fast mode
// for Opus 4.6/4.7/4.8 only, and — unlike adaptive thinking or output_config —
// there is no datasheet flag to verify a newer model against.
//
// So a ">= 4.6" floor would take a *working* claude-opus-5 request and break it.
// This test fails against that floor.
func TestSupportsFastMode_BoundedWindow(t *testing.T) {
	supported := []string{
		"claude-opus-4-6",
		"claude-opus-4.6-20250514",
		"claude-opus-4-7",
		"claude-opus-4.7-20260401",
		"claude-opus-4-8",
		"claude-opus-4.8-20260601",
		// Prefixed / suffixed forms of the same three versions still resolve.
		"global.anthropic.claude-opus-4-6",
		"us.anthropic.claude-opus-4-8-20260601-v1:0",
		"claude-opus-4-8@default",
	}
	// Everything here is Opus at a version ABOVE the fast-mode window. A
	// forward-inclusive ">= 4.6" implementation returns true for every one of
	// them, so each is a real regression trip-wire.
	unsupportedAboveWindow := []string{
		"claude-opus-5",
		"claude-opus-5-20260901",
		"claude-opus-5@default",
		"us.anthropic.claude-opus-5-20260901-v1:0",
		"claude-opus-5-1-20270301",
		"claude-opus-6",
		"claude-opus-4-9",
		"claude-opus-4-10",
	}
	// Below the window, or a family that never had fast mode at all.
	unsupportedOther := []string{
		"claude-opus-4-5",
		"claude-opus-4-1",
		"claude-opus-latest",
		"claude-sonnet-4-6",
		"claude-sonnet-5",
		"claude-haiku-4-5",
		"claude-fable-5",
		"claude-mythos-5",
		"claude-mythos-preview",
		"",
		"some-non-claude-model",
	}

	for _, model := range supported {
		t.Run("supported/"+model, func(t *testing.T) {
			if !SupportsFastMode(model) {
				t.Errorf("SupportsFastMode(%q) = false, want true (Opus 4.6/4.7/4.8 are documented fast-mode models)", model)
			}
		})
	}
	for _, model := range append(append([]string{}, unsupportedAboveWindow...), unsupportedOther...) {
		t.Run("unsupported/"+model, func(t *testing.T) {
			if SupportsFastMode(model) {
				t.Errorf("SupportsFastMode(%q) = true, want false; forwarding speed:\"fast\" to a model outside the documented window is a 400", model)
			}
		})
	}

	// The consequence, not just the predicate: speed must actually be stripped
	// and the beta header must actually be withheld.
	t.Run("speed_stripped_and_header_withheld", func(t *testing.T) {
		cases := []struct {
			model    string
			wantKept bool
		}{
			{"claude-opus-4-8", true},
			{"claude-opus-4-6", true},
			{"claude-opus-5", false},
			{"claude-opus-6", false},
			{"claude-sonnet-5", false},
		}
		for _, tc := range cases {
			t.Run(tc.model, func(t *testing.T) {
				req := &AnthropicMessageRequest{
					Model: tc.model,
					Speed: schemas.Ptr("fast"),
				}
				stripUnsupportedAnthropicFields(req, schemas.Anthropic, tc.model)
				if tc.wantKept && req.Speed == nil {
					t.Errorf("expected speed to survive for %s", tc.model)
				}
				if !tc.wantKept && req.Speed != nil {
					t.Errorf("expected speed to be stripped for %s, got %q", tc.model, *req.Speed)
				}

				// Beta header path is gated independently — check it too, on a
				// request that still carries speed.
				hdrReq := &AnthropicMessageRequest{
					Model: tc.model,
					Speed: schemas.Ptr("fast"),
				}
				ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
				defer cancel()
				if err := AddMissingBetaHeadersToContext(ctx, hdrReq, schemas.Anthropic); err != nil {
					t.Fatalf("AddMissingBetaHeadersToContext: %v", err)
				}
				var headers []string
				if extra, ok := ctx.Value(schemas.BifrostContextKeyExtraHeaders).(map[string][]string); ok {
					headers = extra[AnthropicBetaHeader]
				}
				found := false
				for _, h := range headers {
					if h == AnthropicFastModeBetaHeader {
						found = true
						break
					}
				}
				if found != tc.wantKept {
					t.Errorf("fast-mode beta header present = %v for %s, want %v (headers: %v)",
						found, tc.model, tc.wantKept, headers)
				}
			})
		}
	})
}

// TestMapBifrostEffortToAnthropic pins the effort vocabulary onto Anthropic's
// real accepted set, measured live on 2026-07-25:
//
//	output_config.effort: Input should be 'low', 'medium', 'high', 'xhigh' or 'max'
//
// OpenAI's "minimal" and "none" are NOT in that set and must never be forwarded
// verbatim.
func TestMapBifrostEffortToAnthropic(t *testing.T) {
	cases := []struct {
		effort string
		want   string
	}{
		// Pass-through for the accepted set.
		{"low", "low"},
		{"medium", "medium"},
		{"high", "high"},
		{"xhigh", "xhigh"},
		{"max", "max"},
		// OpenAI-only vocabulary floors to the minimum Anthropic accepts.
		{"minimal", "low"},
		{"none", "low"},
		// Normalisation.
		{"HIGH", "high"},
		{"  max  ", "max"},
		{"Minimal", "low"},
		// Unknown input still lands inside the accepted set rather than 400ing.
		{"", "medium"},
		{"ultra", "medium"},
		{"very-high", "medium"},
	}

	for _, tc := range cases {
		t.Run(tc.effort, func(t *testing.T) {
			got := MapBifrostEffortToAnthropic(tc.effort)
			if got != tc.want {
				t.Errorf("MapBifrostEffortToAnthropic(%q) = %q, want %q", tc.effort, got, tc.want)
			}
			if _, ok := anthropicEffortLevels[got]; !ok {
				t.Errorf("MapBifrostEffortToAnthropic(%q) = %q, which is not an Anthropic-accepted effort level", tc.effort, got)
			}
		})
	}
}

// TestToAnthropicChatRequest_Opus5_StripsSamplingParams is the end-to-end
// reproduction of the first half of #351: through the gateway, claude-opus-5
// returned 400 on temperature and 400 on top_p because IsAdaptiveOnlyThinkingModel
// was false, so chat.go forwarded parameters Anthropic has deprecated for the model.
//
// top_p needs its own case rather than riding along with temperature. chat.go
// picks ONE of the two ("prefer temperature") — so with temperature set, top_p
// is never populated for any model and "result.TopP == nil" would hold even if
// the adaptive-only gate were deleted. The topPOnly variant is the one that
// actually exercises the top_p branch; the guard sub-test below proves that by
// showing a non-adaptive model DOES receive each param from the same input.
func TestToAnthropicChatRequest_Opus5_StripsSamplingParams(t *testing.T) {
	// buildReq constructs a request carrying exactly the sampling params named.
	buildReq := func(model string, withTemp, withTopP bool) *schemas.BifrostChatRequest {
		params := &schemas.ChatParameters{
			ExtraParams: map[string]interface{}{"top_k": 40},
		}
		if withTemp {
			params.Temperature = schemas.Ptr(0.7)
		}
		if withTopP {
			params.TopP = schemas.Ptr(0.9)
		}
		return &schemas.BifrostChatRequest{
			Provider: schemas.Anthropic,
			Model:    model,
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hi")}},
			},
			Params: params,
		}
	}

	variants := []struct {
		name               string
		withTemp, withTopP bool
	}{
		{"both", true, true},
		{"tempOnly", true, false},
		// The only shape in which top_p reaches the wire at all.
		{"topPOnly", false, true},
	}

	for _, model := range []string{"claude-opus-5", "claude-opus-5-20260901", "claude-opus-6", "claude-sonnet-6"} {
		for _, v := range variants {
			t.Run(model+"/"+v.name, func(t *testing.T) {
				ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
				defer cancel()
				result, err := ToAnthropicChatRequest(ctx, buildReq(model, v.withTemp, v.withTopP))
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if result.Temperature != nil {
					t.Errorf("expected Temperature to be dropped for %s, got %v", model, *result.Temperature)
				}
				if result.TopP != nil {
					t.Errorf("expected TopP to be dropped for %s, got %v", model, *result.TopP)
				}
				if result.TopK != nil {
					t.Errorf("expected TopK to be dropped for %s, got %v", model, *result.TopK)
				}
			})
		}
	}

	// Counter-case: the assertions above are only meaningful if these params
	// are otherwise forwarded. A non-adaptive model must receive each one from
	// the exact same inputs — otherwise "dropped" is indistinguishable from
	// "never set".
	t.Run("nonAdaptiveModelStillReceivesThem", func(t *testing.T) {
		const model = "claude-sonnet-4-5"

		ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
		defer cancel()

		withTemp, err := ToAnthropicChatRequest(ctx, buildReq(model, true, true))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if withTemp.Temperature == nil || *withTemp.Temperature != 0.7 {
			t.Errorf("expected Temperature 0.7 to be forwarded for %s, got %v", model, withTemp.Temperature)
		}
		if withTemp.TopK == nil || *withTemp.TopK != 40 {
			t.Errorf("expected TopK 40 to be forwarded for %s, got %v", model, withTemp.TopK)
		}

		topPOnly, err := ToAnthropicChatRequest(ctx, buildReq(model, false, true))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if topPOnly.TopP == nil || *topPOnly.TopP != 0.9 {
			t.Errorf("expected TopP 0.9 to be forwarded for %s, got %v", model, topPOnly.TopP)
		}
	})
}

// TestToAnthropicChatRequest_Opus5_ReasoningEffortUsesAdaptive is the end-to-end
// reproduction of the second half of #351: reasoning_effort returned 500
// ("failed to convert bifrost request to the expected provider request body")
// because SupportsEffortParameter was false for claude-opus-5, so the request
// took the legacy thinking:{type:"enabled",budget_tokens:N} path that these
// models reject.
func TestToAnthropicChatRequest_Opus5_ReasoningEffortUsesAdaptive(t *testing.T) {
	cases := []struct {
		model      string
		effort     string
		wantEffort string
	}{
		{"claude-opus-5", "high", "high"},
		{"claude-opus-5", "xhigh", "xhigh"},
		{"claude-opus-5", "max", "max"},
		// OpenAI-only vocabulary must be translated, never forwarded verbatim.
		{"claude-opus-5", "minimal", "low"},
		{"claude-opus-5-20260901", "medium", "medium"},
		{"us.anthropic.claude-opus-5-20260901-v1:0", "low", "low"},
		// Future majors, no code change.
		{"claude-opus-6", "high", "high"},
		{"claude-sonnet-6", "high", "high"},
		// Models that already worked must keep working.
		{"claude-opus-4-8", "high", "high"},
		{"claude-sonnet-5", "high", "high"},
		{"claude-fable-5", "high", "high"},
	}

	for _, tc := range cases {
		t.Run(tc.model+"/"+tc.effort, func(t *testing.T) {
			effort := tc.effort
			bifrostReq := &schemas.BifrostChatRequest{
				Provider: schemas.Anthropic,
				Model:    tc.model,
				Input: []schemas.ChatMessage{
					{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("think")}},
				},
				Params: &schemas.ChatParameters{
					MaxCompletionTokens: schemas.Ptr(8192),
					Reasoning:           &schemas.ChatReasoning{Effort: &effort},
				},
			}

			ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
			defer cancel()
			result, err := ToAnthropicChatRequest(ctx, bifrostReq)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Thinking == nil {
				t.Fatal("expected Thinking to be set")
			}
			if result.Thinking.Type != "adaptive" {
				t.Errorf("expected thinking type %q, got %q", "adaptive", result.Thinking.Type)
			}
			if result.Thinking.BudgetTokens != nil {
				t.Errorf("expected no budget_tokens on an adaptive-only model, got %d", *result.Thinking.BudgetTokens)
			}
			if result.OutputConfig == nil || result.OutputConfig.Effort == nil {
				t.Fatalf("expected output_config.effort to be set for %s", tc.model)
			}
			if *result.OutputConfig.Effort != tc.wantEffort {
				t.Errorf("output_config.effort = %q, want %q", *result.OutputConfig.Effort, tc.wantEffort)
			}
		})
	}
}

// TestFilterRequestForProvider_Opus5_KeepsEffort guards the SECOND gate the
// effort value has to survive. ToAnthropicChatRequest setting
// output_config.effort is not sufficient: stripUnsupportedAnthropicFields runs
// afterwards and deletes output_config.effort for any model
// SupportsEffortParameter rejects — which, before the fix, included
// claude-opus-5. The result was a silently effort-less request.
//
// This exercises the strip function itself rather than restating the predicate,
// and asserts the negative half too (a model that genuinely lacks the knob must
// still have it removed), so the test cannot be satisfied by a
// SupportsEffortParameter that just returns true.
func TestFilterRequestForProvider_Opus5_KeepsEffort(t *testing.T) {
	cases := []struct {
		model    string
		wantKept bool
	}{
		// The #351 regression plus its future-major siblings.
		{"claude-opus-5", true},
		{"claude-opus-5-20260901", true},
		{"claude-opus-6", true},
		{"claude-sonnet-6", true},
		// Already worked; must not regress.
		{"claude-opus-4-8", true},
		{"claude-opus-4-5", true},
		{"claude-sonnet-5", true},
		{"claude-sonnet-4-6", true},
		{"claude-fable-5", true},
		// Genuinely unsupported — effort MUST still be stripped. Without these
		// the test would pass against a predicate hard-coded to true.
		{"claude-haiku-4-5", false},
		{"claude-haiku-6", false},
		{"claude-sonnet-4-5", false},
		{"claude-opus-4-1", false},
		{"claude-3-5-sonnet-20241022", false},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			req := &AnthropicMessageRequest{
				Model: tc.model,
				OutputConfig: &AnthropicOutputConfig{
					Effort: schemas.Ptr("high"),
				},
			}
			stripUnsupportedAnthropicFields(req, schemas.Anthropic, tc.model)

			gotEffort := req.OutputConfig != nil && req.OutputConfig.Effort != nil
			if gotEffort != tc.wantKept {
				t.Fatalf("after stripUnsupportedAnthropicFields(%q): output_config.effort present = %v, want %v",
					tc.model, gotEffort, tc.wantKept)
			}
			if tc.wantKept && *req.OutputConfig.Effort != "high" {
				t.Errorf("output_config.effort = %q, want %q", *req.OutputConfig.Effort, "high")
			}
		})
	}
}
