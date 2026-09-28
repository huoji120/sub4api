//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeOpenAIResponsesLiteTools_PrefixesToolsAndInstructionsBeforeHistory(t *testing.T) {
	body := []byte(`{
		"instructions":"Follow the user's requirements.",
		"tools":[
			{"type":"tool_search","execution":"client"},
			{"type":"function","name":"shell","defer_loading":true,"parameters":{"type":"object","properties":{"n":{"const":900719925474099312345}},"additionalProperties":false}},
			{"type":"namespace","name":"web","tools":[{"type":"function","name":"run"}]},
			{"type":"custom","name":"exec","format":{"type":"grammar","definition":"start: /.+/","syntax":"lark"}},
			{"type":"namespace","name":"functions","description":"Local tools","tools":[{"type":"function","name":"read"}]}
		],
		"input":[{"type":"message","role":"user","content":"hello"},{"type":"function_call_output","call_id":"call_1","output":"done"}],
		"tool_choice":{"type":"function","name":"shell"},
		"reasoning":{"effort":"high","summary":"concise","context":"current_turn"},
		"parallel_tool_calls":true
	}`)
	updated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(updated, "tools").Exists())
	require.Equal(t, "", gjson.GetBytes(updated, "instructions").String())
	require.Equal(t, "additional_tools", gjson.GetBytes(updated, "input.0.type").String())
	require.Equal(t, "developer", gjson.GetBytes(updated, "input.0.role").String())
	require.Equal(t, "tool_search", gjson.GetBytes(updated, "input.0.tools.0.type").String())
	require.Equal(t, "functions", gjson.GetBytes(updated, "input.0.tools.1.name").String())
	require.Equal(t, "Local tools", gjson.GetBytes(updated, "input.0.tools.1.description").String())
	require.Equal(t, "web", gjson.GetBytes(updated, "input.0.tools.2.name").String())
	members := gjson.GetBytes(updated, "input.0.tools.1.tools").Array()
	require.Len(t, members, 3)
	require.Equal(t, "shell", members[0].Get("name").String())
	require.True(t, members[0].Get("defer_loading").Bool())
	require.JSONEq(t, gjson.GetBytes(body, "tools.1.parameters").Raw, members[0].Get("parameters").Raw)
	require.Equal(t, "900719925474099312345", members[0].Get("parameters.properties.n.const").Raw)
	require.JSONEq(t, gjson.GetBytes(body, "tools.3").Raw, members[1].Raw)
	require.Equal(t, "read", members[2].Get("name").String())
	require.Equal(t, "developer", gjson.GetBytes(updated, "input.1.role").String())
	require.Equal(t, "Follow the user's requirements.", gjson.GetBytes(updated, "input.1.content.0.text").String())
	require.JSONEq(t, gjson.GetBytes(body, "input.0").Raw, gjson.GetBytes(updated, "input.2").Raw)
	require.JSONEq(t, gjson.GetBytes(body, "input.1").Raw, gjson.GetBytes(updated, "input.3").Raw)
	require.JSONEq(t, gjson.GetBytes(body, "tool_choice").Raw, gjson.GetBytes(updated, "tool_choice").Raw)
	require.Equal(t, "high", gjson.GetBytes(updated, "reasoning.effort").String())
	require.Equal(t, "concise", gjson.GetBytes(updated, "reasoning.summary").String())
	require.Equal(t, "all_turns", gjson.GetBytes(updated, "reasoning.context").String())
	require.False(t, gjson.GetBytes(updated, "parallel_tool_calls").Bool())
	repeated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(updated)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, updated, repeated)
}

func TestNormalizeOpenAIResponsesLiteTools_RejectsConflictingAdditionalTool(t *testing.T) {
	reqBody := map[string]any{
		"tools": []any{map[string]any{
			"type":  "namespace",
			"name":  "collaboration",
			"tools": []any{map[string]any{"type": "function", "name": "spawn_agent"}},
		}},
		"input": []any{map[string]any{
			"type": "additional_tools",
			"tools": []any{map[string]any{
				"type":  "namespace",
				"name":  "collaboration",
				"tools": []any{map[string]any{"type": "function", "name": "send_message"}},
			}},
		}},
	}

	changed, err := normalizeOpenAIResponsesLiteTools(reqBody)

	require.ErrorContains(t, err, `conflicts with migrated tool type "namespace" name "collaboration"`)
	require.False(t, changed)
	require.Len(t, reqBody["tools"], 1, "conflicts must not partially remove top-level tools")
}

