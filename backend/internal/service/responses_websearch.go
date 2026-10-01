package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/MACOS-DO/sub4api/internal/pkg/websearch"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	responsesWebSearchIDPrefix    = "ws_sub4api_"
	responsesWebSearchRoundLimit  = 64 << 20
	responsesWebSearchReplayLimit = 4096
	responsesWebSearchReplayBytes = 64 << 20
	responsesWebSearchReplayTTL   = time.Hour
)

// hostedResponsesWebSearchProviderError marks a failure in the external search
// provider after an upstream model round has already been paid for. It is
// intentionally distinct from an upstream account failure: retrying the
// request through another account would replay that paid round.
type hostedResponsesWebSearchProviderError struct {
	err error
}

func (e *hostedResponsesWebSearchProviderError) Error() string { return e.err.Error() }
func (e *hostedResponsesWebSearchProviderError) Unwrap() error { return e.err }

// IsHostedResponsesWebSearchProviderError reports whether a hosted-search
// failure came from the search provider rather than the model account.
func IsHostedResponsesWebSearchProviderError(err error) bool {
	var target *hostedResponsesWebSearchProviderError
	return errors.As(err, &target)
}

// Search replay retains only gateway-issued search calls and their real results,
// never arbitrary conversation history. Callers are isolated by authenticated
// user, API key and group, including when upstream accounts change.
type responsesWebSearchReplayEntry struct {
	call    json.RawMessage
	result  json.RawMessage
	pending []json.RawMessage
	expires time.Time
	bytes   int
}

var responsesWebSearchReplay = struct {
	sync.Mutex
	entries map[string]responsesWebSearchReplayEntry
	bytes   int
}{entries: make(map[string]responsesWebSearchReplayEntry)}

func responsesWebSearchScope(c *gin.Context) string {
	key := getAPIKeyFromContext(c)
	if key == nil || key.ID <= 0 || key.UserID <= 0 {
		return ""
	}
	groupID := int64(0)
	if key.GroupID != nil {
		groupID = *key.GroupID
	}
	return fmt.Sprintf("%d:%d:%d:", key.UserID, key.ID, groupID)
}

func putResponsesWebSearchReplay(scope, id string, entry responsesWebSearchReplayEntry) {
	if scope == "" || id == "" {
		return
	}
	entry.expires = time.Now().Add(responsesWebSearchReplayTTL)
	entry.bytes = len(entry.call) + len(entry.result)
	for _, result := range entry.pending {
		entry.bytes += len(result)
	}
	if entry.bytes > responsesWebSearchReplayBytes {
		return
	}
	responsesWebSearchReplay.Lock()
	defer responsesWebSearchReplay.Unlock()
	cacheKey := scope + id
	if old, ok := responsesWebSearchReplay.entries[cacheKey]; ok {
		responsesWebSearchReplay.bytes -= old.bytes
		delete(responsesWebSearchReplay.entries, cacheKey)
	}
	for len(responsesWebSearchReplay.entries) >= responsesWebSearchReplayLimit || responsesWebSearchReplay.bytes+entry.bytes > responsesWebSearchReplayBytes {
		oldestKey := ""
		var oldest time.Time
		for key, candidate := range responsesWebSearchReplay.entries {
			if oldestKey == "" || candidate.expires.Before(oldest) {
				oldestKey, oldest = key, candidate.expires
			}
		}
		if oldestKey == "" {
			break
		}
		responsesWebSearchReplay.bytes -= responsesWebSearchReplay.entries[oldestKey].bytes
		delete(responsesWebSearchReplay.entries, oldestKey)
	}
	responsesWebSearchReplay.entries[cacheKey] = entry
	responsesWebSearchReplay.bytes += entry.bytes
}

func getResponsesWebSearchReplay(scope, id string) (responsesWebSearchReplayEntry, bool) {
	if scope == "" {
		return responsesWebSearchReplayEntry{}, false
	}
	responsesWebSearchReplay.Lock()
	defer responsesWebSearchReplay.Unlock()
	key := scope + id
	entry, ok := responsesWebSearchReplay.entries[key]
	if ok && !time.Now().Before(entry.expires) {
		delete(responsesWebSearchReplay.entries, key)
		responsesWebSearchReplay.bytes -= entry.bytes
		return responsesWebSearchReplayEntry{}, false
	}
	return entry, ok
}

