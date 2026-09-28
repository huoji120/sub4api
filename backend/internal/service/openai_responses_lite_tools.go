package service

import (
	"bytes"
	"fmt"
	"maps"
	"reflect"
	"strings"

	"github.com/tidwall/sjson"
)

type openAIResponsesLiteValidationError struct {
	param   string
	message string
}

func (e *openAIResponsesLiteValidationError) Error() string { return e.message }

func newOpenAIResponsesLiteValidationError(param, format string, args ...any) error {
	return &openAIResponsesLiteValidationError{param: param, message: fmt.Sprintf(format, args...)}
}

// normalizeOpenAIResponsesLiteTools mirrors Codex's build_responses_request:
// tools and nonempty instructions precede history, reasoning covers all turns,
// and parallel calls are disabled. It also accepts already serialized Lite
// history and incremental tool-output requests without adding another prefix.
func normalizeOpenAIResponsesLiteTools(reqBody map[string]any) (bool, error) {
	if reqBody == nil {
		return false, nil
	}
	if parallel, exists := reqBody["parallel_tool_calls"]; exists {
		if _, ok := parallel.(bool); !ok {
			return false, newOpenAIResponsesLiteValidationError("parallel_tool_calls", "responses Lite requires parallel_tool_calls to be a boolean")
		}
	}
	if rawReasoning, exists := reqBody["reasoning"]; exists && rawReasoning != nil {
		if _, ok := rawReasoning.(map[string]any); !ok {
			return false, newOpenAIResponsesLiteValidationError("reasoning", "responses Lite requires reasoning to be an object")
		}
	}
	instructions, instructionsOK := reqBody["instructions"].(string)
	if raw := reqBody["instructions"]; raw != nil && !instructionsOK {
		return false, newOpenAIResponsesLiteValidationError("instructions", "responses Lite requires instructions to be a string")
	}
	rawTools, hasTools := reqBody["tools"]
	var tools []any
	if rawTools != nil {
		var ok bool
		tools, ok = rawTools.([]any)
		if !ok {
			return false, newOpenAIResponsesLiteValidationError("tools", "responses Lite requires tools to be an array")
		}
	}
	var err error
	tools, err = groupOpenAIResponsesLiteTools(tools)
	if err != nil {
		return false, err
	}
	input := reqBody["input"]
	if hasTools || instructions != "" {
		items, err := appendOpenAIResponsesLiteAdditionalTools(input, tools)
		if err != nil {
			return false, err
		}
		if instructions != "" {
			// Only inspect the prompt prefix. Equal text in later history is not
			// the current instruction, and must not suppress its insertion.
			index := 0
			for index < len(items) {
				item, _ := items[index].(map[string]any)
				if item["type"] != "additional_tools" {
					break
				}
				index++
			}
			if index == len(items) || !isOpenAIResponsesLiteInstruction(items[index], instructions) {
				message := map[string]any{
					"type": "message", "role": "developer",
					"content": []any{map[string]any{"type": "input_text", "text": instructions}},
				}
				prefixed := make([]any, 0, len(items)+1)
				prefixed = append(prefixed, items[:index]...)
				prefixed = append(prefixed, message)
				items = append(prefixed, items[index:]...)
			}
		}
		input = items
	}
	changed := hasTools || instructions != ""
	if stripOpenAIResponsesLiteImageDetails(input) {
		changed = true
	}
	if changed {
		reqBody["input"] = input
	}
	if hasTools {
		delete(reqBody, "tools")
	}
	if hasTools || instructions != "" {
		reqBody["instructions"] = ""
	}
	reasoningChanged, err := ensureOpenAIResponsesLiteReasoningContext(reqBody)
	if err != nil {
		return false, err
	}
	return ensureOpenAIResponsesLiteParallelToolCalls(reqBody, changed || reasoningChanged)
}

// Group functions exactly as codex-tools/tool_spec.rs does, without rebuilding
// schemas or discarding provider extensions on tools/namespaces.
func groupOpenAIResponsesLiteTools(tools []any) ([]any, error) {
	grouped := make([]any, 0, len(tools))
	functions := map[string]any{"type": "namespace", "name": "functions", "description": ""}
	members := []any{}
	functionsIndex := -1
	for index, rawTool := range tools {
		if shorthand, ok := rawTool.(string); ok && strings.TrimSpace(shorthand) != "" {
			grouped = append(grouped, rawTool)
			continue
		}
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return nil, newOpenAIResponsesLiteValidationError("tools", "responses Lite tool at index %d must be an object", index)
		}
		switch tool["type"] {
		case "function", "custom":
			members = append(members, rawTool)
		case "namespace":
			if tool["name"] != "functions" {
				grouped = append(grouped, rawTool)
				continue
			}
			nested, ok := tool["tools"].([]any)
			if !ok {
				return nil, newOpenAIResponsesLiteValidationError("tools", "responses Lite functions namespace tools must be an array")
			}
			members = append(members, nested...)
			for key, value := range tool {
				if key == "tools" {
					continue
				}
				if key == "description" && strings.TrimSpace(firstNonEmptyString(value)) == "" {
					continue
				}
				functions[key] = value
			}
		default:
			if strings.TrimSpace(firstNonEmptyString(tool["type"])) == "" {
				return nil, newOpenAIResponsesLiteValidationError("tools", "responses Lite tool at index %d is missing type", index)
			}
			// ToolSearch and WebSearch are official ToolSpec variants. Forward
			// other typed extensions for upstream validation, never drop them.
			grouped = append(grouped, rawTool)
			continue
		}
		if functionsIndex < 0 {
			functionsIndex = len(grouped)
		}
	}
	if functionsIndex >= 0 {
		functions["tools"] = members
		grouped = append(grouped, nil)
		copy(grouped[functionsIndex+1:], grouped[functionsIndex:])
		grouped[functionsIndex] = functions
	}
	return grouped, nil
}