func TestNormalizeOpenAIResponsesLiteTools_PreservesHistoricalToolDeclarationUpdates(t *testing.T) {
	body := []byte(`{"instructions":"Current guidance","tools":[{"type":"namespace","name":"web","tools":[{"type":"function","name":"current"}]}],"input":[{"type":"message","role":"user","content":"first turn"},{"type":"additional_tools","id":"at_history","tools":[{"type":"namespace","name":"web","tools":[{"type":"function","name":"historical"}]}]},{"type":"function_call_output","call_id":"call_1","output":"old result"}]}`)
	updated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "current", gjson.GetBytes(updated, "input.0.tools.0.tools.0.name").String())
	require.Equal(t, "Current guidance", gjson.GetBytes(updated, "input.1.content.0.text").String())
	require.Len(t, gjson.GetBytes(updated, "input").Array(), 5)
	for i := range 3 {
		require.JSONEq(t, gjson.GetBytes(body, "input").Array()[i].Raw, gjson.GetBytes(updated, "input").Array()[i+2].Raw)
	}
}

func TestNormalizeOpenAIResponsesLiteTools_ConvertsStringInput(t *testing.T) {
	reqBody := map[string]any{
		"input": "hello",
		"tools": []any{map[string]any{
			"type": "namespace",
			"name": "collaboration",
		}},
	}

	changed, err := normalizeOpenAIResponsesLiteTools(reqBody)

	require.NoError(t, err)
	require.True(t, changed)
	require.NotContains(t, reqBody, "tools")
	input := reqBody["input"].([]any)
	require.Len(t, input, 2)
	require.Equal(t, "additional_tools", input[0].(map[string]any)["type"])
	require.Equal(t, "message", input[1].(map[string]any)["type"])
	content := input[1].(map[string]any)["content"].([]any)
	require.Equal(t, "input_text", content[0].(map[string]any)["type"])
	require.Equal(t, "hello", content[0].(map[string]any)["text"])
}

func TestNormalizeOpenAIResponsesLiteTools_ForcesParallelToolCallsFalse(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
	}{
		{
			name: "top-level tools",
			body: map[string]any{
				"tools":               []any{map[string]any{"type": "function", "name": "shell"}},
				"parallel_tool_calls": true,
			},
		},
		{
			name: "input additional tools",
			body: map[string]any{
				"input": []any{map[string]any{
					"type":  "additional_tools",
					"tools": []any{map[string]any{"type": "namespace", "name": "collaboration"}},
				}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed, err := normalizeOpenAIResponsesLiteTools(tt.body)

			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, false, tt.body["parallel_tool_calls"])
		})
	}
}

func TestNormalizeOpenAIResponsesLiteTools_PinsParallelToolCallsWithoutTools(t *testing.T) {
	tests := []struct {
		name        string
		parallel    any
		include     bool
		wantChanged bool
	}{
		{name: "字段缺失", wantChanged: true},
		{name: "值为 true", parallel: true, include: true, wantChanged: true},
		{name: "值为 false", parallel: false, include: true, wantChanged: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqBody := map[string]any{"reasoning": map[string]any{"context": "all_turns"}}
			if tt.include {
				reqBody["parallel_tool_calls"] = tt.parallel
			}

			changed, err := normalizeOpenAIResponsesLiteTools(reqBody)

			require.NoError(t, err)
			require.Equal(t, tt.wantChanged, changed)
			require.Contains(t, reqBody, "parallel_tool_calls")
			require.Equal(t, false, reqBody["parallel_tool_calls"])
		})
	}
}

func TestNormalizeOpenAIResponsesLiteTools_RejectsNonBooleanParallelToolCalls(t *testing.T) {
	for _, value := range []any{"false", float64(0), nil, map[string]any{}} {
		reqBody := map[string]any{
			"tools":               []any{map[string]any{"type": "function", "name": "shell"}},
			"parallel_tool_calls": value,
		}

		changed, err := normalizeOpenAIResponsesLiteTools(reqBody)

		require.ErrorContains(t, err, "parallel_tool_calls to be a boolean")
		require.False(t, changed)
		require.Equal(t, value, reqBody["parallel_tool_calls"])
	}

	reqBody := map[string]any{"parallel_tool_calls": []any{}}
	changed, err := normalizeOpenAIResponsesLiteTools(reqBody)
	require.ErrorContains(t, err, "parallel_tool_calls to be a boolean")
	require.False(t, changed)
}

