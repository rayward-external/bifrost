package modelcatalog

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/modelcatalog/datasheet"
	"github.com/maximhq/bifrost/framework/modelcatalog/keyconfig"
	"github.com/maximhq/bifrost/framework/modelcatalog/live"
)

// makeTestCatalogKey is the composite key format used by pricingData: model|provider|mode.
// Mirrors datasheet.makeKey (private); defined here so test fixtures stay readable.
func makeTestCatalogKey(model, provider, mode string) string {
	return model + "|" + provider + "|" + mode
}

// newCatalogWithPricing constructs a ModelCatalog seeded with pricing rows
// and a base-model index for unit tests. Does not start background workers.
func newCatalogWithPricing(
	pricingData map[string]configstoreTables.TableModelPricing,
	baseModelIndex map[string]string,
) *ModelCatalog {
	return &ModelCatalog{
		datasheet: datasheet.NewTestStoreWithRows(pricingData, baseModelIndex),
		live:      live.New(nil),
		keyconf:   keyconfig.New(nil),
		done:      make(chan struct{}),
	}
}

func TestGetModelCapabilityEntryForModel_PrefersChatThenResponsesThenCompletion(t *testing.T) {
	contextLengthChat := 128000
	maxInputTokensChat := 64000
	maxOutputTokensChat := 16000
	modality := "text"

	mc := newCatalogWithPricing(map[string]configstoreTables.TableModelPricing{
		makeTestCatalogKey("gpt-4o", "openai", "responses"): {
			Model:           "gpt-4o",
			Provider:        "openai",
			Mode:            "responses",
			ContextLength:   capabilityIntPtr(200000),
			MaxInputTokens:  capabilityIntPtr(100000),
			MaxOutputTokens: capabilityIntPtr(32000),
		},
		makeTestCatalogKey("gpt-4o", "openai", "chat"): {
			Model:           "gpt-4o",
			Provider:        "openai",
			Mode:            "chat",
			ContextLength:   &contextLengthChat,
			MaxInputTokens:  &maxInputTokensChat,
			MaxOutputTokens: &maxOutputTokensChat,
			Architecture: &schemas.Architecture{
				Modality: &modality,
			},
		},
	}, nil)

	entry := mc.GetModelCapabilityEntryForModel("gpt-4o", schemas.OpenAI)
	if entry == nil {
		t.Fatal("expected capability entry")
	}
	if entry.Mode != "chat" {
		t.Fatalf("expected chat mode to win, got %q", entry.Mode)
	}
	if entry.ContextLength == nil || *entry.ContextLength != contextLengthChat {
		t.Fatalf("expected context_length=%d, got %#v", contextLengthChat, entry.ContextLength)
	}
	if entry.MaxInputTokens == nil || *entry.MaxInputTokens != maxInputTokensChat {
		t.Fatalf("expected max_input_tokens=%d, got %#v", maxInputTokensChat, entry.MaxInputTokens)
	}
	if entry.MaxOutputTokens == nil || *entry.MaxOutputTokens != maxOutputTokensChat {
		t.Fatalf("expected max_output_tokens=%d, got %#v", maxOutputTokensChat, entry.MaxOutputTokens)
	}
	if entry.Architecture == nil || entry.Architecture.Modality == nil || *entry.Architecture.Modality != modality {
		t.Fatalf("expected architecture modality=%q, got %#v", modality, entry.Architecture)
	}
}

func TestGetModelCapabilityEntryForModel_FallsBackToAnyModeDeterministically(t *testing.T) {
	mc := newCatalogWithPricing(map[string]configstoreTables.TableModelPricing{
		makeTestCatalogKey("imagen", "vertex", "image_generation"): {
			Model:           "imagen",
			Provider:        "vertex",
			Mode:            "image_generation",
			ContextLength:   capabilityIntPtr(4096),
			MaxOutputTokens: capabilityIntPtr(1),
		},
	}, nil)

	entry := mc.GetModelCapabilityEntryForModel("imagen", schemas.Vertex)
	if entry == nil {
		t.Fatal("expected capability entry")
	}
	if entry.Mode != "image_generation" {
		t.Fatalf("expected image_generation fallback, got %q", entry.Mode)
	}
}

