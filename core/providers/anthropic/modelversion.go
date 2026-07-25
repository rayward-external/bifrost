package anthropic

import (
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Claude model-version parsing
//
// Every capability predicate in this package used to be a substring whitelist
// ("does the id contain 4-7 or 4-8?"). That shape is version-rot by
// construction: the whitelist is written against the models that exist on the
// day it is written, and the *next* model — which is exactly the one that needs
// the newest request surface — falls through to the "old model" branch.
//
// Concretely, IsOpus47Plus("claude-opus-5") returned false, so Opus 5 kept
// temperature/top_p/top_k (Anthropic 400s: "`temperature` is deprecated for
// this model") and took the legacy thinking:{type:"enabled",budget_tokens:N}
// path (Anthropic 400s: 'use "thinking.type.adaptive" and
// "output_config.effort"'), which surfaced through Bifrost as a 500
// "failed to convert bifrost request to the expected provider request body".
//
// The fix is to parse (family, major.minor) out of the model id once and
// compare numerically, so "opus-5" > "opus-4.8" falls out of the arithmetic and
// a new Opus/Sonnet major is handled with no code change.
//
// Why not read the capability flag from the datasheet instead?
// getbifrost.ai/datasheet does publish supports_adaptive_thinking /
// supports_mid_conversation_system / supports_output_config per model, and that
// would be the better source of truth. It is not reachable from here: the
// datasheet Store lives in framework/modelcatalog/datasheet, and the framework
// module *depends on* core (framework/go.mod requires
// github.com/maximhq/bifrost/core), so core/providers/anthropic importing it
// would invert the module dependency. Nothing in core is handed a capability
// lookup either — these predicates are called from pure request-conversion
// functions that only receive (ctx, model). Wiring a capability port down into
// core is a much larger refactor than this bug warrants, so version parsing
// stays the mechanism and the datasheet is used only to *verify* the ranges
// below.
// ---------------------------------------------------------------------------

// claudeFamily is the Claude model line a model id belongs to.
type claudeFamily string

const (
	claudeFamilyUnknown claudeFamily = ""
	claudeFamilyOpus    claudeFamily = "opus"
	claudeFamilySonnet  claudeFamily = "sonnet"
	claudeFamilyHaiku   claudeFamily = "haiku"
	claudeFamilyFable   claudeFamily = "fable"
	claudeFamilyMythos  claudeFamily = "mythos"
)

// claudeFamilyTokens maps a bare id token to its family.
var claudeFamilyTokens = map[string]claudeFamily{
	"opus":   claudeFamilyOpus,
	"sonnet": claudeFamilySonnet,
	"haiku":  claudeFamilyHaiku,
	"fable":  claudeFamilyFable,
	"mythos": claudeFamilyMythos,
}

// maxVersionTokenDigits bounds how many digits a token may have and still be
// read as a version component. Version components are 1-2 digits ("4", "8",
// "10"); anything longer is a release date ("20251001"), an ARN account id, or
// a build stamp and must not be mistaken for a version.
const maxVersionTokenDigits = 2

// claudeModelVersion is the parsed identity of a Claude model id: which family
// it belongs to and, when the id carries one, its major.minor version.
type claudeModelVersion struct {
	Family claudeFamily
	Major  int
	Minor  int
	// HasVersion is false for ids that name a family but no numeric version
	// (e.g. "claude-mythos-preview"). Every ordered comparison returns false in
	// that case, so an unversioned id never accidentally claims a newer
	// model's request surface.
	HasVersion bool
}

// atLeast reports whether the parsed version is >= major.minor.
// Always false when the id carries no version.
func (v claudeModelVersion) atLeast(major, minor int) bool {
	if !v.HasVersion {
		return false
	}
	if v.Major != major {
		return v.Major > major
	}
	return v.Minor >= minor
}

// isFamilyAtLeast reports whether the model is in family f at version >= major.minor.
func (v claudeModelVersion) isFamilyAtLeast(f claudeFamily, major, minor int) bool {
	return v.Family == f && v.atLeast(major, minor)
}

// isFableFamily reports whether the model is in the Fable/Mythos line, which is
// versioned independently of the Opus/Sonnet/Haiku ladder.
func (v claudeModelVersion) isFableFamily() bool {
	return v.Family == claudeFamilyFable || v.Family == claudeFamilyMythos
}

// parseClaudeModel extracts (family, major.minor) from any Claude model id
// Bifrost can be handed. It is total: unrecognised input yields the zero value,
// whose every predicate is false.
//
// Handled id shapes:
//
//	claude-opus-5                                       -> opus 5.0
//	claude-opus-4-8 / claude-opus-4.8                   -> opus 4.8   (both separators)
//	claude-haiku-4-5-20251001                           -> haiku 4.5  (date suffix ignored)
//	anthropic.claude-haiku-4-5-20251001-v1:0            -> haiku 4.5  (Bedrock id)
//	us.anthropic.claude-opus-4-8-...                    -> opus 4.8   (inference profile)
//	global.anthropic.claude-sonnet-5                    -> sonnet 5.0
//	arn:aws:bedrock:...:inference-profile/us.anthropic.claude-opus-5-... -> opus 5.0
//	claude-sonnet-4-5@20250929 / claude-opus-5@default  -> sonnet 4.5 / opus 5.0 (Vertex)
//	claude-3-5-sonnet-20241022                          -> sonnet 3.5 (legacy: version precedes family)
//	claude-3-opus-20240229                              -> opus 3.0   (legacy)
//	opus-5                                              -> opus 5.0   (bare alias)
//	claude-fable-5 / claude-mythos-preview              -> fable 5.0 / mythos (unversioned)
func parseClaudeModel(model string) claudeModelVersion {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return claudeModelVersion{}
	}

	// Drop provider routing metadata ahead of the model name: Bedrock
	// inference-profile prefixes ("us.", "eu.", "apac.", "global."), the
	// "anthropic." vendor segment, and full ARNs. Everything before the first
	// "claude" is routing, never version. Bare aliases with no "claude"
	// segment ("opus-5") are parsed as-is.
	if idx := strings.Index(m, "claude"); idx > 0 {
		m = m[idx:]
	}

	// Split on every non-alphanumeric rune, so "-", ".", "_", "@", ":" and "/"
	// are all separators. This is what makes "4-7" and "4.7" parse identically.
	tokens := strings.FieldsFunc(m, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})

	familyIdx := -1
	family := claudeFamilyUnknown
	for i, tok := range tokens {
		if f, ok := claudeFamilyTokens[tok]; ok {
			familyIdx, family = i, f
			break
		}
	}
	if familyIdx < 0 {
		return claudeModelVersion{}
	}

	// Current naming puts the version after the family: claude-opus-4-8,
	// claude-sonnet-5. Take the leading run of version-shaped tokens.
	parts := make([]int, 0, 2)
	for i := familyIdx + 1; i < len(tokens) && len(parts) < 2; i++ {
		n, ok := versionComponent(tokens[i])
		if !ok {
			break
		}
		parts = append(parts, n)
	}

	// Legacy naming puts it before: claude-3-5-sonnet, claude-3-opus. Walk
	// backwards from the family token and reverse what we collected.
	if len(parts) == 0 {
		for i := familyIdx - 1; i >= 0 && len(parts) < 2; i-- {
			n, ok := versionComponent(tokens[i])
			if !ok {
				break
			}
			parts = append([]int{n}, parts...)
		}
	}

	v := claudeModelVersion{Family: family}
	if len(parts) > 0 {
		v.HasVersion = true
		v.Major = parts[0]
		if len(parts) > 1 {
			v.Minor = parts[1]
		}
	}
	return v
}

// versionComponent parses a token as a version number, rejecting anything that
// is not a short run of digits (date stamps, "v1", "default", "latest", ...).
func versionComponent(tok string) (int, bool) {
	if len(tok) == 0 || len(tok) > maxVersionTokenDigits {
		return 0, false
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(tok)
	if err != nil {
		return 0, false
	}
	return n, true
}
