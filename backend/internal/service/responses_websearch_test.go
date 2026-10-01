//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/websearch"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func hostedSearchTestContext(t *testing.T, body string, keyID int64) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	c.Set("api_key", &APIKey{ID: keyID, UserID: 7})
	return c, recorder
}

func hostedSearchTestResponse(c *gin.Context, id string, output []json.RawMessage, in, out int) *OpenAIForwardResult {
	c.Data(http.StatusOK, "application/json", rawResponsesSearchJSON(map[string]any{
		"id": id, "object": "response", "created_at": 1700000000, "model": "test-model", "status": "completed", "output": output,
		"usage": map[string]any{"input_tokens": in, "output_tokens": out, "total_tokens": in + out, "output_tokens_details": map[string]int{"reasoning_tokens": 1}},
	}))
	return &OpenAIForwardResult{ResponseID: id, Model: "test-model", Usage: OpenAIUsage{InputTokens: in, OutputTokens: out}}
}

func hostedSearchTestCall(body []byte, callID string) json.RawMessage {
	tools := gjson.GetBytes(body, "tools").Array()
	for _, tool := range tools {
		name := tool.Get("name").String()
		if strings.HasPrefix(name, responsesWebSearchFunctionPrefix) {
			return rawResponsesSearchJSON(map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": name, "arguments": `{"query":"actual model search query"}`, "status": "completed"})
		}
	}
	return nil
}

func TestHostedResponsesSearchProvidesToolAndContinuesWithEvidence(t *testing.T) {
	body := `{"model":"test-model","input":"latest release","previous_response_id":"resp_anchor","tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}],"max_output_tokens":20,"metadata":{"large":9007199254740993}}`
	c, recorder := hostedSearchTestContext(t, body, 301)
	modelRounds, searches := 0, 0
	forward := func(next []byte) (*OpenAIForwardResult, error) {
		modelRounds++
		require.Equal(t, "9007199254740993", gjson.GetBytes(next, "metadata.large").Raw)
		require.Equal(t, "resp_anchor", gjson.GetBytes(next, "previous_response_id").String())
		require.False(t, gjson.GetBytes(next, "stream").Bool())
		require.Equal(t, "read_file", gjson.GetBytes(next, "tools.0.name").String())
		if modelRounds == 1 {
			c.Writer.Header().Set("X-Upstream-Metadata", "round-1")
			c.Writer.Header().Set("Content-Length", "999")
			first := 7
			result := hostedSearchTestResponse(c, "resp_search", []json.RawMessage{hostedSearchTestCall(next, "search_1")}, 10, 4)
			result.FirstTokenMs = &first
			return result, nil
		}
		require.Equal(t, int64(16), gjson.GetBytes(next, "max_output_tokens").Int())
		require.Equal(t, "actual model search query", gjson.Get(gjson.GetBytes(next, "input.2.output").String(), "query").String())
		require.Equal(t, "verified provider snippet", gjson.Get(gjson.GetBytes(next, "input.2.output").String(), "results.0.snippet").String())
		message := json.RawMessage(`{"type":"message","id":"msg_answer","role":"assistant","status":"completed","content":[{"type":"output_text","text":"发布见 https://example.org/release","annotations":[]}]}`)
		second := hostedSearchTestResponse(c, "resp_final", []json.RawMessage{message}, 15, 5)
		second.FirstTokenMs = func() *int { value := 3; return &value }()
		return second, nil
	}
	search := func(_ context.Context, _ *Account, query string, _ *responsesWebSearchPlan) (*websearch.SearchResponse, error) {
		searches++
		require.Equal(t, "actual model search query", query)
		return &websearch.SearchResponse{Results: []websearch.SearchResult{{URL: "https://example.org/release", Title: "Release", Snippet: "verified provider snippet"}}}, nil
	}
	result, err := forwardHostedResponsesWebSearch(context.Background(), c, &Account{}, []byte(body), forward, mergeOpenAIWebSearchResult, search)
	require.NoError(t, err)
	require.Equal(t, 2, modelRounds)
	require.Equal(t, 1, searches)
	require.Equal(t, 25, result.Usage.InputTokens)
	require.Equal(t, 9, result.Usage.OutputTokens)
	require.NotNil(t, result.FirstTokenMs)
	require.GreaterOrEqual(t, *result.FirstTokenMs, 7)
	require.Equal(t, "resp_final", gjson.Get(recorder.Body.String(), "id").String())
	require.Equal(t, "web_search_call", gjson.Get(recorder.Body.String(), "output.0.type").String())
	require.Equal(t, "https://example.org/release", gjson.Get(recorder.Body.String(), "output.0.action.sources.0.url").String())
	require.Equal(t, "url_citation", gjson.Get(recorder.Body.String(), "output.1.content.0.annotations.0.type").String())
	require.Equal(t, int64(4), gjson.Get(recorder.Body.String(), "output.1.content.0.annotations.0.start_index").Int())
	require.Equal(t, int64(34), gjson.Get(recorder.Body.String(), "usage.total_tokens").Int())
	require.Equal(t, int64(20), gjson.Get(recorder.Body.String(), "max_output_tokens").Int())
	require.Equal(t, "round-1", recorder.Header().Get("X-Upstream-Metadata"))
	require.Empty(t, recorder.Header().Get("Content-Length"))
	require.NotContains(t, recorder.Body.String(), responsesWebSearchFunctionPrefix)
}

func TestHostedResponsesSearchMixedCallsPreserveClientAndScopedReplay(t *testing.T) {
	body := `{"model":"test-model","input":"look up release and read local file","tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}]}`
	c, recorder := hostedSearchTestContext(t, body, 302)
	clientCall := json.RawMessage(`{"type":"function_call","id":"fc_client","call_id":"client_1","name":"read_file","arguments":"{}","status":"completed"}`)
	forward := func(next []byte) (*OpenAIForwardResult, error) {
		return hostedSearchTestResponse(c, "resp_mixed", []json.RawMessage{hostedSearchTestCall(next, "search_mixed"), clientCall}, 5, 3), nil
	}
	search := func(context.Context, *Account, string, *responsesWebSearchPlan) (*websearch.SearchResponse, error) {
		return &websearch.SearchResponse{Results: []websearch.SearchResult{{URL: "https://example.org", Snippet: "retained search evidence"}}}, nil
	}
	_, err := forwardHostedResponsesWebSearch(context.Background(), c, &Account{}, []byte(body), forward, mergeOpenAIWebSearchResult, search)
	require.NoError(t, err)
	require.Equal(t, "read_file", gjson.Get(recorder.Body.String(), "output.1.name").String())
	publicSearch := gjson.Get(recorder.Body.String(), "output.0").Raw

	continuation := `{"model":"test-model","previous_response_id":"resp_mixed","input":[{"type":"function_call_output","call_id":"client_1","output":"local evidence"}]}`
	continued, continuedRecorder := hostedSearchTestContext(t, continuation, 302)
	_, err = forwardHostedResponsesWebSearch(context.Background(), continued, &Account{}, []byte(continuation), func(next []byte) (*OpenAIForwardResult, error) {
		require.Equal(t, "local evidence", gjson.GetBytes(next, "input.0.output").String())
		require.Equal(t, "search_mixed", gjson.GetBytes(next, "input.1.call_id").String())
		require.Contains(t, gjson.GetBytes(next, "input.1.output").String(), "retained search evidence")
		return hostedSearchTestResponse(continued, "resp_followup", []json.RawMessage{}, 8, 1), nil
	}, mergeOpenAIWebSearchResult, search)
	require.NoError(t, err)
	require.Equal(t, "completed", gjson.Get(continuedRecorder.Body.String(), "status").String())

	replay, err := responsesWebSearchInput(json.RawMessage("["+publicSearch+"]"), responsesWebSearchScope(c))
	require.NoError(t, err)
	require.Equal(t, "search_mixed", gjson.GetBytes(replay[0], "call_id").String())
	require.Contains(t, gjson.GetBytes(replay[1], "output").String(), "retained search evidence")
	other, _ := hostedSearchTestContext(t, body, 999)
	_, err = responsesWebSearchInput(json.RawMessage("["+publicSearch+"]"), responsesWebSearchScope(other))
	require.ErrorContains(t, err, "another API key")
}

func TestHostedResponsesSearchFailureAndLimitNeverFabricateAnswer(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		providerError bool
	}{
		{name: "provider_failure", providerError: true},
		{name: "call_limit"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			body := `{"model":"test-model","input":"find latest","max_tool_calls":1}`
			c, recorder := hostedSearchTestContext(t, body, 303)
			modelRounds, searches := 0, 0
			result, err := forwardHostedResponsesWebSearch(context.Background(), c, &Account{}, []byte(body), func(next []byte) (*OpenAIForwardResult, error) {
				modelRounds++
				return hostedSearchTestResponse(c, "resp_failed", []json.RawMessage{hostedSearchTestCall(next, "search_limit")}, 4, 2), nil
			}, mergeOpenAIWebSearchResult, func(context.Context, *Account, string, *responsesWebSearchPlan) (*websearch.SearchResponse, error) {
				searches++
				if scenario.providerError {
					return nil, errors.New("search provider unavailable")
				}
				return &websearch.SearchResponse{}, nil
			})
			require.Error(t, err)
			if scenario.providerError {
				require.NotNil(t, result)
				require.True(t, IsHostedResponsesWebSearchProviderError(err))
			}
			require.Equal(t, "failed", gjson.Get(recorder.Body.String(), "status").String())
			items := gjson.Get(recorder.Body.String(), "output").Array()
			require.Equal(t, "failed", items[len(items)-1].Get("status").String())
			require.Equal(t, 1, searches)
			if scenario.providerError {
				require.Equal(t, 1, modelRounds)
			} else {
				require.Equal(t, 2, modelRounds)
			}
			require.Equal(t, int64(modelRounds*6), gjson.Get(recorder.Body.String(), "usage.total_tokens").Int())
			require.NotContains(t, recorder.Body.String(), responsesWebSearchFunctionPrefix)
			require.False(t, gjson.Get(recorder.Body.String(), `output.#(type=="message")`).Exists())
		})
	}
}

