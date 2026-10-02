package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const responsesWebSearchFunctionPrefix = "__sub4api_web_search"

type responsesWebSearchPlan struct {
	Body               []byte
	FunctionName       string
	MaxCalls           int
	MaxResults         int
	AllowedDomains     []string
	OriginalTools      json.RawMessage
	OriginalToolChoice json.RawMessage
	OriginalStream     bool
}

// RawMessage maps preserve provider extensions and integer precision throughout
// the translation; only fields owned by the hosted-search protocol are changed.
func prepareResponsesWebSearch(body []byte) (*responsesWebSearchPlan, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return nil, fmt.Errorf("invalid Responses request JSON")
	}
	if responsesSearchString(root["tool_choice"]) == "none" {
		return nil, nil
	}
	var policy map[string]json.RawMessage
	if json.Unmarshal(root["tool_choice"], &policy) == nil && responsesSearchString(policy["type"]) == "allowed_tools" {
		var allowed []json.RawMessage
		_ = json.Unmarshal(policy["tools"], &allowed)
		searchAllowed := false
		for _, tool := range allowed {
			var declaration map[string]json.RawMessage
			if json.Unmarshal(tool, &declaration) == nil && responsesSearchBuiltin(responsesSearchString(declaration["type"])) {
				searchAllowed = true
			}
		}
		if !searchAllowed {
			return nil, nil
		}
	}
	if enabled := responsesSearchString(root["background"]); enabled == "true" || string(root["background"]) == "true" {
		return nil, fmt.Errorf("hosted web_search requires background=false; background jobs cannot run gateway-hosted tools")
	}
	if raw := root["max_output_tokens"]; len(raw) > 0 && string(raw) != "null" {
		var tokens int64
		if json.Unmarshal(raw, &tokens) != nil || tokens < 1 {
			return nil, fmt.Errorf("hosted web_search requires max_output_tokens to be a positive integer or null")
		}
	}
	var tools []json.RawMessage
	if raw := root["tools"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, fmt.Errorf("Responses tools must be an array")
		}
	}
	var liftErr error
	tools, liftErr = responsesSearchLiftAdditionalTools(root, tools)
	if liftErr != nil {
		return nil, liftErr
	}
	searchIndex := -1
	var declaration map[string]json.RawMessage
	for i, raw := range tools {
		var tool map[string]json.RawMessage
		if err := json.Unmarshal(raw, &tool); err != nil {
			continue
		}
		if responsesSearchBuiltin(responsesSearchString(tool["type"])) {
			if searchIndex >= 0 {
				return nil, fmt.Errorf("hosted web_search supports one web_search or web_search_preview declaration; remove duplicate search tools")
			}
			searchIndex, declaration = i, tool
		}
	}
	if searchIndex < 0 {
		searchIndex = len(tools)
		tools = append(tools, nil)
	}
	plan := &responsesWebSearchPlan{MaxCalls: 8, MaxResults: 5, OriginalTools: root["tools"], OriginalToolChoice: root["tool_choice"]}
	if raw := root["stream"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &plan.OriginalStream); err != nil {
			return nil, fmt.Errorf("Responses stream must be a boolean")
		}
	}
	if raw := root["max_tool_calls"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &plan.MaxCalls); err != nil || plan.MaxCalls < 1 {
			return nil, fmt.Errorf("hosted web_search requires max_tool_calls to be a positive integer")
		}
	}
	for key, raw := range declaration {
		switch key {
		case "type":
		case "search_context_size":
			switch responsesSearchString(raw) {
			case "low":
				plan.MaxResults = 3
			case "medium":
				plan.MaxResults = 5
			case "high":
				plan.MaxResults = 10
			default:
				return nil, fmt.Errorf("unsupported web_search search_context_size; use low, medium, or high")
			}
		case "external_web_access":
			var enabled bool
			if err := json.Unmarshal(raw, &enabled); err != nil || !enabled {
				return nil, fmt.Errorf("unsupported web_search external_web_access=false; hosted search requires external_web_access=true")
			}
		case "filters":
			var filters map[string]json.RawMessage
			if err := json.Unmarshal(raw, &filters); err != nil || filters == nil {
				return nil, fmt.Errorf("web_search filters must be an object containing allowed_domains")
			}
			for option, value := range filters {
				if option != "allowed_domains" {
					return nil, fmt.Errorf("unsupported web_search filters.%s; only allowed_domains is supported", option)
				}
				if err := json.Unmarshal(value, &plan.AllowedDomains); err != nil || plan.AllowedDomains == nil {
					return nil, fmt.Errorf("web_search filters.allowed_domains must be an array of domain strings")
				}
				for index, domain := range plan.AllowedDomains {
					if strings.TrimSpace(domain) == "" || strings.ContainsAny(domain, "/:?# \t\r\n") {
						return nil, fmt.Errorf("unsupported web_search allowed domain %q; use a hostname without scheme, path, or whitespace", domain)
					}
					plan.AllowedDomains[index] = strings.TrimSuffix(strings.ToLower(domain), ".")
				}
			}
		default:
			return nil, fmt.Errorf("unsupported web_search option %s; remove it to use hosted search", key)
		}
	}
	// Check names recursively, including functions inside namespace tools.
	names := make(map[string]bool)
	var collectNames func(json.RawMessage)
	collectNames = func(raw json.RawMessage) {
		var shortName string
		if json.Unmarshal(raw, &shortName) == nil {
			names[shortName] = true
			return
		}
		var array []json.RawMessage
		if json.Unmarshal(raw, &array) == nil {
			for _, tool := range array {
				collectNames(tool)
			}
			return
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) == nil && object != nil {
			if name := responsesSearchString(object["name"]); name != "" {
				names[name] = true
			}
			for _, key := range []string{"tools", "children", "function"} {
				if nested := object[key]; len(nested) > 0 {
					collectNames(nested)
				}
			}
		}
	}
	collectNames(root["tools"])
	var input []map[string]json.RawMessage
	if json.Unmarshal(root["input"], &input) == nil {
		for _, item := range input {
			if responsesSearchString(item["type"]) == "additional_tools" {
				collectNames(item["tools"])
			}
		}
	}
	plan.FunctionName = responsesWebSearchFunctionPrefix
	for suffix := 1; names[plan.FunctionName]; suffix++ {
		plan.FunctionName = fmt.Sprintf("%s_%d", responsesWebSearchFunctionPrefix, suffix)
	}
	function := map[string]any{
		"type": "function", "name": plan.FunctionName,
		"description": "Search the public web for current information. Supply a specific search query.",
		"parameters":  map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}, "additionalProperties": false},
		"strict":      true,
	}
	tools[searchIndex], _ = json.Marshal(function)
	root["tools"], _ = json.Marshal(tools)
	if len(root["tool_choice"]) > 0 {
		translated, err := responsesSearchTranslateChoice(root["tool_choice"], plan.FunctionName)
		if err != nil {
			return nil, err
		}
		root["tool_choice"] = translated
	}
	if raw := root["include"]; len(raw) > 0 {
		var includes []json.RawMessage
		if err := json.Unmarshal(raw, &includes); err != nil {
			return nil, fmt.Errorf("Responses include must be an array")
		}
		kept := make([]json.RawMessage, 0, len(includes))
		for _, entry := range includes {
			if responsesSearchString(entry) != "web_search_call.action.sources" {
				kept = append(kept, entry)
			}
		}
		root["include"], _ = json.Marshal(kept)
	}
	delete(root, "max_tool_calls")
	// Streaming clients get every model round relayed live; only buffered
	// clients need the single aggregated JSON response.
	if !plan.OriginalStream {
		root["stream"] = json.RawMessage("false")
	}
	plan.Body, _ = json.Marshal(root)
	return plan, nil
}