func responsesWebSearchEnabled(ctx context.Context, c *gin.Context, account *Account, settings *SettingService, channels *ChannelService) bool {
	if c == nil || c.Request == nil || c.Request.Method != http.MethodPost || !strings.HasSuffix(strings.TrimRight(c.Request.URL.Path, "/"), "/responses") {
		return false
	}
	if account == nil || settings == nil || getWebSearchManager() == nil || !settings.IsWebSearchEmulationEnabled(ctx) {
		return false
	}
	switch account.GetWebSearchEmulationMode() {
	case WebSearchModeEnabled:
		return true
	case WebSearchModeDisabled:
		return false
	}
	key := getAPIKeyFromContext(c)
	if key == nil || key.GroupID == nil || channels == nil {
		return false
	}
	channel, err := channels.GetChannelForGroup(ctx, *key.GroupID)
	return err == nil && channel != nil && channel.IsWebSearchEmulationEnabled(account.Platform)
}

// ForwardResponsesWithWebSearch shares the same account/channel gate with the
// Anthropic and Antigravity Responses adapters. The callback remains the only
// upstream transport, preserving authentication, routing and usage observation.
func (s *GatewayService) ForwardResponsesWithWebSearch(ctx context.Context, c *gin.Context, account *Account, body []byte, forward func([]byte) (*ForwardResult, error)) (*ForwardResult, error) {
	if !responsesWebSearchEnabled(ctx, c, account, s.settingService, s.channelService) {
		return forward(body)
	}
	return forwardHostedResponsesWebSearch(ctx, c, account, body, forward, mergeClaudeWebSearchResult, executeResponsesWebSearch)
}

func mergeClaudeWebSearchResult(total, next *ForwardResult) *ForwardResult {
	if next == nil {
		return total
	}
	if total == nil {
		copy := *next
		copy.hostedSearchRounds = []*ForwardResult{next}
		return &copy
	}
	previous := total.Usage
	rounds, imageCount, searchCount := total.hostedSearchRounds, total.ImageCount, total.SearchCount
	*total = *next
	total.hostedSearchRounds = append(rounds, next)
	total.ImageCount += imageCount
	total.SearchCount += searchCount
	total.Usage.InputTokens += previous.InputTokens
	total.Usage.OutputTokens += previous.OutputTokens
	total.Usage.CacheCreationInputTokens += previous.CacheCreationInputTokens
	total.Usage.CacheReadInputTokens += previous.CacheReadInputTokens
	total.Usage.CacheCreation5mTokens += previous.CacheCreation5mTokens
	total.Usage.CacheCreation1hTokens += previous.CacheCreation1hTokens
	total.Usage.ImageOutputTokens += previous.ImageOutputTokens
	return total
}

func mergeOpenAIWebSearchResult(total, next *OpenAIForwardResult) *OpenAIForwardResult {
	if next == nil {
		return total
	}
	if total == nil {
		copy := *next
		copy.hostedSearchRounds = []*OpenAIForwardResult{next}
		return &copy
	}
	previous := total.Usage
	rounds, imageCount, videoCount, searchCount, webSearchCalls := total.hostedSearchRounds, total.ImageCount, total.VideoCount, total.SearchCount, total.WebSearchCalls
	*total = *next
	total.hostedSearchRounds = append(rounds, next)
	total.ImageCount += imageCount
	total.VideoCount += videoCount
	total.SearchCount += searchCount
	total.WebSearchCalls += webSearchCalls
	total.Usage.InputTokens += previous.InputTokens
	total.Usage.OutputTokens += previous.OutputTokens
	total.Usage.CacheCreationInputTokens += previous.CacheCreationInputTokens
	total.Usage.CacheReadInputTokens += previous.CacheReadInputTokens
	total.Usage.ImageInputTokens += previous.ImageInputTokens
	total.Usage.ImageCacheReadTokens += previous.ImageCacheReadTokens
	total.Usage.ImageOutputTokens += previous.ImageOutputTokens
	return total
}