func TestHostedResponsesSearchIncompleteCallIsNotExecuted(t *testing.T) {
	body := `{"model":"test-model","input":"find latest"}`
	c, recorder := hostedSearchTestContext(t, body, 304)
	_, err := forwardHostedResponsesWebSearch(context.Background(), c, &Account{}, []byte(body), func(next []byte) (*OpenAIForwardResult, error) {
		call := hostedSearchTestCall(next, "partial")
		c.Data(200, "application/json", rawResponsesSearchJSON(map[string]any{"id": "resp_partial", "object": "response", "model": "test-model", "created_at": 1700000000, "status": "incomplete", "incomplete_details": map[string]string{"reason": "max_output_tokens"}, "output": []json.RawMessage{call}}))
		return &OpenAIForwardResult{}, nil
	}, mergeOpenAIWebSearchResult, func(context.Context, *Account, string, *responsesWebSearchPlan) (*websearch.SearchResponse, error) {
		t.Fatal("incomplete calls must not execute search")
		return nil, nil
	})
	require.NoError(t, err)
	require.Equal(t, "incomplete", gjson.Get(recorder.Body.String(), "status").String())
	require.Equal(t, "web_search_call", gjson.Get(recorder.Body.String(), "output.0.type").String())
	require.NotContains(t, recorder.Body.String(), responsesWebSearchFunctionPrefix)
}