func isOpenAIResponsesLiteInstruction(raw any, instructions string) bool {
	item, _ := raw.(map[string]any)
	if item["role"] != "developer" || (item["type"] != nil && item["type"] != "message") {
		return false
	}
	if text, ok := item["content"].(string); ok {
		return text == instructions
	}
	content, _ := item["content"].([]any)
	if len(content) != 1 {
		return false
	}
	part, _ := content[0].(map[string]any)
	return part["type"] == "input_text" && part["text"] == instructions
}

func stripOpenAIResponsesLiteImageDetails(input any) bool {
	items, _ := input.([]any)
	changed := false
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		var content []any
		switch item["type"] {
		case "message", nil:
			content, _ = item["content"].([]any)
		case "function_call_output", "custom_tool_call_output":
			content, _ = item["output"].([]any)
		}
		for _, rawPart := range content {
			part, _ := rawPart.(map[string]any)
			if part["type"] == "input_image" {
				if _, exists := part["detail"]; exists {
					delete(part, "detail")
					changed = true
				}
			}
		}
	}
	return changed
}

func ensureOpenAIResponsesLiteParallelToolCalls(reqBody map[string]any, changed bool) (bool, error) {
	parallel, exists := reqBody["parallel_tool_calls"]
	if exists {
		if _, ok := parallel.(bool); !ok {
			return false, newOpenAIResponsesLiteValidationError("parallel_tool_calls", "responses Lite requires parallel_tool_calls to be a boolean")
		}
	}
	if parallel == false {
		return changed, nil
	}
	reqBody["parallel_tool_calls"] = false
	return true, nil
}

func ensureOpenAIResponsesLiteReasoningContext(reqBody map[string]any) (bool, error) {
	rawReasoning, exists := reqBody["reasoning"]
	if !exists || rawReasoning == nil {
		reqBody["reasoning"] = map[string]any{"context": "all_turns"}
		return true, nil
	}
	reasoning, ok := rawReasoning.(map[string]any)
	if !ok {
		return false, newOpenAIResponsesLiteValidationError("reasoning", "responses Lite requires reasoning to be an object")
	}
	if context, ok := reasoning["context"].(string); ok && context == "all_turns" {
		return false, nil
	}
	reasoning["context"] = "all_turns"
	return true, nil
}

func appendOpenAIResponsesLiteAdditionalTools(input any, moved []any) ([]any, error) {
	var items []any
	switch typed := input.(type) {
	case nil:
		items = make([]any, 0, 1)
	case string:
		items = []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": typed}},
		}}
	case []any:
		items = typed
	default:
		return nil, newOpenAIResponsesLiteValidationError("input", "responses Lite tools and instructions require input to be a string or array")
	}
	// Later additional_tools items are historical declaration updates. They
	// must neither suppress the current prefix nor be moved ahead of history.
	var target map[string]any
	var existing []any
	if len(items) > 0 {
		first, _ := items[0].(map[string]any)
		if first["type"] == "additional_tools" {
			target = first
			var ok bool
			existing, ok = first["tools"].([]any)
			if !ok && first["tools"] != nil {
				return nil, newOpenAIResponsesLiteValidationError("input", "responses Lite input.additional_tools tools must be an array")
			}
		}
	}
	if len(moved) == 0 && target != nil {
		return items, nil
	}
	merged, err := mergeOpenAIResponsesLiteAdditionalTools(existing, moved)
	if err != nil {
		return nil, err
	}
	if target == nil {
		if merged == nil {
			merged = []any{}
		}
		prefix := map[string]any{"type": "additional_tools", "role": "developer", "tools": merged}
		return append([]any{prefix}, items...), nil
	}
	if reflect.DeepEqual(existing, merged) {
		return items, nil
	}
	result := append([]any(nil), items...)
	carrier := maps.Clone(target)
	carrier["tools"] = merged
	result[0] = carrier
	return result, nil
}

