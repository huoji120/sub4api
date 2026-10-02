//go:build unit

package service

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/websearch"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type hostedSearchSSEEvent struct {
	Kind string
	Data map[string]any
}

func writeHostedSearchSSE(c *gin.Context, events []map[string]any) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.WriteHeader(http.StatusOK)
	for index, event := range events {
		event["sequence_number"] = index
		data, _ := json.Marshal(event)
		// Split each event across writes to exercise the incremental parser.
		frame := fmt.Sprintf("event: %s\ndata: %s\n\n", event["type"], data)
		half := len(frame) / 2
		_, _ = c.Writer.WriteString(frame[:half])
		_, _ = c.Writer.WriteString(frame[half:])
		c.Writer.Flush()
	}
}

func hostedSearchRoundEvents(id string, items []map[string]any) []map[string]any {
	response := map[string]any{"id": id, "object": "response", "created_at": 1700000000, "model": "test-model", "status": "in_progress", "output": []any{}, "tools": []any{map[string]any{"type": "function", "name": responsesWebSearchFunctionPrefix}}}
	events := []map[string]any{
		{"type": "response.created", "response": response},
		{"type": "response.in_progress", "response": response},
	}
	for index, item := range items {
		added := map[string]any{}
		for key, value := range item {
			added[key] = value
		}
		added["status"] = "in_progress"
		events = append(events, map[string]any{"type": "response.output_item.added", "output_index": index, "item": added})
		switch item["type"] {
		case "message":
			text := item["content"].([]any)[0].(map[string]any)["text"].(string)
			events = append(events,
				map[string]any{"type": "response.output_text.delta", "output_index": index, "item_id": item["id"], "content_index": 0, "delta": text},
				map[string]any{"type": "response.output_text.done", "output_index": index, "item_id": item["id"], "content_index": 0, "text": text},
			)
		case "function_call":
			events = append(events,
				map[string]any{"type": "response.function_call_arguments.delta", "output_index": index, "item_id": item["id"], "delta": item["arguments"]},
				map[string]any{"type": "response.function_call_arguments.done", "output_index": index, "item_id": item["id"], "arguments": item["arguments"]},
			)
		}
		events = append(events, map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
	}
	final := map[string]any{}
	for key, value := range response {
		final[key] = value
	}
	output := make([]any, 0, len(items))
	for _, item := range items {
		output = append(output, item)
	}
	final["status"], final["output"] = "completed", output
	final["usage"] = map[string]any{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8}
	return append(events, map[string]any{"type": "response.completed", "response": final})
}

func readHostedSearchSSE(t *testing.T, body string) []hostedSearchSSEEvent {
	t.Helper()
	var events []hostedSearchSSEEvent
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 1024), 1<<20)
	kind := ""
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			kind = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var data map[string]any
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data))
			require.Equal(t, kind, data["type"])
			events = append(events, hostedSearchSSEEvent{Kind: kind, Data: data})
		}
	}
	return events
}

