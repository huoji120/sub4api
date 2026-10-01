//go:build unit

package service

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestResponsesSearchServerInjectionAndChoice(t *testing.T) {
	for _, body := range []string{
		`{"input":"question","extension":9007199254740993}`,
		`{"tools":[],"tool_choice":"required"}`,
		`{"tools":[{"type":"function","name":"client_tool","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"client_tool"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			plan, err := prepareResponsesWebSearch([]byte(body))
			require.NoError(t, err)
			require.NotNil(t, plan)
			var before, after map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(body), &before))
			require.NoError(t, json.Unmarshal(plan.Body, &after))
			if raw := before["tool_choice"]; len(raw) > 0 {
				require.JSONEq(t, string(raw), string(after["tool_choice"]))
			}
			if raw := before["extension"]; len(raw) > 0 {
				require.Equal(t, string(raw), string(after["extension"]))
			}
			var oldTools, newTools []json.RawMessage
			if len(before["tools"]) > 0 {
				require.NoError(t, json.Unmarshal(before["tools"], &oldTools))
			}
			require.NoError(t, json.Unmarshal(after["tools"], &newTools))
			for i := range oldTools {
				require.JSONEq(t, string(oldTools[i]), string(newTools[i]))
			}
			var injected map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(newTools[len(newTools)-1], &injected))
			require.Equal(t, plan.FunctionName, responsesSearchString(injected["name"]))
			require.Equal(t, "function", responsesSearchString(injected["type"]))
		})
	}
	plan, err := prepareResponsesWebSearch([]byte(`{"tool_choice":"none","tools":[{"type":"web_search","user_location":{"type":"approximate"}}]}`))
	require.NoError(t, err)
	require.Nil(t, plan)
}

func TestResponsesSearchBuiltinTranslation(t *testing.T) {
	body := []byte(`{"stream":true,"max_tool_calls":2,"tools":[{"type":"function","name":"__sub4api_web_search"},{"type":"function","name":"__sub4api_web_search_1"},{"type":"web_search_preview","search_context_size":"high","external_web_access":true,"filters":{"allowed_domains":["example.com"]}}],"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"web_search_preview"},{"type":"function","name":"__sub4api_web_search"}]},"include":["web_search_call.action.sources","reasoning.encrypted_content"],"extension":{"integer":9007199254740993}}`)
	plan, err := prepareResponsesWebSearch(body)
	require.NoError(t, err)
	require.Equal(t, "__sub4api_web_search_2", plan.FunctionName)
	require.Equal(t, 2, plan.MaxCalls)
	require.Equal(t, 10, plan.MaxResults)
	require.Equal(t, []string{"example.com"}, plan.AllowedDomains)
	require.True(t, plan.OriginalStream)
	var after map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(plan.Body, &after))
	require.NotContains(t, after, "max_tool_calls")
	require.Equal(t, "false", string(after["stream"]))
	require.JSONEq(t, `["reasoning.encrypted_content"]`, string(after["include"]))
	require.JSONEq(t, `{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"__sub4api_web_search_2"},{"type":"function","name":"__sub4api_web_search"}]}`, string(after["tool_choice"]))
	require.Contains(t, string(plan.Body), "9007199254740993")
	forced, err := prepareResponsesWebSearch([]byte(`{"tool_choice":{"type":"web_search"},"tools":[{"type":"web_search"}]}`))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(forced.Body, &after))
	require.JSONEq(t, `{"type":"function","name":"__sub4api_web_search"}`, string(after["tool_choice"]))
}

func TestResponsesSearchRejectsUnsupportedConstraints(t *testing.T) {
	for _, declaration := range []string{
		`{"type":"web_search","user_location":{"type":"approximate"}}`,
		`{"type":"web_search","external_web_access":false}`,
		`{"type":"web_search","search_context_size":"huge"}`,
		`{"type":"web_search","filters":{"blocked_domains":["example.com"]}}`,
		`{"type":"web_search","filters":{"allowed_domains":["https://example.com/path"]}}`,
	} {
		_, err := prepareResponsesWebSearch([]byte(`{"tools":[` + declaration + `]}`))
		require.ErrorContains(t, err, "unsupported web_search")
	}
}