func TestHostedResponsesSearchGateDisabledAndExplicitNoneRemainTransparent(t *testing.T) {
	previousManager, previousConfig := getWebSearchManager(), webSearchEmulationCache.Load()
	t.Cleanup(func() {
		SetWebSearchManager(previousManager)
		if previousConfig != nil {
			webSearchEmulationCache.Store(previousConfig)
		} else {
			webSearchEmulationCache.Store((*cachedWebSearchEmulationConfig)(nil))
		}
	})
	SetWebSearchManager(websearch.NewManager([]websearch.ProviderConfig{{Type: "brave", APIKey: "fixture"}}, nil))
	webSearchEmulationCache.Store(&cachedWebSearchEmulationConfig{config: &WebSearchEmulationConfig{Enabled: false, Providers: []WebSearchProviderConfig{{Type: "brave", APIKey: "fixture"}}}, expiresAt: time.Now().Add(time.Minute).UnixNano()})
	body := []byte(`{ "model":"test-model", "tools":[{"type":"web_search"}], "input":"hello" }`)
	c, _ := hostedSearchTestContext(t, string(body), 305)
	svc := &GatewayService{settingService: &SettingService{}}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{featureKeyWebSearchEmulation: WebSearchModeEnabled}}
	_, err := svc.ForwardResponsesWithWebSearch(context.Background(), c, account, body, func(next []byte) (*ForwardResult, error) {
		require.True(t, bytes.Equal(body, next))
		return &ForwardResult{}, nil
	})
	require.NoError(t, err)
	webSearchEmulationCache.Store(&cachedWebSearchEmulationConfig{config: &WebSearchEmulationConfig{Enabled: true, Providers: []WebSearchProviderConfig{{Type: "brave", APIKey: "fixture"}}}, expiresAt: time.Now().Add(time.Minute).UnixNano()})
	account.Extra[featureKeyWebSearchEmulation] = WebSearchModeDisabled
	require.False(t, responsesWebSearchEnabled(context.Background(), c, account, svc.settingService, nil))
	account.Extra[featureKeyWebSearchEmulation] = WebSearchModeEnabled
	body = []byte(`{ "model":"test-model", "tool_choice":"none", "input":"no search" }`)
	_, err = svc.ForwardResponsesWithWebSearch(context.Background(), c, account, body, func(next []byte) (*ForwardResult, error) {
		require.True(t, bytes.Equal(body, next))
		return &ForwardResult{}, nil
	})
	require.NoError(t, err)
	c.Request.URL.Path = "/v1/responses/compact"
	require.False(t, responsesWebSearchEnabled(context.Background(), c, account, svc.settingService, nil))
}