func TestGetModelCapabilityEntryForModel_ResolvesAliasFamilyViaBaseModel(t *testing.T) {
	contextLengthChat := 128000

	mc := newCatalogWithPricing(map[string]configstoreTables.TableModelPricing{
		makeTestCatalogKey("gpt-4o-2024-08-06", "openai", "responses"): {
			Model:           "gpt-4o-2024-08-06",
			BaseModel:       "gpt-4o",
			Provider:        "openai",
			Mode:            "responses",
			ContextLength:   capabilityIntPtr(64000),
			MaxOutputTokens: capabilityIntPtr(8000),
		},
		makeTestCatalogKey("gpt-4o-2024-08-06", "openai", "chat"): {
			Model:           "gpt-4o-2024-08-06",
			BaseModel:       "gpt-4o",
			Provider:        "openai",
			Mode:            "chat",
			ContextLength:   &contextLengthChat,
			MaxOutputTokens: capabilityIntPtr(16000),
		},
	}, map[string]string{
		"gpt-4o-2024-08-06": "gpt-4o",
	})

	entry := mc.GetModelCapabilityEntryForModel("gpt-4o", schemas.OpenAI)
	if entry == nil {
		t.Fatal("expected capability entry for base-model alias")
	}
	if entry.Mode != "chat" {
		t.Fatalf("expected chat mode to win for alias family, got %q", entry.Mode)
	}
	if entry.ContextLength == nil || *entry.ContextLength != contextLengthChat {
		t.Fatalf("expected alias family context_length=%d, got %#v", contextLengthChat, entry.ContextLength)
	}
}

func TestGetModelCapabilityEntryForModel_ResolvesProviderPrefixedAlias(t *testing.T) {
	mc := newCatalogWithPricing(map[string]configstoreTables.TableModelPricing{
		makeTestCatalogKey("gpt-4o-2024-08-06", "openai", "chat"): {
			Model:           "gpt-4o-2024-08-06",
			BaseModel:       "gpt-4o",
			Provider:        "openai",
			Mode:            "chat",
			ContextLength:   capabilityIntPtr(128000),
			MaxOutputTokens: capabilityIntPtr(16000),
		},
	}, map[string]string{
		"gpt-4o-2024-08-06": "gpt-4o",
	})

	entry := mc.GetModelCapabilityEntryForModel("openai/gpt-4o", schemas.OpenAI)
	if entry == nil {
		t.Fatal("expected capability entry for provider-prefixed alias")
	}
	if entry.Mode != "chat" {
		t.Fatalf("expected chat mode for provider-prefixed alias, got %q", entry.Mode)
	}
}

func TestGetModelCapabilityEntryForModel_PrefersLiteralMatchOverAliasFamily(t *testing.T) {
	literalContextLength := 32000
	aliasContextLength := 128000

	mc := newCatalogWithPricing(map[string]configstoreTables.TableModelPricing{
		makeTestCatalogKey("gpt-4o", "openai", "chat"): {
			Model:           "gpt-4o",
			BaseModel:       "gpt-4o",
			Provider:        "openai",
			Mode:            "chat",
			ContextLength:   &literalContextLength,
			MaxOutputTokens: capabilityIntPtr(4000),
		},
		makeTestCatalogKey("gpt-4o-2024-08-06", "openai", "chat"): {
			Model:           "gpt-4o-2024-08-06",
			BaseModel:       "gpt-4o",
			Provider:        "openai",
			Mode:            "chat",
			ContextLength:   &aliasContextLength,
			MaxOutputTokens: capabilityIntPtr(16000),
		},
	}, map[string]string{
		"gpt-4o":            "gpt-4o",
		"gpt-4o-2024-08-06": "gpt-4o",
	})

	entry := mc.GetModelCapabilityEntryForModel("gpt-4o", schemas.OpenAI)
	if entry == nil {
		t.Fatal("expected literal capability entry")
	}
	if entry.ContextLength == nil || *entry.ContextLength != literalContextLength {
		t.Fatalf("expected literal match to win with context_length=%d, got %#v", literalContextLength, entry.ContextLength)
	}
}

func capabilityIntPtr(v int) *int { return &v }