func responsesSearchBuiltin(kind string) bool {
	return kind == "web_search" || kind == "web_search_preview"
}

func responsesSearchString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func responsesSearchTranslateChoice(raw json.RawMessage, name string) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return raw, nil // Native string choices remain untouched.
	}
	if responsesSearchBuiltin(responsesSearchString(object["type"])) {
		object["type"] = json.RawMessage(`"function"`)
		object["name"], _ = json.Marshal(name)
	}
	if responsesSearchString(object["type"]) == "allowed_tools" {
		var tools []json.RawMessage
		if err := json.Unmarshal(object["tools"], &tools); err != nil {
			return nil, fmt.Errorf("Responses allowed_tools tool_choice.tools must be an array")
		}
		for i, tool := range tools {
			translated, err := responsesSearchTranslateChoice(tool, name)
			if err != nil {
				return nil, err
			}
			tools[i] = translated
		}
		object["tools"], _ = json.Marshal(tools)
	}
	return json.Marshal(object)
}

// Buffered hosted-search responses use the same event/data framing as native
// Responses. The final response and done items retain every raw extension.
func writeResponsesWebSearchResponse(c *gin.Context, response json.RawMessage, stream bool) error {
	if !stream {
		c.Header("Content-Type", "application/json")
		c.Status(http.StatusOK)
		_, err := c.Writer.Write(response)
		return err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(response, &root); err != nil || root == nil {
		return fmt.Errorf("invalid hosted-search response JSON")
	}
	status := responsesSearchString(root["status"])
	switch status {
	case "completed", "incomplete", "failed":
	default:
		return fmt.Errorf("unsupported terminal Responses status %q", status)
	}
	var items []json.RawMessage
	if len(root["output"]) > 0 {
		if err := json.Unmarshal(root["output"], &items); err != nil {
			return fmt.Errorf("invalid Responses output array: %w", err)
		}
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	sequence := 0
	emit := func(kind string, fields map[string]any) error {
		fields["type"], fields["sequence_number"] = kind, sequence
		data, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", kind, data); err != nil {
			return err
		}
		sequence++
		c.Writer.Flush()
		return nil
	}
	initial := responsesSearchCopy(root)
	initial["status"] = json.RawMessage(`"in_progress"`)
	initial["output"] = json.RawMessage(`[]`)
	initial["usage"] = json.RawMessage(`null`)
	initial["error"] = json.RawMessage(`null`)
	initial["incomplete_details"] = json.RawMessage(`null`)
	if _, ok := initial["output_text"]; ok {
		initial["output_text"] = json.RawMessage(`""`)
	}
	for _, kind := range []string{"response.created", "response.in_progress"} {
		if err := emit(kind, map[string]any{"response": initial}); err != nil {
			return err
		}
	}
	for index, raw := range items {
		if err := emitResponsesWebSearchItem(emit, index, raw); err != nil {
			return err
		}
	}
	return emit("response."+status, map[string]any{"response": response})
}

// emitResponsesWebSearchItem frames one completed output item as the native
// added/delta/done event sequence at the given public output_index.
func emitResponsesWebSearchItem(emit func(string, map[string]any) error, index int, raw json.RawMessage) error {
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil || item == nil {
		return fmt.Errorf("invalid Responses output item at index %d", index)
	}
	kind := responsesSearchString(item["type"])
	id := responsesSearchString(item["id"])
	fields := func() map[string]any { return map[string]any{"item_id": id, "output_index": index} }
	added := responsesSearchCopy(item)
	if _, ok := added["status"]; ok {
		added["status"] = json.RawMessage(`"in_progress"`)
	}
	switch kind {
	case "message":
		added["content"] = json.RawMessage(`[]`)
	case "reasoning":
		added["summary"] = json.RawMessage(`[]`)
		if _, ok := added["content"]; ok {
			added["content"] = json.RawMessage(`[]`)
		}
	case "function_call":
		added["arguments"] = json.RawMessage(`""`)
	case "custom_tool_call":
		added["input"] = json.RawMessage(`""`)
	}
	if err := emit("response.output_item.added", map[string]any{"output_index": index, "item": added}); err != nil {
		return err
	}
	switch kind {
	case "web_search_call":
		for _, phase := range []string{"in_progress", "searching"} {
			if err := emit("response.web_search_call."+phase, fields()); err != nil {
				return err
			}
		}
		if responsesSearchString(item["status"]) == "completed" {
			if err := emit("response.web_search_call.completed", fields()); err != nil {
				return err
			}
		}
	case "function_call", "custom_tool_call":
		field, prefix := "arguments", "response.function_call_arguments"
		if kind == "custom_tool_call" {
			field, prefix = "input", "response.custom_tool_call_input"
		}
		value := responsesSearchString(item[field])
		payload := fields()
		payload["delta"] = value
		if err := emit(prefix+".delta", payload); err != nil {
			return err
		}
		payload = fields()
		payload[field] = value
		for _, key := range []string{"name", "call_id", "namespace"} {
			if raw, ok := item[key]; ok {
				payload[key] = raw
			}
		}
		if err := emit(prefix+".done", payload); err != nil {
			return err
		}
	case "message", "reasoning":
		if err := responsesSearchEmitParts(item["content"], kind == "reasoning", false, fields, emit); err != nil {
			return err
		}
		if kind == "reasoning" {
			if err := responsesSearchEmitParts(item["summary"], true, true, fields, emit); err != nil {
				return err
			}
		}
	}
	if kind == "reasoning" && (len(item["summary"]) == 0 || string(item["summary"]) == "null") {
		item["summary"] = json.RawMessage(`[]`)
		raw = rawResponsesSearchJSON(item)
	}
	if err := emit("response.output_item.done", map[string]any{"output_index": index, "item": raw}); err != nil {
		return err
	}
	return nil
}

func responsesSearchCopy(source map[string]json.RawMessage) map[string]json.RawMessage {
	copy := make(map[string]json.RawMessage, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

func responsesSearchEmitParts(raw json.RawMessage, reasoning, summary bool, fields func() map[string]any, emit func(string, map[string]any) error) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return fmt.Errorf("invalid Responses content parts: %w", err)
	}
	for index, rawPart := range parts {
		var part map[string]json.RawMessage
		if err := json.Unmarshal(rawPart, &part); err != nil || part == nil {
			return fmt.Errorf("invalid Responses content part at index %d", index)
		}
		kind := responsesSearchString(part["type"])
		indexKey, partPrefix := "content_index", "response.content_part"
		if summary {
			indexKey, partPrefix = "summary_index", "response.reasoning_summary_part"
		}
		partFields := func() map[string]any { payload := fields(); payload[indexKey] = index; return payload }
		textField, textPrefix := "", ""
		switch kind {
		case "output_text":
			textField, textPrefix = "text", "response.output_text"
		case "refusal":
			textField, textPrefix = "refusal", "response.refusal"
		case "summary_text":
			if summary {
				textField, textPrefix = "text", "response.reasoning_summary_text"
			}
		case "reasoning_text":
			if reasoning {
				textField, textPrefix = "text", "response.reasoning_text"
			}
		}
		added := responsesSearchCopy(part)
		if textField != "" {
			added[textField] = json.RawMessage(`""`)
		}
		if kind == "output_text" {
			added["annotations"] = json.RawMessage(`[]`)
			added["logprobs"] = json.RawMessage(`[]`)
		}
		payload := partFields()
		payload["part"] = added
		if err := emit(partPrefix+".added", payload); err != nil {
			return err
		}
		if textField != "" {
			text := responsesSearchString(part[textField])
			payload = partFields()
			payload["delta"] = text
			if kind == "output_text" {
				payload["logprobs"] = json.RawMessage(`[]`)
				if logprobs, ok := part["logprobs"]; ok {
					payload["logprobs"] = logprobs
				}
			}
			if err := emit(textPrefix+".delta", payload); err != nil {
				return err
			}
			if kind == "output_text" && len(part["annotations"]) > 0 {
				var annotations []json.RawMessage
				if err := json.Unmarshal(part["annotations"], &annotations); err != nil {
					return fmt.Errorf("invalid Responses text annotations: %w", err)
				}
				for annotationIndex, annotation := range annotations {
					payload = partFields()
					payload["annotation_index"], payload["annotation"] = annotationIndex, annotation
					if err := emit("response.output_text.annotation.added", payload); err != nil {
						return err
					}
				}
			}
			payload = partFields()
			payload[textField] = text
			if kind == "output_text" {
				payload["annotations"] = json.RawMessage(`[]`)
				payload["logprobs"] = json.RawMessage(`[]`)
				if annotations, ok := part["annotations"]; ok {
					payload["annotations"] = annotations
				}
				if logprobs, ok := part["logprobs"]; ok {
					payload["logprobs"] = logprobs
				}
			}
			if err := emit(textPrefix+".done", payload); err != nil {
				return err
			}
		}
		payload = partFields()
		if kind == "output_text" {
			for _, key := range []string{"annotations", "logprobs"} {
				if len(part[key]) == 0 || string(part[key]) == "null" {
					part[key] = json.RawMessage(`[]`)
				}
			}
			rawPart = rawResponsesSearchJSON(part)
		}
		payload["part"] = rawPart
		if err := emit(partPrefix+".done", payload); err != nil {
			return err
		}
	}
	return nil
}

func responsesSearchAllowsCall(choice json.RawMessage, name string) bool {
	var policy map[string]json.RawMessage
	if json.Unmarshal(choice, &policy) != nil || policy == nil {
		return responsesSearchString(choice) != "none"
	}
	switch responsesSearchString(policy["type"]) {
	case "function":
		return responsesSearchString(policy["name"]) == name
	case "allowed_tools":
		if responsesSearchString(policy["mode"]) == "none" {
			return false
		}
		var tools []json.RawMessage
		_ = json.Unmarshal(policy["tools"], &tools)
		for _, tool := range tools {
			var allowed map[string]json.RawMessage
			if json.Unmarshal(tool, &allowed) == nil && responsesSearchString(allowed["type"]) == "function" && responsesSearchString(allowed["name"]) == name {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func responsesSearchContinuationChoice(choice json.RawMessage) json.RawMessage {
	var policy map[string]json.RawMessage
	if json.Unmarshal(choice, &policy) == nil && responsesSearchString(policy["type"]) == "allowed_tools" {
		policy["mode"] = json.RawMessage(`"auto"`)
		return rawResponsesSearchJSON(policy)
	}
	return json.RawMessage(`"auto"`)
}

// Lift hosted declarations without moving unrelated historical tool updates.
// Current top-level search options take precedence over historical declarations.
func responsesSearchLiftAdditionalTools(root map[string]json.RawMessage, tools []json.RawMessage) ([]json.RawMessage, error) {
	var input []json.RawMessage
	if json.Unmarshal(root["input"], &input) != nil {
		return tools, nil
	}
	var latest json.RawMessage
	changed := false
	for index, raw := range input {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil || responsesSearchString(item["type"]) != "additional_tools" {
			continue
		}
		var declarations []json.RawMessage
		if json.Unmarshal(item["tools"], &declarations) != nil {
			return nil, fmt.Errorf("Responses input additional_tools.tools must be an array")
		}
		kept := make([]json.RawMessage, 0, len(declarations))
		removed := false
		for _, declaration := range declarations {
			var tool map[string]json.RawMessage
			if json.Unmarshal(declaration, &tool) != nil || !responsesSearchBuiltin(responsesSearchString(tool["type"])) {
				kept = append(kept, declaration)
				continue
			}
			validation := rawResponsesSearchJSON(map[string]any{"tools": []json.RawMessage{declaration}})
			if _, err := prepareResponsesWebSearch(validation); err != nil {
				return nil, err
			}
			latest, removed = declaration, true
		}
		if removed {
			item["tools"] = rawResponsesSearchJSON(kept)
			input[index] = rawResponsesSearchJSON(item)
			changed = true
		}
	}
	if !changed {
		return tools, nil
	}
	root["input"] = rawResponsesSearchJSON(input)
	hasSearch := false
	for _, raw := range tools {
		var tool map[string]json.RawMessage
		if json.Unmarshal(raw, &tool) == nil && responsesSearchBuiltin(responsesSearchString(tool["type"])) {
			hasSearch = true
		}
	}
	if !hasSearch {
		tools = append(tools, latest)
		root["tools"] = rawResponsesSearchJSON(tools)
	}
	return tools, nil
}