func TestHostedResponsesSearchParsesTerminalSSEAndRejectsTruncation(t *testing.T) {
	wire := "event: response.completed\r\ndata: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"id\":\"resp_sse\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\r\n\r\n"
	response, err := parseResponsesWebSearchRound([]byte(wire))
	require.NoError(t, err)
	require.Equal(t, "resp_sse", responsesSearchString(response["id"]))
	_, err = parseResponsesWebSearchRound([]byte(`data: {"type":"response.created","response":{"id":"resp_partial"}}`))
	require.ErrorContains(t, err, "no complete Responses object")
}

func TestHostedResponsesSearchKeepsAllowedToolsDuringContinuation(t *testing.T) {
	body := `{"model":"test-model","input":"find latest","tools":[{"type":"function","name":"delete_file","parameters":{"type":"object"}},{"type":"web_search"}],"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"web_search"}]}}`
	c, _ := hostedSearchTestContext(t, body, 306)
	round := 0
	_, err := forwardHostedResponsesWebSearch(context.Background(), c, &Account{}, []byte(body), func(next []byte) (*OpenAIForwardResult, error) {
		round++
		if round == 1 {
			return hostedSearchTestResponse(c, "resp_policy_search", []json.RawMessage{hostedSearchTestCall(next, "policy_search")}, 2, 1), nil
		}
		require.Equal(t, "allowed_tools", gjson.GetBytes(next, "tool_choice.type").String())
		require.Equal(t, "auto", gjson.GetBytes(next, "tool_choice.mode").String())
		require.NotContains(t, gjson.GetBytes(next, "tool_choice.tools").Raw, "delete_file")
		return hostedSearchTestResponse(c, "resp_policy_done", []json.RawMessage{}, 3, 1), nil
	}, mergeOpenAIWebSearchResult, func(context.Context, *Account, string, *responsesWebSearchPlan) (*websearch.SearchResponse, error) {
		return &websearch.SearchResponse{}, nil
	})
	require.NoError(t, err)
}