func hostedSearchSSEKinds(events []hostedSearchSSEEvent) []string {
	kinds := make([]string, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

func TestHostedResponsesSearchStreamsEveryRoundLive(t *testing.T) {
	body := `{"model":"test-model","input":"latest release","stream":true,"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}]}`
	c, recorder := hostedSearchTestContext(t, body, 401)
	rounds := 0
	forward := func(next []byte) (*OpenAIForwardResult, error) {
		rounds++
		require.True(t, gjson.GetBytes(next, "stream").Bool(), "streaming clients keep upstream streaming")
		if rounds == 1 {
			reasoning := map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{}}
			call := map[string]any{"type": "function_call", "id": "fc_1", "call_id": "search_1", "name": gjson.GetBytes(next, "tools.1.name").String(), "arguments": `{"query":"release notes"}`, "status": "completed"}
			writeHostedSearchSSE(c, hostedSearchRoundEvents("resp_round_1", []map[string]any{reasoning, call}))
			return &OpenAIForwardResult{ResponseID: "resp_round_1", Usage: OpenAIUsage{InputTokens: 5, OutputTokens: 3}}, nil
		}
		require.Equal(t, "search_1", gjson.GetBytes(next, "input.3.call_id").String())
		message := map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "见 https://example.org/r", "annotations": []any{}}}}
		writeHostedSearchSSE(c, hostedSearchRoundEvents("resp_round_2", []map[string]any{message}))
		return &OpenAIForwardResult{ResponseID: "resp_round_2", Usage: OpenAIUsage{InputTokens: 7, OutputTokens: 4}}, nil
	}
	search := func(context.Context, *Account, string, *responsesWebSearchPlan) (*websearch.SearchResponse, error) {
		return &websearch.SearchResponse{Results: []websearch.SearchResult{{URL: "https://example.org/r", Title: "R"}}}, nil
	}
	result, err := forwardHostedResponsesWebSearch(context.Background(), c, &Account{}, []byte(body), forward, mergeOpenAIWebSearchResult, search)
	require.NoError(t, err)
	require.True(t, result.Stream)
	require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
	require.NotContains(t, recorder.Body.String(), responsesWebSearchFunctionPrefix)

	events := readHostedSearchSSE(t, recorder.Body.String())
	kinds := hostedSearchSSEKinds(events)
	require.Equal(t, "response.created", kinds[0])
	require.Equal(t, "response.completed", kinds[len(kinds)-1])
	count := func(kind string) int {
		n := 0
		for _, k := range kinds {
			if k == kind {
				n++
			}
		}
		return n
	}
	require.Equal(t, 1, count("response.created"))
	require.Equal(t, 1, count("response.in_progress"))
	require.Equal(t, 1, count("response.completed"))
	require.Equal(t, 0, count("response.function_call_arguments.delta"))
	for _, kind := range []string{"response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed"} {
		require.Equal(t, 1, count(kind), kind)
	}
	for index, event := range events {
		require.EqualValues(t, index, event.Data["sequence_number"])
	}
	// reasoning=0, web_search_call=1 (in place of the internal call), message=2.
	indexOf := map[string]float64{}
	for _, event := range events {
		if event.Kind == "response.output_item.done" {
			item := event.Data["item"].(map[string]any)
			indexOf[item["type"].(string)] = event.Data["output_index"].(float64)
			if item["type"] == "message" {
				annotations := item["content"].([]any)[0].(map[string]any)["annotations"].([]any)
				require.Len(t, annotations, 1, "live message done carries citations")
			}
		}
	}
	require.Equal(t, map[string]float64{"reasoning": 0, "web_search_call": 1, "message": 2}, indexOf)

	created := events[0].Data["response"].(map[string]any)
	terminal := events[len(events)-1].Data["response"].(map[string]any)
	require.Equal(t, "resp_round_1", created["id"])
	require.Equal(t, "resp_round_1", terminal["id"], "one public id for the whole exchange")
	require.Len(t, terminal["output"], 3)
	require.Equal(t, "web_search", terminal["tools"].([]any)[1].(map[string]any)["type"])

	continuation := `{"model":"test-model","previous_response_id":"resp_round_1","input":"next","stream":true}`
	continued, _ := hostedSearchTestContext(t, continuation, 401)
	_, err = forwardHostedResponsesWebSearch(context.Background(), continued, &Account{}, []byte(continuation), func(next []byte) (*OpenAIForwardResult, error) {
		require.Equal(t, "resp_round_2", gjson.GetBytes(next, "previous_response_id").String(), "public id resolves to the last round")
		writeHostedSearchSSE(continued, hostedSearchRoundEvents("resp_next", nil))
		return &OpenAIForwardResult{ResponseID: "resp_next"}, nil
	}, mergeOpenAIWebSearchResult, search)
	require.NoError(t, err)
}

func TestHostedResponsesSearchStreamsSingleRoundTransparently(t *testing.T) {
	body := `{"model":"test-model","input":"hi","stream":true}`
	c, recorder := hostedSearchTestContext(t, body, 402)
	forward := func(next []byte) (*OpenAIForwardResult, error) {
		message := map[string]any{"type": "message", "id": "msg_hi", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Hi!", "annotations": []any{}}}}
		writeHostedSearchSSE(c, hostedSearchRoundEvents("resp_hi", []map[string]any{message}))
		return &OpenAIForwardResult{ResponseID: "resp_hi"}, nil
	}
	_, err := forwardHostedResponsesWebSearch(context.Background(), c, &Account{}, []byte(body), forward, mergeOpenAIWebSearchResult, executeResponsesWebSearch)
	require.NoError(t, err)
	events := readHostedSearchSSE(t, recorder.Body.String())
	require.Equal(t, []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.output_text.delta", "response.output_text.done", "response.output_item.done",
		"response.completed",
	}, hostedSearchSSEKinds(events))
	require.Equal(t, "resp_hi", events[len(events)-1].Data["response"].(map[string]any)["id"])
	require.Equal(t, "Hi!", events[3].Data["delta"])
}