func TestNormalizeOpenAIResponsesLiteTools_ParallelToolCallsIsIdempotent(t *testing.T) {
	reqBody := map[string]any{
		"reasoning":           map[string]any{"context": "all_turns"},
		"tools":               []any{map[string]any{"type": "function", "name": "shell"}},
		"parallel_tool_calls": true,
	}

	changed, err := normalizeOpenAIResponsesLiteTools(reqBody)

	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, false, reqBody["parallel_tool_calls"])

	changed, err = normalizeOpenAIResponsesLiteTools(reqBody)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, false, reqBody["parallel_tool_calls"])
}

func TestNormalizeOpenAIResponsesLiteTools_EnsuresReasoningContext(t *testing.T) {
	tests := []struct {
		name      string
		reasoning any
	}{
		{name: "missing"},
		{name: "missing context", reasoning: map[string]any{"effort": "high"}},
		{name: "wrong context", reasoning: map[string]any{"effort": "medium", "context": "current_turn"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqBody := map[string]any{"input": "hello"}
			if tt.reasoning != nil {
				reqBody["reasoning"] = tt.reasoning
			}

			changed, err := normalizeOpenAIResponsesLiteTools(reqBody)

			require.NoError(t, err)
			require.True(t, changed)
			reasoning := reqBody["reasoning"].(map[string]any)
			require.Equal(t, "all_turns", reasoning["context"])
			if tt.name != "missing" {
				require.Equal(t, tt.reasoning.(map[string]any)["effort"], reasoning["effort"])
			}
		})
	}
}

func TestNormalizeOpenAIResponsesLiteTools_RejectsNonObjectReasoning(t *testing.T) {
	reqBody := map[string]any{"reasoning": "high"}

	changed, err := normalizeOpenAIResponsesLiteTools(reqBody)

	require.ErrorContains(t, err, "reasoning to be an object")
	require.False(t, changed)
	require.Equal(t, "high", reqBody["reasoning"])
}

func TestNormalizeOpenAIResponsesLiteTools_PreservesHostedAndProviderTools(t *testing.T) {
	for _, tool := range []string{
		`{"type":"web_search","external_web_access":false}`,
		`{"type":"image_generation","quality":"high"}`,
		`{"type":"provider_extension","configuration":{"enabled":true}}`,
		`"custom shorthand"`,
	} {
		body := []byte(`{"tools":[` + tool + `],"input":[]}`)
		updated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(body)
		require.NoError(t, err)
		require.True(t, changed)
		require.JSONEq(t, tool, gjson.GetBytes(updated, "input.0.tools.0").Raw)
		require.False(t, gjson.GetBytes(updated, "tools").Exists())
	}
}

func TestNormalizeOpenAIResponsesLiteToolsPayload_PreservesResponseCreateShape(t *testing.T) {
	body := []byte(`{
		"type":"response.create",
		"model":"gpt-5.6-terra",
		"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"},
		"input":[{"type":"message","role":"user","content":"hello"}],
		"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent"}]}],
		"tool_choice":{"type":"namespace","name":"collaboration"}
	}`)

	updated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(body)

	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "response.create", gjson.GetBytes(updated, "type").String())
	require.False(t, gjson.GetBytes(updated, "tools").Exists())
	require.Equal(t, "collaboration", gjson.GetBytes(updated, `input.#(type=="additional_tools").tools.0.name`).String())
	require.Equal(t, "namespace", gjson.GetBytes(updated, "tool_choice.type").String())
	require.True(t, gjson.GetBytes(updated, "parallel_tool_calls").Exists())
	require.False(t, gjson.GetBytes(updated, "parallel_tool_calls").Bool())
}

func TestNormalizeOpenAIResponsesLitePayloads_PreserveLargeSequence(t *testing.T) {
	body := []byte(`{
		"type":"response.create",
		"sequence":900719925474099312345,
		"tools":[{"type":"function","name":"lookup"}],
		"parallel_tool_calls":true
	}`)
	tests := []struct {
		name      string
		normalize func([]byte) ([]byte, bool, error)
	}{
		{name: "OAuth-like tools normalization", normalize: normalizeOpenAIResponsesLiteToolsPayload},
		{name: "API key parallel normalization", normalize: normalizeOpenAIResponsesLiteParallelToolCallsPayload},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updated, changed, err := tt.normalize(body)

			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, "900719925474099312345", gjson.GetBytes(updated, "sequence").Raw)
			require.True(t, gjson.GetBytes(updated, "parallel_tool_calls").Exists())
			require.False(t, gjson.GetBytes(updated, "parallel_tool_calls").Bool())
		})
	}
}