func forwardHostedResponsesWebSearch[T any](ctx context.Context, c *gin.Context, account *Account, body []byte, forward func([]byte) (*T, error), merge func(*T, *T) *T, search func(context.Context, *Account, string, *responsesWebSearchPlan) (*websearch.SearchResponse, error)) (*T, error) {
	plan, err := prepareResponsesWebSearch(body)
	if err != nil {
		writeResponsesWebSearchRequestError(c, err)
		return nil, err
	}
	if plan == nil {
		return forward(body)
	}
	start := time.Now()
	var request map[string]json.RawMessage
	if err := json.Unmarshal(plan.Body, &request); err != nil {
		writeResponsesWebSearchRequestError(c, err)
		return nil, err
	}
	scope := responsesWebSearchScope(c)
	input, err := responsesWebSearchInput(request["input"], scope)
	if err != nil {
		writeResponsesWebSearchRequestError(c, err)
		return nil, err
	}
	previousID := gjson.GetBytes(plan.Body, "previous_response_id").String()
	if pending, ok := getResponsesWebSearchReplay(scope, "pending:"+previousID); ok {
		for _, result := range pending.pending {
			callID := gjson.GetBytes(result, "call_id").String()
			present := false
			for _, item := range input {
				if gjson.GetBytes(item, "type").String() == "function_call_output" && gjson.GetBytes(item, "call_id").String() == callID {
					present = true
					break
				}
			}
			if !present {
				input = append(input, result)
			}
		}
	}

	originalWriter := c.Writer
	var total *T
	var response map[string]json.RawMessage
	output := make([]json.RawMessage, 0)
	var usage json.RawMessage
	var sources []websearch.SearchResult
	var responseHeaders http.Header
	var firstTokenMs *int
	searchCalls := 0
	remainingTokens := int64(-1)
	var originalMaxOutputTokens json.RawMessage
	if raw, ok := request["max_output_tokens"]; ok {
		originalMaxOutputTokens = append(json.RawMessage(nil), raw...)
	}
	if value := gjson.GetBytes(body, "max_output_tokens"); value.Exists() && value.Type == gjson.Number {
		remainingTokens = value.Int()
	}

	finish := func(cause error) (*T, error) {
		if response == nil {
			response = map[string]json.RawMessage{
				"id":         rawResponsesSearchJSON("resp_ws_" + uuid.NewString()),
				"object":     rawResponsesSearchJSON("response"),
				"created_at": rawResponsesSearchJSON(time.Now().Unix()),
				"model":      request["model"],
			}
		}
		if cause != nil {
			response["status"] = rawResponsesSearchJSON("failed")
			response["error"] = rawResponsesSearchJSON(map[string]string{"code": "web_search_error", "message": strings.ReplaceAll(cause.Error(), plan.FunctionName, "web_search")})
			delete(response, "incomplete_details")
		}
		normalizeResponsesWebSearchItems(output)
		addResponsesWebSearchCitations(output, sources)
		response["output"] = rawResponsesSearchJSON(output)
		if len(usage) != 0 {
			response["usage"] = usage
		}
		if len(originalMaxOutputTokens) != 0 {
			response["max_output_tokens"] = originalMaxOutputTokens
		}
		// Public declarations never reveal the internal function. Server provision
		// is represented as a hosted tool even when the client omitted tools.
		var publicTools []json.RawMessage
		if len(plan.OriginalTools) != 0 {
			_ = json.Unmarshal(plan.OriginalTools, &publicTools)
		}
		hasSearch := false
		for _, tool := range publicTools {
			if kind := gjson.GetBytes(tool, "type").String(); kind == "web_search" || kind == "web_search_preview" {
				hasSearch = true
			}
		}
		if !hasSearch {
			publicTools = append(publicTools, json.RawMessage(`{"type":"web_search"}`))
		}
		response["tools"] = rawResponsesSearchJSON(publicTools)
		if len(plan.OriginalToolChoice) != 0 {
			response["tool_choice"] = plan.OriginalToolChoice
		} else {
			response["tool_choice"] = json.RawMessage(`"auto"`)
		}
		response["parallel_tool_calls"] = request["parallel_tool_calls"]
		if len(response["parallel_tool_calls"]) == 0 {
			delete(response, "parallel_tool_calls")
		}
		encoded, encodeErr := json.Marshal(response)
		if encodeErr != nil {
			return total, encodeErr
		}
		copyResponsesWebSearchHeaders(originalWriter.Header(), responseHeaders)
		finishResponsesWebSearchResult(total, plan.OriginalStream, time.Since(start), firstTokenMs)
		if writeErr := writeResponsesWebSearchResponse(c, encoded, plan.OriginalStream); writeErr != nil {
			return total, writeErr
		}
		return total, cause
	}

	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		request["input"] = rawResponsesSearchJSON(input)
		if remainingTokens >= 0 {
			if remainingTokens == 0 {
				if response != nil {
					response["status"] = json.RawMessage(`"incomplete"`)
					response["incomplete_details"] = json.RawMessage(`{"reason":"max_output_tokens"}`)
				}
				return finish(nil)
			}
			request["max_output_tokens"] = rawResponsesSearchJSON(remainingTokens)
		}
		upstreamBody, err := json.Marshal(request)
		if err != nil {
			return finish(err)
		}
		roundStart := time.Now()
		capture := &responsesWebSearchRoundWriter{header: make(http.Header), status: http.StatusOK, size: -1}
		var next *T
		func() {
			c.Writer = capture
			defer func() { c.Writer = originalWriter }()
			next, err = forward(upstreamBody)
		}()
		if responseHeaders == nil {
			responseHeaders = capture.header.Clone()
		} else {
			for key, values := range capture.header {
				responseHeaders[key] = append([]string(nil), values...)
			}
		}
		if firstTokenMs == nil && next != nil {
			var roundFirstToken *int
			switch value := any(next).(type) {
			case *OpenAIForwardResult:
				roundFirstToken = value.FirstTokenMs
			case *ForwardResult:
				roundFirstToken = value.FirstTokenMs
			}
			if roundFirstToken != nil {
				value := int(roundStart.Sub(start).Milliseconds()) + *roundFirstToken
				firstTokenMs = &value
			}
		}
		total = merge(total, next)
		if capture.err != nil {
			return finish(capture.err)
		}
		if err != nil || capture.status >= http.StatusBadRequest {
			if err == nil {
				err = fmt.Errorf("upstream returned HTTP %d", capture.status)
			}
			if response == nil && total == nil {
				if capture.body.Len() > 0 {
					copyResponsesWebSearchHeaders(originalWriter.Header(), capture.header)
					originalWriter.WriteHeader(capture.status)
					_, _ = originalWriter.Write(bytes.ReplaceAll(capture.body.Bytes(), []byte(plan.FunctionName), []byte("web_search")))
				}
				return nil, err
			}
			// A paid model round/search has already occurred. Emit a terminal failed
			// response instead of replaying the whole request via account failover.
			return finish(errors.New(err.Error()))
		}
		response, err = parseResponsesWebSearchRound(capture.body.Bytes())
		if err != nil {
			return finish(err)
		}
		usage = mergeResponsesWebSearchUsage(usage, response["usage"])
		if remainingTokens >= 0 {
			remainingTokens -= gjson.GetBytes(response["usage"], "output_tokens").Int()
			if remainingTokens < 0 {
				remainingTokens = 0
			}
		}
		var roundOutput []json.RawMessage
		if err := json.Unmarshal(response["output"], &roundOutput); err != nil {
			return finish(fmt.Errorf("invalid upstream Responses output: %w", err))
		}
		var toolResults []json.RawMessage
		hasClientCall := false
		status := gjson.GetBytes(response["status"], "@this").String()
		for _, item := range roundOutput {
			kind := gjson.GetBytes(item, "type").String()
			if kind != "function_call" || gjson.GetBytes(item, "name").String() != plan.FunctionName {
				output = append(output, item)
				if isResponsesWebSearchClientCall(item) {
					hasClientCall = true
				}
				continue
			}
			query := gjson.Get(gjson.GetBytes(item, "arguments").String(), "query").String()
			searchID := responsesWebSearchIDPrefix + uuid.NewString()
			publicCall := map[string]any{"type": "web_search_call", "id": searchID, "status": "failed", "action": map[string]any{"type": "search", "query": query}}
			if status != "completed" || gjson.GetBytes(item, "status").String() == "incomplete" {
				output = append(output, rawResponsesSearchJSON(publicCall))
				continue
			}
			if !responsesSearchAllowsCall(request["tool_choice"], plan.FunctionName) {
				output = append(output, rawResponsesSearchJSON(publicCall))
				return finish(errors.New("model selected web_search outside the permitted tool_choice"))
			}
			if strings.TrimSpace(query) == "" || !gjson.Valid(gjson.GetBytes(item, "arguments").String()) || gjson.Get(gjson.GetBytes(item, "arguments").String(), "query").Type != gjson.String || gjson.GetBytes(item, "call_id").String() == "" {
				output = append(output, rawResponsesSearchJSON(publicCall))
				return finish(errors.New("model returned an invalid web search call"))
			}
			if searchCalls >= plan.MaxCalls {
				output = append(output, rawResponsesSearchJSON(publicCall))
				return finish(fmt.Errorf("web search exceeded max_tool_calls=%d", plan.MaxCalls))
			}
			searchCalls++
			results, searchErr := search(ctx, account, query, plan)
			if searchErr != nil {
				output = append(output, rawResponsesSearchJSON(publicCall))
				return finish(&hostedResponsesWebSearchProviderError{err: searchErr})
			}
			publicCall["status"] = "completed"
			callSources := make([]map[string]string, 0, len(results.Results))
			for _, result := range results.Results {
				callSources = append(callSources, map[string]string{"type": "url", "url": result.URL})
			}
			publicCall["action"] = map[string]any{"type": "search", "query": query, "queries": []string{query}, "sources": callSources}
			output = append(output, rawResponsesSearchJSON(publicCall))
			sources = append(sources, results.Results...)
			toolResult := rawResponsesSearchJSON(map[string]any{"type": "function_call_output", "call_id": gjson.GetBytes(item, "call_id").String(), "output": string(rawResponsesSearchJSON(map[string]any{"query": query, "results": results.Results}))})
			toolResults = append(toolResults, toolResult)
			putResponsesWebSearchReplay(scope, searchID, responsesWebSearchReplayEntry{call: item, result: toolResult})
		}
		if len(toolResults) > 0 {
			responseID := gjson.GetBytes(response["id"], "@this").String()
			putResponsesWebSearchReplay(scope, "pending:"+responseID, responsesWebSearchReplayEntry{pending: toolResults})
		}
		if status != "completed" || len(toolResults) == 0 {
			return finish(nil)
		}
		if hasClientCall {
			// These calls remain the client's responsibility. Retain the already
			// executed server results for previous_response_id continuation.
			return finish(nil)
		}
		input = append(input, roundOutput...)
		input = append(input, toolResults...)
		request["tool_choice"] = responsesSearchContinuationChoice(request["tool_choice"])
	}
}

