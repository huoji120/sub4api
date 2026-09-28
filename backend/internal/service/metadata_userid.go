package service

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// NewMetadataFormatMinVersion is the minimum Claude Code version that uses
// JSON-formatted metadata.user_id instead of the legacy concatenated string.
const NewMetadataFormatMinVersion = "2.1.78"

// ParsedUserID represents the components extracted from a metadata.user_id value.
type ParsedUserID struct {
	DeviceID    string // 64-char hex (or arbitrary client id)
	AccountUUID string // may be empty
	SessionID   string // UUID
	IsNewFormat bool   // true if the original was JSON format
	// ExtraFields contains unknown JSON fields from the new format. The raw
	// values are retained so rewrites do not discard fields added by newer
	// Claude Code clients (for example parent_session_id and tk).
	ExtraFields map[string]json.RawMessage
}

// legacyUserIDRegex matches the legacy user_id format:
//
//	user_{64hex}_account_{optional_uuid}_session_{uuid}
var legacyUserIDRegex = regexp.MustCompile(`^user_([a-fA-F0-9]{64})_account_([a-fA-F0-9-]*)_session_([a-fA-F0-9-]{36})$`)

// jsonUserID is the JSON structure for the new metadata.user_id format.
type jsonUserID struct {
	DeviceID    string `json:"device_id"`
	AccountUUID string `json:"account_uuid"`
	SessionID   string `json:"session_id"`
}

func formatJSONMetadataUserID(deviceID, accountUUID, sessionID string, extraFields map[string]json.RawMessage) string {
	var out bytes.Buffer
	out.WriteByte('{')
	writeField := func(key string, value []byte, comma *bool) {
		if *comma {
			out.WriteByte(',')
		}
		encodedKey, _ := json.Marshal(key)
		out.Write(encodedKey)
		out.WriteByte(':')
		out.Write(value)
		*comma = true
	}

	comma := false
	deviceRaw, _ := json.Marshal(deviceID)
	accountRaw, _ := json.Marshal(accountUUID)
	sessionRaw, _ := json.Marshal(sessionID)
	writeField("device_id", deviceRaw, &comma)
	writeField("account_uuid", accountRaw, &comma)
	writeField("session_id", sessionRaw, &comma)

	keys := make([]string, 0, len(extraFields))
	for key := range extraFields {
		if key != "device_id" && key != "account_uuid" && key != "session_id" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		writeField(key, extraFields[key], &comma)
	}
	out.WriteByte('}')
	return out.String()
}

// ParseMetadataUserID parses a metadata.user_id string in either format.
// Returns nil if the input cannot be parsed.
func ParseMetadataUserID(raw string) *ParsedUserID {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	// Try JSON format first (starts with '{')
	if raw[0] == '{' {
		var j jsonUserID
		if err := json.Unmarshal([]byte(raw), &j); err != nil {
			return nil
		}
		if j.DeviceID == "" || j.SessionID == "" {
			return nil
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			return nil
		}
		extraFields := make(map[string]json.RawMessage, len(fields))
		for key, value := range fields {
			if key == "device_id" || key == "account_uuid" || key == "session_id" {
				continue
			}
			extraFields[key] = value
		}
		return &ParsedUserID{
			DeviceID:    j.DeviceID,
			AccountUUID: j.AccountUUID,
			SessionID:   j.SessionID,
			IsNewFormat: true,
			ExtraFields: extraFields,
		}
	}

	// Try legacy format
	matches := legacyUserIDRegex.FindStringSubmatch(raw)
	if matches == nil {
		return nil
	}
	return &ParsedUserID{
		DeviceID:    matches[1],
		AccountUUID: matches[2],
		SessionID:   matches[3],
		IsNewFormat: false,
	}
}

// FormatMetadataUserID builds a metadata.user_id string in the format
// appropriate for the given CLI version. Components are the rewritten values
// (not necessarily the originals).
func FormatMetadataUserID(deviceID, accountUUID, sessionID, uaVersion string) string {
	return formatMetadataUserID(deviceID, accountUUID, sessionID, nil, uaVersion)
}

func formatMetadataUserID(deviceID, accountUUID, sessionID string, extraFields map[string]json.RawMessage, uaVersion string) string {
	if IsNewMetadataFormatVersion(uaVersion) {
		return formatJSONMetadataUserID(deviceID, accountUUID, sessionID, extraFields)
	}
	// Legacy format
	return "user_" + deviceID + "_account_" + accountUUID + "_session_" + sessionID
}

// IsNewMetadataFormatVersion returns true if the given CLI version uses the
// new JSON metadata.user_id format (>= 2.1.78).
func IsNewMetadataFormatVersion(version string) bool {
	if version == "" {
		return false
	}
	return CompareVersions(version, NewMetadataFormatMinVersion) >= 0
}

// ExtractCLIVersion extracts the Claude Code version from a User-Agent string.
// Returns "" if the UA doesn't match the expected pattern.
func ExtractCLIVersion(ua string) string {
	matches := claudeCodeUAVersionPattern.FindStringSubmatch(ua)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}