func TestApplyCodexOAuthTransform_PreservesLiteNamespaceToolChoice(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.6-terra",
		"input": []any{map[string]any{
			"type": "additional_tools",
			"tools": []any{map[string]any{
				"type": "namespace",
				"name": "collaboration",
			}},
		}},
		"tool_choice": map[string]any{"type": "namespace", "name": "collaboration"},
	}

	applyCodexOAuthTransform(reqBody, true, false)

	require.Equal(t, map[string]any{"type": "namespace", "name": "collaboration"}, reqBody["tool_choice"])
}

func TestOpenAIGatewayServiceForward_NormalizesResponsesLiteToolsForOAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, passthrough := range []bool{false, true} {
		name := "managed"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
			c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.1")
			c.Request.Header.Set(responsesLiteHeader, "true")
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_lite\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
						"data: [DONE]\n\n",
				)),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{
				ID: 501, Name: "responses-lite", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Concurrency: 1, Status: StatusActive, Schedulable: true, RateMultiplier: f64p(1),
				Credentials: map[string]any{"access_token": "oauth-token", "chatgpt_account_id": "chatgpt-account"},
				Extra:       map[string]any{"openai_passthrough": passthrough},
			}
			body := []byte(`{
				"model":"gpt-5.6-terra","stream":true,"instructions":"test",
				"reasoning":{"effort":"high","context":"current_turn"},
				"parallel_tool_calls":true,
				"tools":[
					{"type":"function","name":"shell","parameters":{"type":"object"}},
					{"type":"custom","name":"exec"},
					{"type":"tool_search"},
					{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object"}}]}
				],
				"input":[{"type":"message","role":"user","content":"hello"}],
				"tool_choice":{"type":"namespace","name":"collaboration"}
			}`)

			result, err := svc.Forward(context.Background(), c, account, body)

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, "true", upstream.lastReq.Header.Get(responsesLiteHeader))
			require.Equal(t, "high", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
			require.Equal(t, "all_turns", gjson.GetBytes(upstream.lastBody, "reasoning.context").String())
			require.False(t, gjson.GetBytes(upstream.lastBody, "tools").Exists())
			require.Equal(t, "functions", gjson.GetBytes(upstream.lastBody, "input.0.tools.0.name").String())
			require.Equal(t, "shell", gjson.GetBytes(upstream.lastBody, "input.0.tools.0.tools.0.name").String())
			require.Equal(t, "exec", gjson.GetBytes(upstream.lastBody, "input.0.tools.0.tools.1.name").String())
			require.Equal(t, "tool_search", gjson.GetBytes(upstream.lastBody, "input.0.tools.1.type").String())
			require.Equal(t, "collaboration", gjson.GetBytes(upstream.lastBody, "input.0.tools.2.name").String())
			require.Empty(t, gjson.GetBytes(upstream.lastBody, "instructions").String())
			require.Equal(t, "test", gjson.GetBytes(upstream.lastBody, "input.1.content.0.text").String())
			require.Equal(t, "hello", gjson.GetBytes(upstream.lastBody, "input.2.content").String())
			require.Equal(t, "namespace", gjson.GetBytes(upstream.lastBody, "tool_choice.type").String())
			require.Equal(t, "collaboration", gjson.GetBytes(upstream.lastBody, "tool_choice.name").String())
			require.True(t, gjson.GetBytes(upstream.lastBody, "parallel_tool_calls").Exists())
			require.False(t, gjson.GetBytes(upstream.lastBody, "parallel_tool_calls").Bool())

			badRec := httptest.NewRecorder()
			badCtx, _ := gin.CreateTestContext(badRec)
			badCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
			badCtx.Request.Header.Set(responsesLiteHeader, "true")
			badUpstream := &httpUpstreamRecorder{}
			svc.httpUpstream = badUpstream

			result, err = svc.Forward(context.Background(), badCtx, account, []byte(`{"model":"gpt-5.6-terra","tools":[{"type":"function","name":"shell"}],"parallel_tool_calls":"false"}`))

			require.ErrorContains(t, err, "parallel_tool_calls to be a boolean")
			require.Nil(t, result)
			require.Equal(t, http.StatusBadRequest, badRec.Code)
			require.Equal(t, "invalid_request_error", gjson.Get(badRec.Body.String(), "error.type").String())
			require.Equal(t, "parallel_tool_calls", gjson.Get(badRec.Body.String(), "error.param").String())
			require.Contains(t, gjson.Get(badRec.Body.String(), "error.message").String(), "parallel_tool_calls to be a boolean")
			require.Nil(t, badUpstream.lastReq)

			for _, malformed := range []struct {
				body      string
				wantParam string
			}{
				{body: `{"model":"gpt-5.6-terra","tools":{}}`, wantParam: "tools"},
				{body: `{"model":"gpt-5.6-terra","reasoning":[]}`, wantParam: "reasoning"},
			} {
				rec := httptest.NewRecorder()
				requestCtx, _ := gin.CreateTestContext(rec)
				requestCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
				requestCtx.Request.Header.Set(responsesLiteHeader, "true")

				result, err = svc.Forward(context.Background(), requestCtx, account, []byte(malformed.body))

				require.Error(t, err)
				require.Nil(t, result)
				require.Equal(t, http.StatusBadRequest, rec.Code)
				require.Equal(t, malformed.wantParam, gjson.Get(rec.Body.String(), "error.param").String())
			}
		})
	}
}