func TestHostedResponsesSearchRejectsModelCallOutsideForcedClientChoice(t *testing.T) {
	body := `{"model":"test-model","input":"read local file","tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"read_file"}}`
	c, recorder := hostedSearchTestContext(t, body, 307)
	_, err := forwardHostedResponsesWebSearch(context.Background(), c, &Account{}, []byte(body), func(next []byte) (*OpenAIForwardResult, error) {
		return hostedSearchTestResponse(c, "resp_bad_choice", []json.RawMessage{hostedSearchTestCall(next, "forbidden_search")}, 2, 1), nil
	}, mergeOpenAIWebSearchResult, func(context.Context, *Account, string, *responsesWebSearchPlan) (*websearch.SearchResponse, error) {
		t.Fatal("a model cannot override the caller's forced client tool")
		return nil, nil
	})
	require.ErrorContains(t, err, "permitted tool_choice")
	require.Equal(t, "failed", gjson.Get(recorder.Body.String(), "status").String())
}

func TestHostedResponsesSearchReturnsClientToolDiscoveryWithoutAnotherModelRound(t *testing.T) {
	body := `{"model":"test-model","input":"search and discover tools","tools":[{"type":"tool_search","execution":"client"}]}`
	c, recorder := hostedSearchTestContext(t, body, 308)
	rounds := 0
	discovery := json.RawMessage(`{"type":"tool_search_call","id":"tsc_1","call_id":"discover_1","execution":"client","arguments":{"query":"files"}}`)
	_, err := forwardHostedResponsesWebSearch(context.Background(), c, &Account{}, []byte(body), func(next []byte) (*OpenAIForwardResult, error) {
		rounds++
		return hostedSearchTestResponse(c, "resp_discovery", []json.RawMessage{hostedSearchTestCall(next, "search_discovery"), discovery}, 3, 1), nil
	}, mergeOpenAIWebSearchResult, func(context.Context, *Account, string, *responsesWebSearchPlan) (*websearch.SearchResponse, error) {
		return &websearch.SearchResponse{}, nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, rounds)
	require.Equal(t, "tool_search_call", gjson.Get(recorder.Body.String(), "output.1.type").String())
	require.Equal(t, "client", gjson.Get(recorder.Body.String(), "output.1.execution").String())
	require.Equal(t, "discover_1", gjson.Get(recorder.Body.String(), "output.1.call_id").String())
}

func TestHostedResponsesSearchOverridesDoNotEnableLegacyMessagesShortcut(t *testing.T) {
	previousManager, previousConfig := getWebSearchManager(), webSearchEmulationCache.Load()
	t.Cleanup(func() {
		SetWebSearchManager(previousManager)
		if previousConfig != nil {
			webSearchEmulationCache.Store(previousConfig)
		} else {
			webSearchEmulationCache.Store((*cachedWebSearchEmulationConfig)(nil))
		}
	})
	SetWebSearchManager(websearch.NewManager([]websearch.ProviderConfig{{Type: "brave", APIKey: "fixture"}}, nil))
	webSearchEmulationCache.Store(&cachedWebSearchEmulationConfig{config: &WebSearchEmulationConfig{Enabled: true, Providers: []WebSearchProviderConfig{{Type: "brave", APIKey: "fixture"}}}, expiresAt: time.Now().Add(time.Minute).UnixNano()})
	svc := &GatewayService{settingService: newSettingServiceForWebSearchTest(true)}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{featureKeyWebSearchEmulation: WebSearchModeEnabled}}
	require.False(t, svc.shouldEmulateWebSearch(context.Background(), account, nil, []byte(`{"tools":[{"type":"web_search"}]}`)))
}