func writeResponsesWebSearchRequestError(c *gin.Context, err error) {
	c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": err.Error()}})
}

func rawResponsesSearchJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func responsesWebSearchInput(raw json.RawMessage, scope string) ([]json.RawMessage, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return []json.RawMessage{}, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []json.RawMessage{rawResponsesSearchJSON(map[string]any{"type": "message", "role": "user", "content": text})}, nil
	}
	var input []json.RawMessage
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, fmt.Errorf("invalid Responses input: %w", err)
	}
	var normalized []json.RawMessage
	for _, item := range input {
		id := gjson.GetBytes(item, "id").String()
		if gjson.GetBytes(item, "type").String() == "web_search_call" && strings.HasPrefix(id, responsesWebSearchIDPrefix) {
			entry, ok := getResponsesWebSearchReplay(scope, id)
			if !ok {
				return nil, errors.New("gateway web search history is expired or belongs to another API key; resend the conversation as messages")
			}
			normalized = append(normalized, entry.call, entry.result)
		} else {
			normalized = append(normalized, item)
		}
	}
	return normalized, nil
}

func executeResponsesWebSearch(ctx context.Context, account *Account, query string, plan *responsesWebSearchPlan) (*websearch.SearchResponse, error) {
	searchQuery := query
	if len(plan.AllowedDomains) > 0 {
		terms := make([]string, 0, len(plan.AllowedDomains))
		for _, domain := range plan.AllowedDomains {
			terms = append(terms, "site:"+domain)
		}
		searchQuery += " (" + strings.Join(terms, " OR ") + ")"
	}
	manager := getWebSearchManager()
	if manager == nil {
		return nil, errors.New("web search provider is no longer configured")
	}
	response, _, err := manager.SearchWithBestProvider(ctx, websearch.SearchRequest{Query: searchQuery, MaxResults: plan.MaxResults, ProxyURL: resolveAccountProxyURL(account)})
	if err != nil {
		return nil, err
	}
	if len(plan.AllowedDomains) > 0 {
		filtered := response.Results[:0]
		for _, result := range response.Results {
			parsed, err := url.Parse(result.URL)
			if err != nil {
				continue
			}
			host := strings.ToLower(parsed.Hostname())
			for _, domain := range plan.AllowedDomains {
				if host == domain || strings.HasSuffix(host, "."+domain) {
					filtered = append(filtered, result)
					break
				}
			}
		}
		response.Results = filtered
	}
	return response, nil
}