func TestOpenAIGatewayServiceForward_PinsParallelToolCallsForToollessResponsesLite(t *testing.T) {
	gin.SetMode(gin.TestMode)

	accountCases := []struct {
		name        string
		accountType string
		credentials map[string]any
	}{
		{name: "oauth", accountType: AccountTypeOAuth, credentials: map[string]any{"access_token": "oauth-token", "chatgpt_account_id": "chatgpt-account"}},
		{name: "apikey", accountType: AccountTypeAPIKey, credentials: map[string]any{"api_key": "sk-test"}},
	}
	parallelCases := []struct {
		name  string
		field string
	}{
		{name: "字段缺失"},
		{name: "值为 true", field: `,"parallel_tool_calls":true`},
		{name: "值为 false", field: `,"parallel_tool_calls":false`},
	}

	for _, accountCase := range accountCases {
		for _, passthrough := range []bool{false, true} {
			mode := "managed"
			if passthrough {
				mode = "passthrough"
			}
			for _, parallelCase := range parallelCases {
				name := accountCase.name + "/" + mode + "/" + parallelCase.name
				t.Run(name, func(t *testing.T) {
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
					c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.1")
					c.Request.Header.Set(responsesLiteHeader, "true")
					upstream := &httpUpstreamRecorder{resp: &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body: io.NopCloser(strings.NewReader(
							"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_lite\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
								"data: [DONE]\n\n",
						)),
					}}
					svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
					account := &Account{
						ID: 502, Name: "responses-lite-no-tools", Platform: PlatformOpenAI, Type: accountCase.accountType,
						Concurrency: 1, Status: StatusActive, Schedulable: true, RateMultiplier: f64p(1),
						Credentials: accountCase.credentials,
						Extra:       map[string]any{"openai_passthrough": passthrough},
					}
					body := []byte(`{
						"model":"gpt-5.6-terra","stream":true,"instructions":"test",
						"reasoning":{"effort":"high","context":"current_turn"},
						"input":[{"type":"message","role":"user","content":"hello"}]` + parallelCase.field + `
					}`)

					result, err := svc.Forward(context.Background(), c, account, body)

					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, "true", upstream.lastReq.Header.Get(responsesLiteHeader))
					require.Equal(t, gjson.False, gjson.GetBytes(upstream.lastBody, "parallel_tool_calls").Type, string(upstream.lastBody))
				})
			}
		}
	}
}

func TestOpenAIGatewayServiceForward_DisablesParallelToolCallsForResponsesLiteAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, passthrough := range []bool{false, true} {
		name := "managed"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
			c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.1")
			c.Request.Header.Set(responsesLiteHeader, "true")
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_lite\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
						"data: [DONE]\n\n",
				)),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{
				ID: 503, Name: "responses-lite-api-key", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Concurrency: 1, Status: StatusActive, Schedulable: true, RateMultiplier: f64p(1),
				Credentials: map[string]any{"api_key": "sk-test"},
				Extra:       map[string]any{"openai_passthrough": passthrough},
			}
			body := []byte(`{
				"model":"gpt-5.6-terra","stream":true,"instructions":"test",
				"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
				"parallel_tool_calls":true,
				"input":[{"type":"message","role":"user","content":"hello"}]
			}`)

			result, err := svc.Forward(context.Background(), c, account, body)

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, "true", upstream.lastReq.Header.Get(responsesLiteHeader))
			require.True(t, gjson.GetBytes(upstream.lastBody, "tools").IsArray())
			require.True(t, gjson.GetBytes(upstream.lastBody, "parallel_tool_calls").Exists())
			require.False(t, gjson.GetBytes(upstream.lastBody, "parallel_tool_calls").Bool())
		})
	}
}

