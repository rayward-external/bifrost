// Package complexity provides request-complexity scoring for governance routing.
package complexity

import (
	"slices"

	"github.com/maximhq/bifrost/framework/configstore"
)

// ComplexityInput is the normalized input for the analyzer.
// The caller is responsible for extracting text from request payloads.
type ComplexityInput struct {
	LastUserText   string   // last user message text
	PriorUserTexts []string // previous user message texts (up to 10)
	SystemText     string   // concatenated system/developer prompt text
}

// ComplexityResult holds the computed complexity scores and tier classification.
type ComplexityResult struct {
	Score     float64
	Tier      string
	WordCount int
}

const (
	TierSimple    = "SIMPLE"
	TierMedium    = "MEDIUM"
	TierComplex   = "COMPLEX"
	TierReasoning = "REASONING"
)

const (
	simpleMediumBoundary     = 0.15
	mediumComplexBoundary    = 0.35
	complexReasoningBoundary = 0.60
)

// TierBoundaries defines the score thresholds for tier classification.
type TierBoundaries = configstore.ComplexityTierBoundaries

// EditableKeywordConfig is the user-facing subset of analyzer keyword lists.
type EditableKeywordConfig = configstore.ComplexityEditableKeywordConfig

// AnalyzerConfig is the runtime configuration for the complexity analyzer.
type AnalyzerConfig = configstore.ComplexityAnalyzerConfig

// KeywordConfig is the full internal keyword set used by the compiled matcher.
type KeywordConfig struct {
	CodeKeywords            []string
	StrongReasoningKeywords []string
	TechnicalKeywords       []string
	SimpleKeywords          []string
	ContinuationPhrases     []string
}

// DefaultTierBoundaries returns the built-in classification thresholds.
//
// Upstream's shared ComplexityTierBoundaries only carries the SimpleMedium/
// MediumComplex cut points now (its own semantic classifier superseded the
// lexical one this fork's analyzer still uses); the fork's 4-tier scheme
// keeps its REASONING cutoff as a local, non-persisted constant
// (complexReasoningBoundary, used directly in analyzer.go's classifyTier)
// instead of a field on the shared struct.
func DefaultTierBoundaries() TierBoundaries {
	return TierBoundaries{
		SimpleMedium:  simpleMediumBoundary,
		MediumComplex: mediumComplexBoundary,
	}
}

// DefaultEditableKeywordConfig returns the user-visible default keyword lists.
//
// Upstream's shared ComplexityEditableKeywordConfig collapsed the legacy
// 4-list shape (code/reasoning/technical/simple) into a canonical 3-list one
// (simple/medium/complex) — mirroring its own legacy->canonical migration in
// ComplexityEditableKeywordConfig.UnmarshalJSON, MediumKeywords carries both
// code and technical terms and ComplexKeywords carries the reasoning terms.
func DefaultEditableKeywordConfig() EditableKeywordConfig {
	return EditableKeywordConfig{
		SimpleKeywords:  cloneStringSlice(simpleKeywords),
		MediumKeywords:  append(cloneStringSlice(codeKeywords), technicalKeywords...),
		ComplexKeywords: cloneStringSlice(strongReasoningKeywords),
	}
}

// DefaultAnalyzerConfig returns the built-in analyzer config.
func DefaultAnalyzerConfig() AnalyzerConfig {
	return AnalyzerConfig{
		TierBoundaries: DefaultTierBoundaries(),
		Keywords:       DefaultEditableKeywordConfig(),
	}
}

// ValidateAndNormalize normalizes and validates analyzer config.
func ValidateAndNormalize(cfg *AnalyzerConfig) (*AnalyzerConfig, error) {
	if cfg == nil {
		defaults := DefaultAnalyzerConfig()
		return &defaults, nil
	}
	normalized := cfg.Normalized()
	if err := normalized.Validate(); err != nil {
		return nil, err
	}
	return &normalized, nil
}

// defaultMediumKeywords is DefaultEditableKeywordConfig's own MediumKeywords value, computed
// once so mergeEditableKeywordsOntoDefaults can tell "this is just the resolved default" (Validate
// requires MediumKeywords to be non-empty, so DefaultAnalyzerConfig must populate it) apart from
// a caller's real override, without which every analyzer — including the built-in, uncustomized
// one — would collapse code and technical keywords into the same merged list.
var defaultMediumKeywords = append(cloneStringSlice(codeKeywords), technicalKeywords...)

func mergeEditableKeywordsOntoDefaults(editable EditableKeywordConfig) KeywordConfig {
	keywords := defaultFullKeywordConfig()
	// Canonical shape merges code+technical into one editable list (see
	// DefaultEditableKeywordConfig) — apply the same override to both internal buckets so a
	// genuinely customized MediumKeywords list isn't silently half-ignored. A MediumKeywords
	// that still matches the resolved default verbatim is not a real override (see
	// defaultMediumKeywords above); leave the two buckets at their separate built-in defaults.
	if len(editable.MediumKeywords) > 0 && !slices.Equal(editable.MediumKeywords, defaultMediumKeywords) {
		keywords.CodeKeywords = cloneStringSlice(editable.MediumKeywords)
		keywords.TechnicalKeywords = cloneStringSlice(editable.MediumKeywords)
	}
	if len(editable.ComplexKeywords) > 0 {
		keywords.StrongReasoningKeywords = cloneStringSlice(editable.ComplexKeywords)
	}
	if len(editable.SimpleKeywords) > 0 {
		keywords.SimpleKeywords = cloneStringSlice(editable.SimpleKeywords)
	}
	return keywords
}

func defaultFullKeywordConfig() KeywordConfig {
	return KeywordConfig{
		CodeKeywords:            cloneStringSlice(codeKeywords),
		StrongReasoningKeywords: cloneStringSlice(strongReasoningKeywords),
		TechnicalKeywords:       cloneStringSlice(technicalKeywords),
		SimpleKeywords:          cloneStringSlice(simpleKeywords),
		ContinuationPhrases:     cloneStringSlice(continuationPhrases),
	}
}

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}