func mergeResponsesWebSearchUsage(previous, next json.RawMessage) json.RawMessage {
	if len(previous) == 0 {
		return next
	}
	if len(next) == 0 || bytes.Equal(bytes.TrimSpace(next), []byte("null")) {
		return previous
	}
	var left, right map[string]json.RawMessage
	if json.Unmarshal(previous, &left) != nil || json.Unmarshal(next, &right) != nil {
		return next
	}
	for key, old := range left {
		current, exists := right[key]
		if !exists {
			right[key] = old
			continue
		}
		if len(old) > 0 && len(current) > 0 && old[0] == '{' && current[0] == '{' {
			right[key] = mergeResponsesWebSearchUsage(old, current)
			continue
		}
		oldCount, oldErr := strconv.ParseInt(string(old), 10, 64)
		newCount, newErr := strconv.ParseInt(string(current), 10, 64)
		if oldErr == nil && newErr == nil && oldCount >= 0 && newCount >= 0 && oldCount <= (1<<63-1)-newCount {
			right[key] = rawResponsesSearchJSON(oldCount + newCount)
		}
	}
	return rawResponsesSearchJSON(right)
}

func copyResponsesWebSearchHeaders(dst, src http.Header) {
	if dst == nil || src == nil {
		return
	}
	for key, values := range src {
		switch strings.ToLower(key) {
		case "content-length", "content-encoding", "transfer-encoding", "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "upgrade":
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func finishResponsesWebSearchResult[T any](result *T, stream bool, duration time.Duration, firstTokenMs *int) {
	if result == nil {
		return
	}
	switch value := any(result).(type) {
	case *OpenAIForwardResult:
		value.Stream, value.Duration, value.FirstTokenMs = stream, duration, firstTokenMs
	case *ForwardResult:
		value.Stream, value.Duration, value.FirstTokenMs = stream, duration, firstTokenMs
	}
}

func addResponsesWebSearchCitations(output []json.RawMessage, sources []websearch.SearchResult) {
	if len(sources) == 0 {
		return
	}
	for index, item := range output {
		if gjson.GetBytes(item, "type").String() != "message" {
			continue
		}
		var message map[string]json.RawMessage
		if json.Unmarshal(item, &message) != nil {
			continue
		}
		var parts []map[string]json.RawMessage
		if json.Unmarshal(message["content"], &parts) != nil {
			continue
		}
		changed := false
		for _, part := range parts {
			if gjson.GetBytes(part["type"], "@this").String() != "output_text" {
				continue
			}
			text := gjson.GetBytes(part["text"], "@this").String()
			var annotations []json.RawMessage
			_ = json.Unmarshal(part["annotations"], &annotations)
			for _, source := range sources {
				start := strings.Index(text, source.URL)
				if start < 0 || source.URL == "" {
					continue
				}
				found := false
				for _, annotation := range annotations {
					if gjson.GetBytes(annotation, "url").String() == source.URL {
						found = true
						break
					}
				}
				if !found {
					annotations = append(annotations, rawResponsesSearchJSON(map[string]any{"type": "url_citation", "url": source.URL, "title": source.Title, "start_index": utf8.RuneCountInString(text[:start]), "end_index": utf8.RuneCountInString(text[:start+len(source.URL)])}))
					changed = true
				}
			}
			if changed {
				part["annotations"] = rawResponsesSearchJSON(annotations)
			}
		}
		if changed {
			message["content"] = rawResponsesSearchJSON(parts)
			output[index] = rawResponsesSearchJSON(message)
		}
	}
}

// A round is deliberately isolated from the downstream writer. Intermediate
// function calls and terminal events must never escape to the client.
type responsesWebSearchRoundWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
	size   int
	err    error
}

func (w *responsesWebSearchRoundWriter) Header() http.Header { return w.header }
func (w *responsesWebSearchRoundWriter) Status() int         { return w.status }
func (w *responsesWebSearchRoundWriter) Size() int           { return w.size }
func (w *responsesWebSearchRoundWriter) Written() bool       { return w.size >= 0 }
func (w *responsesWebSearchRoundWriter) WriteHeader(status int) {
	if !w.Written() {
		w.status = status
	}
}
func (w *responsesWebSearchRoundWriter) WriteHeaderNow() {
	if !w.Written() {
		w.size = 0
	}
}
func (w *responsesWebSearchRoundWriter) Write(data []byte) (int, error) {
	w.WriteHeaderNow()
	if w.body.Len()+len(data) > responsesWebSearchRoundLimit {
		w.err = errors.New("web search model round exceeds the response buffer limit")
		return 0, w.err
	}
	n, err := w.body.Write(data)
	w.size += n
	return n, err
}
func (w *responsesWebSearchRoundWriter) WriteString(data string) (int, error) {
	w.WriteHeaderNow()
	if w.body.Len()+len(data) > responsesWebSearchRoundLimit {
		w.err = errors.New("web search model round exceeds the response buffer limit")
		return 0, w.err
	}
	n, err := w.body.WriteString(data)
	w.size += n
	return n, err
}
func (w *responsesWebSearchRoundWriter) Flush()                   { w.WriteHeaderNow() }
func (w *responsesWebSearchRoundWriter) CloseNotify() <-chan bool { return nil }
func (w *responsesWebSearchRoundWriter) Pusher() http.Pusher      { return nil }
func (w *responsesWebSearchRoundWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("web search round cannot hijack an HTTP connection")
}

func parseResponsesWebSearchRound(data []byte) (map[string]json.RawMessage, error) {
	var response map[string]json.RawMessage
	if json.Unmarshal(data, &response) == nil && gjson.GetBytes(data, "object").String() == "response" {
		return response, nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), responsesWebSearchRoundLimit)
	var payload []byte
	var terminal map[string]json.RawMessage
	var streamError string
	var completedItems map[int]json.RawMessage
	consume := func() {
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			payload = payload[:0]
			return
		}
		var event map[string]json.RawMessage
		if json.Unmarshal(payload, &event) == nil {
			kind := gjson.GetBytes(event["type"], "@this").String()
			switch kind {
			case "response.output_item.done":
				var index int
				if json.Unmarshal(event["output_index"], &index) == nil && index >= 0 && gjson.GetBytes(event["item"], "@this").IsObject() {
					if completedItems == nil {
						completedItems = make(map[int]json.RawMessage)
					}
					completedItems[index] = event["item"]
				}
			case "response.completed", "response.incomplete", "response.failed", "response.done":
				var parsed map[string]json.RawMessage
				if json.Unmarshal(event["response"], &parsed) == nil && parsed != nil {
					terminal = parsed
				}
			case "error":
				streamError = gjson.GetBytes(payload, "message").String()
			}
		}
		payload = payload[:0]
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			consume()
		} else if bytes.HasPrefix(line, []byte("data:")) {
			if len(payload) > 0 {
				payload = append(payload, '\n')
			}
			value := line[len("data:"):]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			payload = append(payload, value...)
		}
	}
	consume()
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if terminal != nil {
		// Codex can leave terminal.output empty after sending complete items.
		// Restore those items without replacing an authoritative terminal output.
		var finalItems []json.RawMessage
		if len(completedItems) > 0 && (len(terminal["output"]) == 0 || json.Unmarshal(terminal["output"], &finalItems) == nil) && len(finalItems) == 0 {
			indices := make([]int, 0, len(completedItems))
			for index := range completedItems {
				indices = append(indices, index)
			}
			sort.Ints(indices)
			for _, index := range indices {
				finalItems = append(finalItems, completedItems[index])
			}
			terminal["output"] = rawResponsesSearchJSON(finalItems)
		}
		return terminal, nil
	}
	if streamError != "" {
		return nil, fmt.Errorf("upstream Responses error: %s", streamError)
	}
	return nil, errors.New("upstream returned no complete Responses object")
}