func TestNormalizeOpenAIResponsesLiteTools_MergesExistingFunctionsWithoutDuplicatePrefix(t *testing.T) {
	body := []byte(`{"instructions":"Keep literal guidance","tools":[{"type":"function","name":"read","parameters":{"type":"object"}},{"type":"custom","name":"exec"}],"input":[{"type":"additional_tools","id":"at_existing","role":"developer","tools":[{"type":"namespace","name":"functions","description":"Existing tools","extension":true,"tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}]},{"type":"message","id":"msg_existing","role":"developer","content":[{"type":"input_text","text":"Keep literal guidance"}]},{"type":"custom_tool_call_output","call_id":"call_1","output":"ok"}]}`)
	updated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, gjson.GetBytes(updated, "input").Array(), 3)
	require.Equal(t, "at_existing", gjson.GetBytes(updated, "input.0.id").String())
	require.Equal(t, "msg_existing", gjson.GetBytes(updated, "input.1.id").String())
	require.Len(t, gjson.GetBytes(updated, "input.0.tools").Array(), 1)
	require.Len(t, gjson.GetBytes(updated, "input.0.tools.0.tools").Array(), 2)
	require.Equal(t, "Existing tools", gjson.GetBytes(updated, "input.0.tools.0.description").String())
	require.True(t, gjson.GetBytes(updated, "input.0.tools.0.extension").Bool())
	require.Equal(t, "exec", gjson.GetBytes(updated, "input.0.tools.0.tools.1.name").String())
	require.JSONEq(t, gjson.GetBytes(body, "input.2").Raw, gjson.GetBytes(updated, "input.2").Raw)
	repeated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(updated)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, updated, repeated)
}

func TestNormalizeOpenAIResponsesLiteTools_PreservesSerializedAndIncrementalRequests(t *testing.T) {
	for _, body := range []string{
		`{"instructions":"","input":[{"type":"additional_tools","id":"at_stable","role":"developer","tools":[]},{"type":"message","id":"msg_stable","role":"developer","content":[{"type":"input_text","text":"Instructions"}]}],"reasoning":{"context":"all_turns"},"parallel_tool_calls":false}`,
		`{"type":"response.create","previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"call_1","output":"900719925474099312345"},{"type":"custom_tool_call_output","call_id":"call_2","output":"ok"}],"reasoning":{"context":"all_turns"},"parallel_tool_calls":false}`,
	} {
		updated, changed, err := normalizeOpenAIResponsesLiteToolsPayload([]byte(body))
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, body, string(updated))
	}
}

func TestNormalizeOpenAIResponsesLiteTools_StripsOnlyImageContentDetails(t *testing.T) {
	body := []byte(`{
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"original"},{"type":"input_text","text":"detail remains","detail":"extension"}]},
			{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_image","image_url":"https://example.test/a.png","detail":"high"}]},
			{"type":"custom_tool_call_output","call_id":"call_2","output":[{"type":"input_image","image_url":"https://example.test/b.png","detail":"low"}]},
			{"type":"function_call_output","call_id":"call_3","output":"{\"type\":\"input_image\",\"detail\":\"high\"}"},
			{"type":"additional_tools","tools":[{"type":"function","name":"image_options","parameters":{"type":"object","properties":{"detail":{"const":"high"}}}}]},
			{"type":"compaction_trigger"}
		],
		"reasoning":{"context":"all_turns"},"parallel_tool_calls":false
	}`)
	updated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, gjson.GetBytes(updated, "input").Array(), 6)
	for _, path := range []string{"input.0.content.0.detail", "input.1.output.0.detail", "input.2.output.0.detail"} {
		require.False(t, gjson.GetBytes(updated, path).Exists(), path)
	}
	require.Equal(t, "extension", gjson.GetBytes(updated, "input.0.content.1.detail").String())
	for _, path := range []string{"input.0.content.0.image_url", "input.1.call_id", "input.2.call_id", "input.3", "input.4", "input.5"} {
		require.JSONEq(t, gjson.GetBytes(body, path).Raw, gjson.GetBytes(updated, path).Raw, path)
	}
}

