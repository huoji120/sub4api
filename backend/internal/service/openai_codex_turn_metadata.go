package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf16"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Turn metadata is also used as an HTTP header, including when carried in WS JSON.
func marshalCodexTurnMetadata(metadata map[string]any) ([]byte, error) {
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	for i, b := range raw {
		if b < 0x7f {
			continue
		}
		out := make([]byte, 0, len(raw)+16)
		out = append(out, raw[:i]...)
		appendEscape := func(r rune) {
			const hex = "0123456789abcdef"
			out = append(out, '\\', 'u', hex[r>>12&15], hex[r>>8&15], hex[r>>4&15], hex[r&15])
		}
		for _, r := range string(raw[i:]) {
			switch {
			case r < 0x7f:
				out = append(out, byte(r))
			case r <= 0xffff:
				appendEscape(r)
			default:
				high, low := utf16.EncodeRune(r)
				appendEscape(high)
				appendEscape(low)
			}
		}
		return out, nil
	}
	return raw, nil
}

// Keep business metadata integers lossless when rewriting only identity fields.
func decodeCodexMetadataObject(raw string, target *map[string]any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("multiple JSON values in Codex metadata")
	}
	return nil
}

func decodeCodexTurnMetadata(raw string) (map[string]any, error) {
	var metadata map[string]any
	err := decodeCodexMetadataObject(raw, &metadata)
	return metadata, err
}

func splitCodexWindowID(value string) (string, string, bool) {
	thread, ordinal, found := strings.Cut(value, ":")
	if !found || thread == "" || ordinal == "" {
		return "", "", false
	}
	for _, digit := range ordinal {
		if digit < '0' || digit > '9' {
			return "", "", false
		}
	}
	return thread, ordinal, true
}

// normalizeCodexRequestMetadata runs after account/fingerprint projection. It
// never scopes an already-scoped ID. Core's embedded metadata is canonical;
// flat client_metadata and transport headers are compatibility projections.
// isCompact additionally identifies the legacy /responses/compact endpoint.
func normalizeCodexRequestMetadata(body []byte, headers http.Header, account *Account, isCompact bool) ([]byte, bool, error) {
	if account == nil || !account.IsOpenAIOAuthLike() || !gjson.ParseBytes(body).IsObject() {
		return body, false, nil
	}
	if isCompact {
		// Legacy CompactionInput has no client_metadata field.
		_, err := normalizeCodexClientMetadata(map[string]any{}, headers, true)
		return body, false, err
	}
	cm := map[string]any{}
	if raw := gjson.GetBytes(body, "client_metadata"); raw.IsObject() {
		if err := decodeCodexMetadataObject(raw.Raw, &cm); err != nil {
			return body, false, err
		}
	} else if raw.Exists() {
		return body, false, nil
	}
	changed, err := normalizeCodexClientMetadata(cm, headers, isCompact || HasCompactionTriggerInInput(body))
	if err != nil || !changed {
		return body, false, err
	}
	raw, err := json.Marshal(cm)
	if err != nil {
		return body, false, err
	}
	next, err := sjson.SetRawBytes(body, "client_metadata", raw)
	if err != nil {
		return body, false, err
	}
	return next, true, nil
}

func normalizeCodexClientMetadata(cm map[string]any, headers http.Header, isCompact bool) (bool, error) {
	metadata := map[string]any{}
	if raw, ok := cm[openAIWSTurnMetadataHeader].(string); ok {
		if decoded, err := decodeCodexTurnMetadata(raw); err == nil && decoded != nil {
			metadata = decoded
		}
	}
	if raw := headers.Get(openAIWSTurnMetadataHeader); raw != "" {
		if decoded, err := decodeCodexTurnMetadata(raw); err == nil {
			for key, value := range decoded {
				if _, exists := metadata[key]; !exists {
					metadata[key] = value
				}
			}
		}
	}
	changed := false
	set := func(values map[string]any, key string, value any) {
		if current, exists := values[key]; exists && current == value {
			return
		}
		values[key] = value
		changed = true
	}
	for _, field := range []struct {
		metadata string
		flat     string
		header   string
	}{
		{"installation_id", "x-codex-installation-id", "x-codex-installation-id"},
		{"session_id", "session_id", "session-id"},
		{"thread_id", "thread_id", "thread-id"},
		{"turn_id", "turn_id", "turn-id"},
		{"window_id", "x-codex-window-id", "x-codex-window-id"},
	} {
		value, _ := metadata[field.metadata].(string)
		if value == "" {
			value, _ = cm[field.flat].(string)
		}
		if value == "" {
			value, _ = cm[field.metadata].(string)
		}
		if value == "" {
			value = headers.Get(field.header)
		}
		if value == "" {
			continue
		}
		set(cm, field.flat, value)
		if _, exists := cm[field.metadata]; exists {
			set(cm, field.metadata, value)
		}
		if len(metadata) != 0 {
			set(metadata, field.metadata, value)
		}
		if headers != nil && (field.metadata == "session_id" || field.metadata == "thread_id" || headers.Get(field.header) != "") {
			headers.Set(field.header, value)
		}
	}
	if headers != nil {
		if session, _ := cm["session_id"].(string); session != "" && headers.Get("session_id") != "" {
			headers.Set("session_id", session)
		}
		if thread, _ := cm["thread_id"].(string); thread != "" && headers.Get("x-client-request-id") != "" {
			headers.Set("x-client-request-id", thread)
		}
	}
	if isCompact {
		set(metadata, "request_kind", "compaction")
		if _, exists := cm["request_kind"]; exists {
			set(cm, "request_kind", "compaction")
		}
	}
	if len(metadata) != 0 {
		raw, err := marshalCodexTurnMetadata(metadata)
		if err != nil {
			return false, err
		}
		set(cm, openAIWSTurnMetadataHeader, string(raw))
		if headers != nil {
			headers.Set(openAIWSTurnMetadataHeader, string(raw))
		}
	}
	return changed, nil
}

func normalizeCodexRequestMetadataMap(payload map[string]any, headers http.Header, account *Account) error {
	if account == nil || !account.IsOpenAIOAuthLike() || payload == nil {
		return nil
	}
	cm, ok := payload["client_metadata"].(map[string]any)
	if !ok {
		if existing, exists := payload["client_metadata"]; exists && existing != nil {
			return nil
		}
		cm = map[string]any{}
	}
	isCompact := false
	if input, ok := payload["input"].([]any); ok {
		for _, raw := range input {
			if item, ok := raw.(map[string]any); ok && item["type"] == "compaction_trigger" {
				isCompact = true
				break
			}
		}
	}
	changed, err := normalizeCodexClientMetadata(cm, headers, isCompact)
	if err == nil && changed {
		payload["client_metadata"] = cm
	}
	return err
}

// applyCodexRequestMetadata keeps the HTTP request and the caller's body snapshot
// identical before transport compression.
func applyCodexRequestMetadata(req *http.Request, body []byte, account *Account, isCompact bool) ([]byte, error) {
	if req == nil {
		return body, nil
	}
	next, changed, err := normalizeCodexRequestMetadata(body, req.Header, account, isCompact)
	if err != nil {
		return body, err
	}
	if !changed {
		return body, nil
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}
	req.Body = io.NopCloser(bytes.NewReader(next))
	req.ContentLength = int64(len(next))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(next)), nil
	}
	return next, nil
}