func isResponsesWebSearchClientCall(item json.RawMessage) bool {
	if gjson.GetBytes(item, "execution").String() == "client" {
		return true
	}
	switch gjson.GetBytes(item, "type").String() {
	case "function_call", "custom_tool_call", "local_shell_call", "shell_call", "computer_call", "apply_patch_call", "mcp_approval_request":
		return true
	default:
		return false
	}
}

// Preserve opaque/provider fields while supplying the same required empty
// arrays as apicompat's canonical Responses wire emitter.
func normalizeResponsesWebSearchItems(items []json.RawMessage) {
	for index, raw := range items {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		changed := false
		switch responsesSearchString(item["type"]) {
		case "reasoning":
			if len(item["summary"]) == 0 || string(item["summary"]) == "null" {
				item["summary"] = json.RawMessage(`[]`)
				changed = true
			}
		case "message":
			var parts []map[string]json.RawMessage
			if json.Unmarshal(item["content"], &parts) != nil {
				continue
			}
			for _, part := range parts {
				if responsesSearchString(part["type"]) != "output_text" {
					continue
				}
				for _, field := range []string{"annotations", "logprobs"} {
					if len(part[field]) == 0 || string(part[field]) == "null" {
						part[field] = json.RawMessage(`[]`)
						changed = true
					}
				}
			}
			if changed {
				item["content"] = rawResponsesSearchJSON(parts)
			}
		}
		if changed {
			items[index] = rawResponsesSearchJSON(item)
		}
	}
}