func TestNormalizeOpenAIResponsesLiteTools_PreservesUnrelatedRawFields(t *testing.T) {
	body := []byte(`{ "metadata" : { "sequence" : 900719925474099312345, "escaped" : "\u003c" }, "text":{ "format":{"type":"json_schema","schema":{"const":900719925474099312345}} }, "instructions":"literal", "tools":[], "input":[] }`)
	updated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(body)
	require.NoError(t, err)
	require.True(t, changed)
	for _, path := range []string{"metadata", "text"} {
		require.Equal(t, gjson.GetBytes(body, path).Raw, gjson.GetBytes(updated, path).Raw)
	}
}

func TestNormalizeOpenAIResponsesLitePayloadForAccount_IsolatesOAuthAndPATSemantics(t *testing.T) {
	body := []byte(`{"instructions":"literal","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.test/image.png","detail":"original"}]}],"parallel_tool_calls":false}`)
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken, AccountTypeAPIKey} {
		t.Run(accountType, func(t *testing.T) {
			account := &Account{Platform: PlatformOpenAI, Type: accountType}
			updated, changed, err := normalizeOpenAIResponsesLitePayloadForAccount(body, account)
			require.NoError(t, err)
			if accountType == AccountTypeAPIKey {
				require.False(t, changed)
				require.Equal(t, body, updated)
				return
			}
			require.True(t, changed)
			require.Equal(t, "read", gjson.GetBytes(updated, "input.0.tools.0.tools.0.name").String())
			require.Equal(t, "literal", gjson.GetBytes(updated, "input.1.content.0.text").String())
			require.False(t, gjson.GetBytes(updated, "input.2.content.0.detail").Exists())
		})
	}
}

func TestNormalizeOpenAIResponsesLiteTools_CompactDoesNotGainStreamingFields(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-terra","instructions":"Compact this history","tools":[],"input":[{"type":"compaction","encrypted_content":"opaque_history"}],"prompt_cache_key":"thread_1"}`)
	updated, _, err := normalizeOpenAIResponsesLiteToolsPayload(body)
	require.NoError(t, err)
	require.Equal(t, "Compact this history", gjson.GetBytes(updated, "input.1.content.0.text").String())
	require.Equal(t, "opaque_history", gjson.GetBytes(updated, "input.2.encrypted_content").String())
	for _, field := range []string{"stream", "store", "include"} {
		require.False(t, gjson.GetBytes(updated, field).Exists(), field)
	}
}

func TestNormalizeOpenAIResponsesLiteTools_RejectsInvalidPrefixWithoutMutatingRequest(t *testing.T) {
	for _, body := range []string{
		`{"instructions":17,"tools":[],"input":[]}`,
		`{"instructions":"literal","tools":[],"input":17}`,
		`{"tools":[{"type":"function","name":"read","parameters":{"const":2}}],"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"read","parameters":{"const":1}}]}]}]}`,
	} {
		var request map[string]any
		require.NoError(t, decodeOpenAIJSONUseNumber([]byte(body), &request))
		changed, err := normalizeOpenAIResponsesLiteTools(request)
		require.Error(t, err)
		require.False(t, changed)
		after, err := marshalOpenAIUpstreamJSON(request)
		require.NoError(t, err)
		require.JSONEq(t, body, string(after))
	}
}

func TestNormalizeOpenAIResponsesLiteTools_DeduplicatesExistingNamelessTools(t *testing.T) {
	body := []byte(`{"tools":[{"type":"tool_search","execution":"client"},{"type":"web_search","external_web_access":false}],"input":[{"type":"additional_tools","id":"at_existing","role":"developer","tools":[{"type":"tool_search","execution":"client"},{"type":"web_search","external_web_access":false}]},{"type":"function_call_output","call_id":"call_1","output":"done"}]}`)
	updated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, gjson.GetBytes(updated, "input").Array(), 2)
	require.Len(t, gjson.GetBytes(updated, "input.0.tools").Array(), 2)
	require.Equal(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(updated, "input").Raw)
	require.False(t, gjson.GetBytes(updated, "tools").Exists())
}