func TestHostedResponsesSearchStreamingReplaysJSONRound(t *testing.T) {
	body := `{"model":"test-model","input":"hi","stream":true}`
	c, recorder := hostedSearchTestContext(t, body, 403)
	forward := func(next []byte) (*OpenAIForwardResult, error) {
		message := json.RawMessage(`{"type":"message","id":"msg_json","role":"assistant","status":"completed","content":[{"type":"output_text","text":"from json","annotations":[]}]}`)
		return hostedSearchTestResponse(c, "resp_json", []json.RawMessage{message}, 1, 1), nil
	}
	_, err := forwardHostedResponsesWebSearch(context.Background(), c, &Account{}, []byte(body), forward, mergeOpenAIWebSearchResult, executeResponsesWebSearch)
	require.NoError(t, err)
	events := readHostedSearchSSE(t, recorder.Body.String())
	kinds := hostedSearchSSEKinds(events)
	require.Equal(t, "response.created", kinds[0])
	require.Contains(t, kinds, "response.output_text.delta")
	require.Equal(t, "response.completed", kinds[len(kinds)-1])
	require.Equal(t, "resp_json", events[0].Data["response"].(map[string]any)["id"])
}

func TestHostedResponsesSearchStreamingKeepsFirstRoundHTTPError(t *testing.T) {
	body := `{"model":"test-model","input":"hi","stream":true}`
	c, recorder := hostedSearchTestContext(t, body, 404)
	forward := func(next []byte) (*OpenAIForwardResult, error) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"message": "slow down"}})
		return nil, fmt.Errorf("upstream 429")
	}
	_, err := forwardHostedResponsesWebSearch(context.Background(), c, &Account{}, []byte(body), forward, mergeOpenAIWebSearchResult, executeResponsesWebSearch)
	require.Error(t, err)
	require.Equal(t, http.StatusTooManyRequests, recorder.Code, "nothing was streamed, so the HTTP error and failover stay intact")
	require.NotContains(t, recorder.Body.String(), "event:")
}

func TestHostedResponsesSearchUpstreamBreakIsServerError(t *testing.T) {
	body := `{"model":"test-model","input":"hi","stream":true}`
	c, recorder := hostedSearchTestContext(t, body, 405)
	forward := func(next []byte) (*OpenAIForwardResult, error) {
		events := hostedSearchRoundEvents("resp_cut", nil)
		writeHostedSearchSSE(c, events[:2]) // created + in_progress, then the upstream dies
		return nil, fmt.Errorf("stream read error: unexpected EOF")
	}
	_, err := forwardHostedResponsesWebSearch(context.Background(), c, &Account{}, []byte(body), forward, mergeOpenAIWebSearchResult, executeResponsesWebSearch)
	require.Error(t, err)
	events := readHostedSearchSSE(t, recorder.Body.String())
	last := events[len(events)-1]
	require.Equal(t, "response.failed", last.Kind)
	require.Equal(t, "server_error", last.Data["response"].(map[string]any)["error"].(map[string]any)["code"])
	require.Equal(t, "resp_cut", last.Data["response"].(map[string]any)["id"])
}

func TestHostedResponsesSearchErrorCodeSeparatesToolFailures(t *testing.T) {
	require.Equal(t, "web_search_error", responsesWebSearchErrorCode(&hostedResponsesWebSearchProviderError{err: fmt.Errorf("provider down")}))
	require.Equal(t, "web_search_error", responsesWebSearchErrorCode(&hostedResponsesWebSearchCallError{err: fmt.Errorf("bad call")}))
	require.Equal(t, "server_error", responsesWebSearchErrorCode(fmt.Errorf("stream read error: unexpected EOF")))
}