func TestHostedResponsesSearchBillsEachRoundWithoutFalseLongContextTier(t *testing.T) {
	billing := newTestBillingServiceForResolver()
	pricing := billing.fallbackPrices["claude-sonnet-4"]
	pricing.LongContextInputThreshold = 100
	pricing.LongContextInputMultiplier = 2
	pricing.LongContextOutputMultiplier = 2
	key := &APIKey{}
	at := time.Unix(1700000000, 0)
	enabled := true
	openAI := &OpenAIGatewayService{billingService: billing}
	first := &OpenAIForwardResult{Model: "claude-sonnet-4", Usage: OpenAIUsage{InputTokens: 75, OutputTokens: 10}}
	second := &OpenAIForwardResult{Model: "claude-sonnet-4", Usage: OpenAIUsage{InputTokens: 75, OutputTokens: 10}}
	combined := mergeOpenAIWebSearchResult(mergeOpenAIWebSearchResult(nil, first), second)
	cost, err := openAI.calculateOpenAIRecordUsageCost(context.Background(), combined, key, []string{"claude-sonnet-4"}, 1, 1, 1, 1, openAIResponsesWebSearchTokens(combined.Usage), "", &enabled, at)
	require.NoError(t, err)
	require.False(t, cost.LongContextBillingApplied)
	require.InDelta(t, float64(150)*3e-6+float64(20)*15e-6, cost.TotalCost, 1e-12)
	naive, err := billing.CalculateCost("claude-sonnet-4", UsageTokens{InputTokens: 150, OutputTokens: 20}, 1)
	require.NoError(t, err)
	require.True(t, naive.LongContextBillingApplied)
	require.Greater(t, naive.TotalCost, cost.TotalCost)

	gateway := &GatewayService{billingService: billing}
	claude := mergeClaudeWebSearchResult(mergeClaudeWebSearchResult(nil, &ForwardResult{Model: "claude-sonnet-4", Usage: ClaudeUsage{InputTokens: 75, OutputTokens: 10}}), &ForwardResult{Model: "claude-sonnet-4", Usage: ClaudeUsage{InputTokens: 75, OutputTokens: 10}})
	claudeCost := gateway.calculateRecordUsageCost(context.Background(), claude, key, "claude-sonnet-4", 1, 1, at)
	require.False(t, claudeCost.LongContextBillingApplied)
	require.InDelta(t, cost.TotalCost, claudeCost.TotalCost, 1e-12)

	channels := newChannelServiceWithCache(11, &Channel{ID: 33, Status: StatusActive, ApplyPricingToAccountStats: true})
	stats := responsesWebSearchAccountStatsCost(context.Background(), channels, billing, 1, 11, combined.hostedSearchRounds, combined.hostedSearchRoundCosts, cost.TotalCost, at, true)
	require.NotNil(t, stats)
	require.InDelta(t, cost.TotalCost, *stats, 1e-12)
}

func TestHostedResponsesSearchPerRequestPricingStillChargesOnePublicRequest(t *testing.T) {
	resolver := newResolverWithChannel(t, []ChannelModelPricing{{Platform: "anthropic", Models: []string{"claude-sonnet-4"}, BillingMode: BillingModePerRequest, PerRequestPrice: testPtrFloat64(0.05), Intervals: []PricingInterval{{MinTokens: 0, MaxTokens: testPtrInt(100), PerRequestPrice: testPtrFloat64(0.05)}, {MinTokens: 100, PerRequestPrice: testPtrFloat64(0.10)}}}})
	gateway := &GatewayService{billingService: resolver.billingService, resolver: resolver, channelService: resolver.channelService}
	key := &APIKey{GroupID: groupIDPtr(), Group: &Group{ID: 100, Platform: PlatformAnthropic}}
	combined := mergeClaudeWebSearchResult(mergeClaudeWebSearchResult(nil, &ForwardResult{Model: "claude-sonnet-4", Usage: ClaudeUsage{InputTokens: 75}}), &ForwardResult{Model: "claude-sonnet-4", Usage: ClaudeUsage{InputTokens: 75}})
	cost := gateway.calculateRecordUsageCost(context.Background(), combined, key, "claude-sonnet-4", 1, 1, time.Unix(1700000000, 0))
	require.Equal(t, string(BillingModePerRequest), cost.BillingMode)
	require.InDelta(t, 0.05, cost.TotalCost, 1e-12)
}