func TestResponsesSearchSSEPreservesFinalPayloadAndIndexes(t *testing.T) {
	response := json.RawMessage(`{"id":"resp_public","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"extension":9007199254740993,"usage":{"input_tokens":7,"output_tokens":9,"total_tokens":16,"provider_extra":true},"output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"latest","sources":[{"type":"url","url":"https://example.com"}]}},{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"reason"}],"encrypted_content":"opaque"},{"type":"message","id":"msg_1","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"answer","annotations":[{"type":"url_citation","url":"https://example.com","title":"Source","start_index":0,"end_index":6}],"extension":42},{"type":"refusal","refusal":"declined"}]},{"type":"function_call","id":"fc_1","call_id":"call_client","name":"client_tool","arguments":"{\"number\":9007199254740993}","status":"completed","namespace":"client"},{"type":"provider_item","id":"opaque_item","provider_extension":{"integer":9007199254740993}}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	require.NoError(t, writeResponsesWebSearchResponse(c, response, true))
	require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
	var events []map[string]json.RawMessage
	for _, frame := range strings.Split(recorder.Body.String(), "\n\n") {
		lines := strings.Split(frame, "\n")
		if len(lines) < 2 {
			continue
		}
		var event map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &event))
		require.Equal(t, strings.TrimPrefix(lines[0], "event: "), responsesSearchString(event["type"]))
		var sequence int
		require.NoError(t, json.Unmarshal(event["sequence_number"], &sequence))
		require.Equal(t, len(events), sequence)
		events = append(events, event)
	}
	require.Equal(t, "response.created", responsesSearchString(events[0]["type"]))
	require.Equal(t, "response.in_progress", responsesSearchString(events[1]["type"]))
	var final map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(response, &final))
	var output []json.RawMessage
	require.NoError(t, json.Unmarshal(final["output"], &output))
	addedIndex, doneIndex := 0, 0
	seen := make(map[string]bool)
	for _, event := range events {
		kind := responsesSearchString(event["type"])
		seen[kind] = true
		if kind == "response.output_item.added" || kind == "response.output_item.done" {
			var index int
			require.Contains(t, event, "output_index")
			require.NoError(t, json.Unmarshal(event["output_index"], &index))
			if kind == "response.output_item.added" {
				require.Equal(t, addedIndex, index)
				addedIndex++
			} else {
				require.Equal(t, doneIndex, index)
				require.JSONEq(t, string(output[index]), string(event["item"]))
				doneIndex++
			}
		}
	}
	require.Equal(t, len(output), doneIndex)
	for _, kind := range []string{"response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed", "response.output_text.annotation.added", "response.refusal.done", "response.reasoning_summary_text.done", "response.function_call_arguments.done"} {
		require.True(t, seen[kind], kind)
	}
	last := events[len(events)-1]
	require.Equal(t, "response.incomplete", responsesSearchString(last["type"]))
	require.JSONEq(t, string(response), string(last["response"]))
	require.Contains(t, string(last["response"]), "9007199254740993")
	unaryRecorder := httptest.NewRecorder()
	unary, _ := gin.CreateTestContext(unaryRecorder)
	require.NoError(t, writeResponsesWebSearchResponse(unary, response, false))
	require.Equal(t, string(response), unaryRecorder.Body.String())
}

func TestResponsesSearchRejectsUnserviceableBackgroundAndTokenBudgets(t *testing.T) {
	for _, body := range []string{
		`{"input":"hello","background":true}`,
		`{"input":"hello","max_output_tokens":0}`,
		`{"input":"hello","max_output_tokens":-1}`,
	} {
		_, err := prepareResponsesWebSearch([]byte(body))
		require.Error(t, err)
	}
	_, err := prepareResponsesWebSearch([]byte(`{"input":"hello","max_output_tokens":null}`))
	require.NoError(t, err)
}

func TestResponsesSearchHonorsAllowedToolsExclusionBeforeSearchOptions(t *testing.T) {
	body := `{"input":"hello","background":true,"tools":[{"type":"web_search","external_web_access":false},{"type":"function","name":"client_tool"}],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"client_tool"}]}}`
	plan, err := prepareResponsesWebSearch([]byte(body))
	require.NoError(t, err)
	require.Nil(t, plan)
}

func TestResponsesSearchAdditionalToolsAndStringNamesAvoidCollisions(t *testing.T) {
	body := `{"tools":["__sub4api_web_search"],"input":[{"type":"additional_tools","role":"developer","extension":9007199254740993,"tools":[{"type":"function","name":"__sub4api_web_search_1"},{"type":"web_search","filters":{"allowed_domains":["Example.COM"]}}]},{"role":"user","content":"latest"}]}`
	plan, err := prepareResponsesWebSearch([]byte(body))
	require.NoError(t, err)
	require.Equal(t, "__sub4api_web_search_2", plan.FunctionName)
	require.Equal(t, []string{"example.com"}, plan.AllowedDomains)
	require.Contains(t, string(plan.Body), "9007199254740993")
	var parsed map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(plan.Body, &parsed))
	var input []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(parsed["input"], &input))
	require.JSONEq(t, `[{"type":"function","name":"__sub4api_web_search_1"}]`, string(input[0]["tools"]))
	require.JSONEq(t, `"latest"`, string(input[1]["content"]))
	_, err = prepareResponsesWebSearch([]byte(`{"input":[{"type":"additional_tools","tools":[{"type":"web_search","external_web_access":false}]}]}`))
	require.ErrorContains(t, err, "external_web_access")
}

func TestResponsesSearchStreamKnownItemsHaveRequiredEmptyFields(t *testing.T) {
	response := json.RawMessage(`{"id":"resp_shapes","object":"response","created_at":1700000000,"status":"completed","output":[{"type":"reasoning","id":"rs_empty","encrypted_content":"opaque"},{"type":"message","id":"msg_shapes","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer"}]}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	require.NoError(t, writeResponsesWebSearchResponse(c, response, true))
	seen := make(map[string]bool)
	for _, frame := range strings.Split(recorder.Body.String(), "\n\n") {
		lines := strings.Split(frame, "\n")
		if len(lines) < 2 {
			continue
		}
		var event map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &event))
		seen[responsesSearchString(event["type"])] = true
		switch responsesSearchString(event["type"]) {
		case "response.output_item.added", "response.output_item.done":
			var item map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(event["item"], &item))
			if responsesSearchString(item["type"]) == "reasoning" {
				require.Equal(t, "[]", string(item["summary"]))
				require.Equal(t, `"opaque"`, string(item["encrypted_content"]))
			}
		case "response.content_part.added", "response.content_part.done":
			var part map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(event["part"], &part))
			require.Equal(t, "[]", string(part["annotations"]))
			require.Equal(t, "[]", string(part["logprobs"]))
		}
	}
	for _, required := range []string{"response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done", "response.completed"} {
		require.True(t, seen[required], required)
	}
}