func addResponsesWebSearchCost(total, next *CostBreakdown) *CostBreakdown {
	if total == nil {
		if next == nil {
			return nil
		}
		copy := *next
		return &copy
	}
	if next == nil {
		return total
	}
	total.InputCost += next.InputCost
	total.ImageInputCost += next.ImageInputCost
	total.OutputCost += next.OutputCost
	total.ImageOutputCost += next.ImageOutputCost
	total.CacheCreationCost += next.CacheCreationCost
	total.CacheReadCost += next.CacheReadCost
	total.TotalCost += next.TotalCost
	total.ActualCost += next.ActualCost
	total.LongContextBillingApplied = total.LongContextBillingApplied || next.LongContextBillingApplied
	return total
}

func openAIResponsesWebSearchTokens(usage OpenAIUsage) UsageTokens {
	input := usage.InputTokens - usage.CacheReadInputTokens - usage.CacheCreationInputTokens
	if input < 0 {
		input = 0
	}
	imageInput := usage.ImageInputTokens - usage.ImageCacheReadTokens
	if imageInput < 0 {
		imageInput = 0
	}
	return UsageTokens{
		InputTokens: input, OutputTokens: usage.OutputTokens,
		ImageInputTokens: imageInput, ImageCacheReadTokens: usage.ImageCacheReadTokens,
		CacheCreationTokens: usage.CacheCreationInputTokens, CacheReadTokens: usage.CacheReadInputTokens,
		ImageOutputTokens: usage.ImageOutputTokens,
	}
}

