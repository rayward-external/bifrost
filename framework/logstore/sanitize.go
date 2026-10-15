package logstore

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ObjectStorageSanitizeConfig rewrites every payload immediately before it is
// written to object storage. It exists so an operator can keep secrets and
// inline binary attachments out of the stored request/response content.
// Nothing here changes what is sent upstream or what the database keeps.
type ObjectStorageSanitizeConfig struct {
	// RedactPatterns are RE2 expressions. Every match inside any string value
	// of the payload (including strings nested inside JSON-encoded fields) is
	// replaced with Placeholder.
	RedactPatterns []string `json:"redact_patterns,omitempty"`
	// Placeholder replaces each redacted match. Defaults to "[redacted-secret]".
	Placeholder string `json:"placeholder,omitempty"`
	// StripAttachments replaces inline base64 attachment data (data: URIs,
	// {"type":"base64","data":...} sources, input_audio.data) longer than
	// AttachmentMinChars with a size placeholder.
	StripAttachments bool `json:"strip_attachments,omitempty"`
	// AttachmentMinChars is the shortest base64 payload that is stripped.
	// Defaults to 64.
	AttachmentMinChars int `json:"attachment_min_chars,omitempty"`
}

const (
	defaultSanitizePlaceholder        = "[redacted-secret]"
	defaultSanitizeAttachmentMinChars = 64
	// sanitizeMaxDepth bounds the walk; anything deeper is replaced whole, so
	// an adversarially nested body can neither exhaust the stack nor slip past.
	sanitizeMaxDepth = 64
	sanitizeTooDeep  = "[sanitize: nesting too deep]"
)

var dataURIRe = regexp.MustCompile(`^data:([^;,]*)(;[^,]*)?;base64,`)

// payloadSanitizer is the compiled form of ObjectStorageSanitizeConfig.
type payloadSanitizer struct {
	patterns    []*regexp.Regexp
	placeholder string
	strip       bool
	minChars    int
}

// newPayloadSanitizer compiles cfg. A nil or empty config yields a nil
// sanitizer (no-op). An invalid pattern is an error: a sanitizer that silently
// skipped a pattern would store the very secrets it was configured to remove.
func newPayloadSanitizer(cfg *ObjectStorageSanitizeConfig) (*payloadSanitizer, error) {
	if cfg == nil || (len(cfg.RedactPatterns) == 0 && !cfg.StripAttachments) {
		return nil, nil
	}
	s := &payloadSanitizer{
		placeholder: cfg.Placeholder,
		strip:       cfg.StripAttachments,
		minChars:    cfg.AttachmentMinChars,
	}
	if s.placeholder == "" {
		s.placeholder = defaultSanitizePlaceholder
	}
	if s.minChars <= 0 {
		s.minChars = defaultSanitizeAttachmentMinChars
	}
	for i, p := range cfg.RedactPatterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("object_storage_sanitize.redact_patterns[%d]: %w", i, err)
		}
		s.patterns = append(s.patterns, re)
	}
	return s, nil
}

// Sanitize returns a rewritten copy of a JSON payload. It never returns the
// input unchanged on failure: an error means the caller must drop the upload.
func (s *payloadSanitizer) Sanitize(data []byte) ([]byte, error) {
	if s == nil {
		return data, nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("sanitize: decode payload: %w", err)
	}
	out, err := marshalNoEscape(s.walk(v, "", 0))
	if err != nil {
		return nil, fmt.Errorf("sanitize: encode payload: %w", err)
	}
	return out, nil
}

func (s *payloadSanitizer) walk(v any, key string, depth int) any {
	if depth > sanitizeMaxDepth {
		return sanitizeTooDeep
	}
	switch t := v.(type) {
	case map[string]any:
		if s.strip {
			// Anthropic-style source block: {"type":"base64","media_type":...,"data":"..."}
			if typ, _ := t["type"].(string); typ == "base64" {
				if d, ok := t["data"].(string); ok && len(d) >= s.minChars {
					t["data"] = attachmentPlaceholder(len(d))
				}
			}
		}
		for k, child := range t {
			if s.strip && (key == "input_audio" || key == "inlineData" || key == "inline_data") && k == "data" {
				if d, ok := child.(string); ok && len(d) >= s.minChars {
					t[k] = attachmentPlaceholder(len(d))
					continue
				}
			}
			t[k] = s.walk(child, k, depth+1)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = s.walk(child, key, depth+1)
		}
		return t
	case string:
		return s.sanitizeString(t, key, depth)
	default:
		return v
	}
}

// sanitizeString handles a string leaf. Log payload fields are themselves
// JSON-encoded strings, so a string that decodes as a JSON object or array is
// walked and re-encoded, keeping its encoding shape.
func (s *payloadSanitizer) sanitizeString(str, key string, depth int) any {
	trimmed := strings.TrimSpace(str)
	// A2A/agent captures store raw_request/raw_response as base64 of the wire
	// bytes (core/schemas/a2arawcapture.go), which no pattern can see into.
	// Decode, sanitize the text, re-encode. A body that is not text cannot be
	// checked, so it is omitted rather than stored blind.
	if (key == "raw_request" || key == "raw_response") && trimmed != "" && trimmed[0] != '{' && trimmed[0] != '[' {
		if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
			if !utf8.Valid(decoded) {
				return fmt.Sprintf("[binary body omitted: %d bytes]", len(decoded))
			}
			if clean, ok := s.sanitizeString(string(decoded), key+".decoded", depth+1).(string); ok {
				return base64.StdEncoding.EncodeToString([]byte(clean))
			}
		}
	}
	if len(trimmed) > 1 && (trimmed[0] == '{' || trimmed[0] == '[') {
		var inner any
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		if dec.Decode(&inner) == nil && !dec.More() {
			if enc, err := marshalNoEscape(s.walk(inner, key, depth+1)); err == nil {
				return string(enc)
			}
		}
	}
	if s.strip && len(str) >= s.minChars {
		if loc := dataURIRe.FindStringIndex(str); loc != nil {
			return str[:loc[1]] + attachmentPlaceholder(len(str)-loc[1])
		}
	}
	for _, re := range s.patterns {
		str = re.ReplaceAllLiteralString(str, s.placeholder)
	}
	return str
}

// marshalNoEscape is json.Marshal without HTML escaping, so stored bodies keep
// '<', '>' and '&' as written instead of \u003c-style escapes.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func attachmentPlaceholder(n int) string {
	return fmt.Sprintf("[base64_data truncated: %d chars]", n)
}
