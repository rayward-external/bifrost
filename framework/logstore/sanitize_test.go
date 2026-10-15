package logstore

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSecretPattern = `sk-[A-Za-z0-9_-]{8,}`

func mustSanitizer(t *testing.T, cfg *ObjectStorageSanitizeConfig) *payloadSanitizer {
	t.Helper()
	s, err := newPayloadSanitizer(cfg)
	require.NoError(t, err)
	return s
}

func TestSanitize_NilOrEmptyConfigIsANoOp(t *testing.T) {
	for _, cfg := range []*ObjectStorageSanitizeConfig{nil, {}} {
		s, err := newPayloadSanitizer(cfg)
		require.NoError(t, err)
		assert.Nil(t, s)
		out, err := s.Sanitize([]byte(`{"a":"sk-abcdefghijkl"}`))
		require.NoError(t, err)
		assert.Equal(t, `{"a":"sk-abcdefghijkl"}`, string(out))
	}
}

func TestSanitize_InvalidPatternIsAnError(t *testing.T) {
	_, err := newPayloadSanitizer(&ObjectStorageSanitizeConfig{RedactPatterns: []string{"(unclosed"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redact_patterns[0]")
}

func TestSanitize_RedactsInsideJSONEncodedFields(t *testing.T) {
	s := mustSanitizer(t, &ObjectStorageSanitizeConfig{RedactPatterns: []string{testSecretPattern}})
	// Log payloads are map[string]string whose values are themselves JSON.
	inner := `[{"role":"user","content":"my key is sk-abcdefghijklmnop, keep <b>&</b> as is"}]`
	payload, err := json.Marshal(map[string]string{"input_history": inner, "model": "m"})
	require.NoError(t, err)

	out, err := s.Sanitize(payload)
	require.NoError(t, err)
	var got map[string]string
	require.NoError(t, json.Unmarshal(out, &got))
	assert.NotContains(t, string(out), "sk-abcdefghijklmnop")
	assert.Contains(t, got["input_history"], "my key is [redacted-secret], keep <b>&</b> as is")
	var msgs []map[string]any
	require.NoError(t, json.Unmarshal([]byte(got["input_history"]), &msgs), "the field stays JSON-encoded")
	assert.Equal(t, "m", got["model"])
}

func TestSanitize_PlaceholderIsConfigurable(t *testing.T) {
	s := mustSanitizer(t, &ObjectStorageSanitizeConfig{RedactPatterns: []string{testSecretPattern}, Placeholder: "XX"})
	out, err := s.Sanitize([]byte(`{"a":"x sk-abcdefghijkl y"}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":"x XX y"}`, string(out))
}

func TestSanitize_StripsInlineAttachments(t *testing.T) {
	s := mustSanitizer(t, &ObjectStorageSanitizeConfig{StripAttachments: true})
	big := strings.Repeat("A", 500)
	doc := map[string]any{
		"image_url":   map[string]any{"url": "data:image/png;base64," + big},
		"source":      map[string]any{"type": "base64", "media_type": "application/pdf", "data": big},
		"input_audio": map[string]any{"format": "wav", "data": big},
		"short":       "data:image/png;base64,QUJD",
		"text":        big,
	}
	payload, err := json.Marshal(doc)
	require.NoError(t, err)
	out, err := s.Sanitize(payload)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))

	assert.Equal(t, "data:image/png;base64,[base64_data truncated: 500 chars]", got["image_url"].(map[string]any)["url"])
	assert.Equal(t, "[base64_data truncated: 500 chars]", got["source"].(map[string]any)["data"])
	assert.Equal(t, "application/pdf", got["source"].(map[string]any)["media_type"])
	assert.Equal(t, "[base64_data truncated: 500 chars]", got["input_audio"].(map[string]any)["data"])
	assert.Equal(t, "data:image/png;base64,QUJD", got["short"], "below attachment_min_chars is kept")
	assert.Equal(t, big, got["text"], "plain text is not an attachment")
}

func TestSanitize_AttachmentsAreKeptWhenStrippingIsOff(t *testing.T) {
	s := mustSanitizer(t, &ObjectStorageSanitizeConfig{RedactPatterns: []string{testSecretPattern}})
	big := "data:image/png;base64," + strings.Repeat("A", 500)
	out, err := s.Sanitize([]byte(`{"u":"` + big + `"}`))
	require.NoError(t, err)
	assert.Contains(t, string(out), big)
}

func TestSanitize_NonJSONPayloadIsAnError(t *testing.T) {
	s := mustSanitizer(t, &ObjectStorageSanitizeConfig{RedactPatterns: []string{testSecretPattern}})
	_, err := s.Sanitize([]byte("not json sk-abcdefghijkl"))
	require.Error(t, err)
}

func TestSanitize_DeepNestingIsBounded(t *testing.T) {
	s := mustSanitizer(t, &ObjectStorageSanitizeConfig{RedactPatterns: []string{testSecretPattern}})
	deep := strings.Repeat(`{"a":`, sanitizeMaxDepth+10) + `"sk-abcdefghijkl"` + strings.Repeat(`}`, sanitizeMaxDepth+10)
	out, err := s.Sanitize([]byte(deep))
	require.NoError(t, err)
	assert.NotContains(t, string(out), "sk-abcdefghijkl")
	assert.Contains(t, string(out), sanitizeTooDeep)
}

func TestSanitize_NumbersKeepTheirPrecision(t *testing.T) {
	s := mustSanitizer(t, &ObjectStorageSanitizeConfig{RedactPatterns: []string{testSecretPattern}})
	out, err := s.Sanitize([]byte(`{"n":12345678901234567890,"f":0.1}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"n":12345678901234567890,"f":0.1}`, string(out))
}

func TestHybrid_SanitizerRewritesTheUploadedObject(t *testing.T) {
	ctx := context.Background()
	inner, err := newSqliteLogStore(ctx, &SQLiteConfig{Path: filepath.Join(t.TempDir(), "hybrid.db")}, hybridTestLogger{})
	require.NoError(t, err)
	objStore := objectstore.NewInMemoryObjectStore()
	hybrid := newHybridLogStore(inner, objStore, "test", hybridTestLogger{}, nil, nil)
	hybrid.sanitizer = mustSanitizer(t, &ObjectStorageSanitizeConfig{
		RedactPatterns:   []string{testSecretPattern},
		StripAttachments: true,
	})
	defer hybrid.Close(ctx)

	entry := newContentHiddenTestEntry("sanitized-1")
	entry.ContentHidden = true
	secret := "please use sk-abcdefghijklmnop for the call"
	entry.InputHistoryParsed = []schemas.ChatMessage{
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &secret}},
	}
	require.NoError(t, entry.SerializeFields())
	require.NoError(t, hybrid.CreateIfNotExists(ctx, entry))

	waitForUploads(t, func() bool { return objStore.Len() == 1 })
	data, err := objStore.Get(ctx, ObjectKey("test", entry.Timestamp, "sanitized-1"))
	require.NoError(t, err)
	assert.NotContains(t, string(data), "sk-abcdefghijklmnop")
	var payload map[string]string
	require.NoError(t, sonic.Unmarshal(data, &payload))
	assert.Contains(t, payload["input_history"], "[redacted-secret]")
	assert.Contains(t, payload["output_message"], "sensitive answer", "non-secret content is kept")
}

func TestLogStoreConfig_ParsesObjectStorageSanitize(t *testing.T) {
	var cfg Config
	require.NoError(t, json.Unmarshal([]byte(`{"enabled":false,"object_storage_sanitize":{"redact_patterns":["x"],"strip_attachments":true}}`), &cfg))
	require.NotNil(t, cfg.ObjectStorageSanitize)
	assert.Equal(t, []string{"x"}, cfg.ObjectStorageSanitize.RedactPatterns)
	assert.True(t, cfg.ObjectStorageSanitize.StripAttachments)
}