func responsesWebSearchAccountStatsCost[T any](ctx context.Context, channels *ChannelService, billing *BillingService, accountID, groupID int64, rounds []*T, roundCosts []*CostBreakdown, _ float64, pricingAt time.Time, longContext bool) *float64 {
	if len(roundCosts) != len(rounds) {
		return nil
	}
	var total float64
	selectedPerRequest := -1
	for index, roundCost := range roundCosts {
		if roundCost == nil || roundCost.BillingMode != string(BillingModePerRequest) {
			continue
		}
		eligible := false
		switch result := any(rounds[index]).(type) {
		case *OpenAIForwardResult:
			eligible = result.ImageCount == 0 && result.VideoCount == 0 && result.AudioUsage == nil
		case *ForwardResult:
			eligible = result.ImageCount == 0 && result.AudioUsage == nil
		}
		if eligible && (selectedPerRequest < 0 || roundCost.ActualCost > roundCosts[selectedPerRequest].ActualCost) {
			selectedPerRequest = index
		}
	}
	for index, round := range rounds {
		var tokens UsageTokens
		var upstreamModel, model, tier, effort string
		var imageCount int
		roundCost := 0.0
		if roundCosts[index] != nil {
			roundCost = roundCosts[index].TotalCost
		}
		if selectedPerRequest >= 0 && index != selectedPerRequest && roundCosts[index] != nil && roundCosts[index].BillingMode == string(BillingModePerRequest) {
			continue
		}
		switch result := any(round).(type) {
		case *OpenAIForwardResult:
			tokens = openAIResponsesWebSearchTokens(result.Usage)
			upstreamModel, model, tier, effort = result.UpstreamModel, result.Model, optionalStringValue(result.ServiceTier), optionalStringValue(result.ReasoningEffort)
			imageCount = result.ImageCount
		case *ForwardResult:
			tokens = UsageTokens{InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens, CacheCreationTokens: result.Usage.CacheCreationInputTokens, CacheReadTokens: result.Usage.CacheReadInputTokens, CacheCreation5mTokens: result.Usage.CacheCreation5mTokens, CacheCreation1hTokens: result.Usage.CacheCreation1hTokens, ImageOutputTokens: result.Usage.ImageOutputTokens}
			upstreamModel, model, tier, effort = result.UpstreamModel, result.Model, optionalStringValue(result.ServiceTier), optionalStringValue(result.ReasoningEffort)
			imageCount = result.ImageCount
		}
		if upstreamModel == "" {
			upstreamModel = model
		}
		count := imageCount
		if count < 1 {
			count = 1
		}
		cost := resolveAccountStatsCost(ctx, channels, billing, accountID, groupID, upstreamModel, tokens, count, roundCost, tier, pricingAt, longContext, effort)
		if cost == nil {
			return nil
		}
		total += *cost
	}
	return &total
}
