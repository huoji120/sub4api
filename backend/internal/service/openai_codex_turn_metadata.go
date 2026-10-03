package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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
	defaultsChanged, err := ensureCodexClientMetadataDefaults(cm, headers, body, account, false)
	if err != nil {
		return body, false, err
	}
	changed, err := normalizeCodexClientMetadata(cm, headers, HasCompactionTriggerInInput(body))
	if err != nil {
		return body, false, err
	}
	changed = changed || defaultsChanged
	if !changed {
		return body, false, nil
	}
	raw, err := json.Marshal(cm)
	if err != nil {
		return body, false, err
	}
	next := body
	next, err = sjson.SetRawBytes(next, "client_metadata", raw)
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

func ensureCodexClientMetadataDefaults(cm map[string]any, headers http.Header, body []byte, account *Account, isCompact bool) (bool, error) {
	if cm == nil || account == nil {
		return false, nil
	}
	metadata := map[string]any{}
	if raw, ok := cm[openAIWSTurnMetadataHeader].(string); ok && strings.TrimSpace(raw) != "" {
		if decoded, err := decodeCodexTurnMetadata(raw); err == nil && decoded != nil {
			metadata = decoded
		}
	}
	if headers != nil {
		if raw := headers.Get(openAIWSTurnMetadataHeader); strings.TrimSpace(raw) != "" {
			if decoded, err := decodeCodexTurnMetadata(raw); err == nil {
				for key, value := range decoded {
					if _, exists := metadata[key]; !exists {
						metadata[key] = value
					}
				}
			}
		}
	}
	changed := false
	set := func(values map[string]any, key, value string) string {
		if strings.TrimSpace(value) == "" {
			return ""
		}
		if current, ok := values[key].(string); ok && strings.TrimSpace(current) != "" {
			return strings.TrimSpace(current)
		}
		values[key] = value
		changed = true
		return value
	}
	from := func(values ...any) string {
		return firstNonEmptyString(values...)
	}
	installationID := from(metadata["installation_id"], cm["x-codex-installation-id"], cm["installation_id"])
	if installationID == "" && headers != nil {
		installationID = strings.TrimSpace(headers.Get("x-codex-installation-id"))
	}
	if installationID == "" {
		installationID = strings.TrimSpace(account.GetOpenAIDeviceID())
	}
	if installationID == "" {
		installationID = deriveStableUUIDv4(fmt.Sprintf("sub2api:codex-installation:v1:%d", account.ID))
	}
	sessionID := from(metadata["session_id"], cm["session_id"])
	if sessionID == "" && headers != nil {
		sessionID = from(headers.Get("session-id"), headers.Get("session_id"), headers.Get("conversation_id"))
	}
	if sessionID == "" {
		sessionID = strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	}
	if sessionID == "" {
		sessionID = deriveStableUUIDv4(fmt.Sprintf("sub2api:codex-session:v1:%d", account.ID))
	}
	threadID := from(metadata["thread_id"], cm["thread_id"])
	if threadID == "" && headers != nil {
		threadID = from(headers.Get("thread-id"), headers.Get("thread_id"), headers.Get("x-client-request-id"))
	}
	if threadID == "" {
		threadID = sessionID
	}
	bodyDigest := sha256.Sum256(body)
	turnID := from(metadata["turn_id"], cm["turn_id"])
	if turnID == "" && headers != nil {
		turnID = strings.TrimSpace(headers.Get("turn-id"))
	}
	if turnID == "" {
		turnID = deriveStableUUIDv4("sub2api:codex-turn:v1:" + sessionID + ":" + hex.EncodeToString(bodyDigest[:]))
	}
	windowID := from(metadata["window_id"], cm["x-codex-window-id"], cm["window_id"])
	if windowID == "" && headers != nil {
		windowID = strings.TrimSpace(headers.Get("x-codex-window-id"))
	}
	if windowID == "" {
		windowID = threadID + ":0"
	}
	requestKind := from(metadata["request_kind"])
	if isCompact {
		requestKind = "compaction"
	} else if requestKind == "" {
		requestKind = "turn"
	}

	set(cm, "x-codex-installation-id", installationID)
	set(cm, "session_id", sessionID)
	set(cm, "thread_id", threadID)
	set(cm, "turn_id", turnID)
	set(cm, "x-codex-window-id", windowID)
	set(metadata, "installation_id", installationID)
	set(metadata, "session_id", sessionID)
	set(metadata, "thread_id", threadID)
	set(metadata, "turn_id", turnID)
	set(metadata, "window_id", windowID)
	set(metadata, "request_kind", requestKind)
	if _, exists := metadata["turn_started_at_unix_ms"]; !exists {
		metadata["turn_started_at_unix_ms"] = time.Now().UnixMilli()
		changed = true
	}
	raw, err := marshalCodexTurnMetadata(metadata)
	if err != nil {
		return false, err
	}
	if current, ok := cm[openAIWSTurnMetadataHeader].(string); !ok || current != string(raw) {
		cm[openAIWSTurnMetadataHeader] = string(raw)
		changed = true
	}
	if headers != nil {
		headers.Set("x-codex-installation-id", installationID)
		headers.Set("session-id", sessionID)
		headers.Set("thread-id", threadID)
		headers.Set("x-client-request-id", threadID)
		headers.Set("x-codex-window-id", windowID)
		headers.Set("session_id", sessionID)
		headers.Set(openAIWSTurnMetadataHeader, string(raw))
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
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	defaultsChanged, err := ensureCodexClientMetadataDefaults(cm, headers, body, account, isCompact)
	if err != nil {
		return err
	}
	changed, err := normalizeCodexClientMetadata(cm, headers, isCompact)
	if err != nil {
		return err
	}
	if defaultsChanged || changed {
		payload["client_metadata"] = cm
	}
	return nil
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