func mergeOpenAIResponsesLiteAdditionalTools(existing []any, moved []any) ([]any, error) {
	merged := append([]any(nil), existing...)
	seen := make(map[string]int, len(existing)+len(moved))
	for index, rawTool := range existing {
		if identity := openAIResponsesLiteToolIdentity(rawTool); identity != "" {
			seen[identity] = index
		}
	}
nextTool:
	for _, rawTool := range moved {
		identity := openAIResponsesLiteToolIdentity(rawTool)
		if identity == "" {
			for _, previous := range merged {
				if reflect.DeepEqual(previous, rawTool) {
					continue nextTool
				}
			}
		}
		if previous, exists := seen[identity]; identity != "" && exists {
			if reflect.DeepEqual(merged[previous], rawTool) {
				continue
			}
			if identity != "namespace\x00functions" {
				return nil, fmt.Errorf("responses Lite additional_tools conflicts with migrated %s", openAIResponsesLiteToolIdentityForError(rawTool))
			}
			oldNamespace := merged[previous].(map[string]any)
			newNamespace := rawTool.(map[string]any)
			oldTools, oldOK := oldNamespace["tools"].([]any)
			newTools, newOK := newNamespace["tools"].([]any)
			if !oldOK || !newOK {
				return nil, newOpenAIResponsesLiteValidationError("tools", "responses Lite functions namespace tools must be an array")
			}
			members, err := mergeOpenAIResponsesLiteAdditionalTools(oldTools, newTools)
			if err != nil {
				return nil, err
			}
			namespace := maps.Clone(oldNamespace)
			for key, value := range newNamespace {
				if key == "tools" || (key == "description" && strings.TrimSpace(firstNonEmptyString(value)) == "") {
					continue
				}
				namespace[key] = value
			}
			namespace["tools"] = members
			merged[previous] = namespace
			continue
		}
		if identity != "" {
			seen[identity] = len(merged)
		}
		merged = append(merged, rawTool)
	}
	return merged, nil
}

func openAIResponsesLiteToolIdentity(rawTool any) string {
	tool, ok := rawTool.(map[string]any)
	if !ok {
		return ""
	}
	toolType := strings.TrimSpace(firstNonEmptyString(tool["type"]))
	name := strings.TrimSpace(firstNonEmptyString(tool["name"]))
	if toolType == "" || name == "" {
		return ""
	}
	return toolType + "\x00" + name
}

func openAIResponsesLiteToolIdentityForError(rawTool any) string {
	tool, _ := rawTool.(map[string]any)
	return fmt.Sprintf("tool type %q name %q", strings.TrimSpace(firstNonEmptyString(tool["type"])), strings.TrimSpace(firstNonEmptyString(tool["name"])))
}

func normalizeOpenAIResponsesLiteToolsPayload(body []byte) ([]byte, bool, error) {
	var requestBody map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &requestBody); err != nil {
		return body, false, fmt.Errorf("decode responses Lite request body: %w", err)
	}
	// Preserve untouched request fields verbatim, including unknown extensions
	// and large integers. Do not reorder/rebuild the request for CLI cosmetics.
	fields := []string{"input", "tools", "instructions", "reasoning", "parallel_tool_calls"}
	before := make(map[string][]byte, len(fields))
	for _, field := range fields {
		if value, exists := requestBody[field]; exists {
			encoded, err := marshalOpenAIUpstreamJSON(value)
			if err != nil {
				return body, false, err
			}
			before[field] = encoded
		}
	}
	changed, err := normalizeOpenAIResponsesLiteTools(requestBody)
	if err != nil || !changed {
		return body, false, err
	}
	updated := body
	for _, field := range fields {
		value, exists := requestBody[field]
		if !exists {
			if _, existed := before[field]; existed {
				updated, err = sjson.DeleteBytes(updated, field)
			}
		} else {
			var encoded []byte
			encoded, err = marshalOpenAIUpstreamJSON(value)
			if err == nil && !bytes.Equal(before[field], encoded) {
				updated, err = sjson.SetRawBytes(updated, field, encoded)
			}
		}
		if err != nil {
			return body, false, fmt.Errorf("normalize responses Lite %s: %w", field, err)
		}
	}
	return updated, true, nil
}

func normalizeOpenAIResponsesLiteParallelToolCallsPayload(body []byte) ([]byte, bool, error) {
	var requestBody map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &requestBody); err != nil {
		return body, false, fmt.Errorf("decode responses Lite request body: %w", err)
	}
	changed, err := ensureOpenAIResponsesLiteParallelToolCalls(requestBody, false)
	if err != nil || !changed {
		return body, false, err
	}
	updated, err := sjson.SetBytes(body, "parallel_tool_calls", false)
	if err != nil {
		return body, false, fmt.Errorf("normalize responses Lite parallel_tool_calls: %w", err)
	}
	return updated, true, nil
}

func normalizeOpenAIResponsesLitePayloadForAccount(body []byte, account *Account) ([]byte, bool, error) {
	if account == nil || !account.IsOpenAI() {
		return body, false, nil
	}
	if account.IsOpenAIOAuthLike() {
		return normalizeOpenAIResponsesLiteToolsPayload(body)
	}
	return normalizeOpenAIResponsesLiteParallelToolCallsPayload(body)
}
