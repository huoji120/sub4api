package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func hypothesisResponse(ticket, cookie string) *http.Response {
	header := http.Header{}
	if ticket != "" {
		header.Set(openAICodexTurnStateHeader, ticket)
	}
	if cookie != "" {
		header.Add("Set-Cookie", cookie)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"1, 2, 3\"}\n\n" +
			"data: {\"type\":\"response.completed\"}\n\n")),
	}
}

func TestCodexHypothesisRunnerUsesNormalPathAndInjectsTicket(t *testing.T) {
	defer SetCodexCanonicalUserAgentResolver(nil)
	state := "opaque-ticket"
	recorder := &httpUpstreamRecorder{responses: []*http.Response{
		hypothesisResponse(state, "oai-did=test; Path=/; Secure"),
		hypothesisResponse("", ""), hypothesisResponse("", ""),
		hypothesisResponse("", ""), hypothesisResponse("", ""), hypothesisResponse("", ""),
	}}
	runner, err := NewCodexHypothesisRunner(recorder, CodexHypothesisOptions{
		Token: "secret", ChatGPTAccountID: "account-test", Model: "gpt-5.6-sol",
	})
	require.NoError(t, err)
	challenges := []CodexHypothesisChallenge{
		{Prompt: "one", ExpectedCount: 292},
		{Prompt: "two", System: "system", ExpectedCount: 308},
		{Prompt: "three", UserPrefix: "prefix", ExpectedCount: 324},
	}
	baseline, err := runner.RunPhase(context.Background(), challenges, "", "")
	require.NoError(t, err)
	require.Equal(t, state, baseline.TicketState)
	require.Equal(t, "oai-did=test", baseline.TicketCookie)
	require.Len(t, baseline.Outputs, 3)
	ticketed, err := runner.RunPhase(context.Background(), challenges, baseline.TicketState, baseline.TicketCookie)
	require.NoError(t, err)
	require.Len(t, ticketed.Outputs, 3)
	require.Len(t, recorder.requests, 6)
	plainBody, err := recorder.requests[0].GetBody()
	require.NoError(t, err)
	defer plainBody.Close()
	var plainRequest map[string]any
	require.NoError(t, json.NewDecoder(plainBody).Decode(&plainRequest))
	require.NotEmpty(t, plainRequest["client_metadata"])
	require.NotContains(t, plainRequest, "prompt_cache_key")
	require.Len(t, plainRequest["input"], 1)
	for index, request := range recorder.requests {
		require.Equal(t, chatgptCodexURL, request.URL.String())
		require.Equal(t, buildCodexCLIUserAgent(codexHypothesisClientVersion), request.Header.Get("User-Agent"))
		require.Equal(t, "codex-tui", request.Header.Get("originator"))
		require.Equal(t, codexHypothesisClientVersion, request.Header.Get("version"))
		require.Equal(t, "Bearer secret", request.Header.Get("Authorization"))
		require.Equal(t, "account-test", request.Header.Get("ChatGPT-Account-ID"))
		if index < 3 {
			require.Empty(t, request.Header.Get(openAICodexTurnStateHeader))
			require.Empty(t, request.Header.Get("Cookie"))
		} else {
			require.Equal(t, state, request.Header.Get(openAICodexTurnStateHeader))
			require.Equal(t, "oai-did=test", request.Header.Get("Cookie"))
		}
	}
	require.NotContains(t, baseline.Headers, "Authorization")
	require.NotContains(t, baseline.Headers, "ChatGPT-Account-ID")
}

func TestCodexHypothesisStreamRejectsIncomplete(t *testing.T) {
	_, err := readCodexHypothesisStream(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n"))
	require.ErrorContains(t, err, "did not complete")
}

func TestCodexHypothesisRejectsWhitespaceTicket(t *testing.T) {
	defer SetCodexCanonicalUserAgentResolver(nil)
	runner, err := NewCodexHypothesisRunner(&httpUpstreamRecorder{}, CodexHypothesisOptions{
		Token: "secret", ChatGPTAccountID: "account-test", Model: "gpt-5.6-sol",
	})
	require.NoError(t, err)
	_, err = runner.RunPhase(context.Background(), make([]CodexHypothesisChallenge, 3), "   ", "")
	require.ErrorContains(t, err, "empty")
}

func TestCodexHypothesisCapturesOnlyCompletedResponse(t *testing.T) {
	defer SetCodexCanonicalUserAgentResolver(nil)
	response := hypothesisResponse("another-state-with-no-fixed-length", "oai-did=test; Path=/")
	response.Body = io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n"))
	recorder := &httpUpstreamRecorder{responses: []*http.Response{response}}
	runner, err := NewCodexHypothesisRunner(recorder, CodexHypothesisOptions{
		Token: "secret", ChatGPTAccountID: "account-test", Model: "gpt-5.6-sol",
	})
	require.NoError(t, err)
	challenges := []CodexHypothesisChallenge{{Prompt: "one", ExpectedCount: 292}, {Prompt: "two", ExpectedCount: 308}, {Prompt: "three", ExpectedCount: 324}}
	phase, err := runner.RunPhase(context.Background(), challenges, "", "")
	require.ErrorContains(t, err, "did not complete")
	require.Empty(t, phase.TicketState)
	require.Empty(t, phase.TicketCookie)
}

func TestCodexHypothesisAcceptsDifferentTicketLengths(t *testing.T) {
	defer SetCodexCanonicalUserAgentResolver(nil)
	for _, state := range []string{"x", strings.Repeat("z", 400)} {
		t.Run(fmt.Sprintf("length-%d", len(state)), func(t *testing.T) {
			recorder := &httpUpstreamRecorder{responses: []*http.Response{
				hypothesisResponse(state, ""), hypothesisResponse("", ""), hypothesisResponse("", ""),
				hypothesisResponse("", ""), hypothesisResponse("", ""), hypothesisResponse("", ""),
			}}
			runner, err := NewCodexHypothesisRunner(recorder, CodexHypothesisOptions{
				Token: "secret", ChatGPTAccountID: "account-test", Model: "gpt-5.6-sol",
			})
			require.NoError(t, err)
			challenges := []CodexHypothesisChallenge{{Prompt: "one", ExpectedCount: 292}, {Prompt: "two", ExpectedCount: 308}, {Prompt: "three", ExpectedCount: 324}}
			baseline, err := runner.RunPhase(context.Background(), challenges, "", "")
			require.NoError(t, err)
			require.Equal(t, state, baseline.TicketState)
			_, err = runner.RunPhase(context.Background(), challenges, baseline.TicketState, "")
			require.NoError(t, err)
			require.Equal(t, state, recorder.requests[3].Header.Get(openAICodexTurnStateHeader))
		})
	}
}

func TestCodexHypothesisFiltersUnrelatedCookies(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, chatgptCodexURL, nil)
	response := &http.Response{Header: http.Header{}}
	response.Header.Add("Set-Cookie", "valid=one; Domain=.chatgpt.com; Path=/backend-api; Secure")
	response.Header.Add("Set-Cookie", "foreign=two; Domain=example.com; Path=/")
	response.Header.Add("Set-Cookie", "other_path=three; Path=/other")
	response.Header.Add("Set-Cookie", "valid=duplicate; Path=/")
	require.Equal(t, "valid=one", extractCodexHypothesisCookies(response, request))
}

func TestCodexHypothesisProxyURLValidation(t *testing.T) {
	require.NoError(t, validateCodexHypothesisProxyURL("socks5://user:password@127.0.0.1:1080"))
	require.NoError(t, validateCodexHypothesisProxyURL("socks5h://localhost:1080"))
	require.Error(t, validateCodexHypothesisProxyURL("socks5://localhost"))
	require.Error(t, validateCodexHypothesisProxyURL("file:///tmp/proxy"))
}

func TestCodexHypothesisSingleChallengeKeepsActualHeaderValues(t *testing.T) {
	defer SetCodexCanonicalUserAgentResolver(nil)
	first := hypothesisResponse("first-state", "session=first; Path=/; Secure")
	first.Header.Add("Set-Cookie", "other=value; Path=/; Secure")
	second := hypothesisResponse("second-state", "session=second; Path=/; Secure")
	recorder := &httpUpstreamRecorder{responses: []*http.Response{first, second}}
	runner, err := NewCodexHypothesisRunner(recorder, CodexHypothesisOptions{Token: "secret", ChatGPTAccountID: "account-test", Model: "gpt-6-astra"})
	require.NoError(t, err)
	challenge := CodexHypothesisChallenge{Prompt: "single", ExpectedCount: 292}
	baseline, err := runner.RunChallenge(context.Background(), challenge, "", "")
	require.NoError(t, err)
	require.Empty(t, baseline.RequestState)
	require.Empty(t, baseline.RequestCookie)
	require.Equal(t, "first-state", baseline.TicketState)
	require.Equal(t, "session=first; other=value", baseline.TicketCookie)
	require.Equal(t, []string{"session=first; Path=/; Secure", "other=value; Path=/; Secure"}, baseline.ResponseCookies)
	verified, err := runner.RunChallenge(context.Background(), challenge, baseline.TicketState, baseline.TicketCookie)
	require.NoError(t, err)
	require.Equal(t, "first-state", verified.RequestState)
	require.Equal(t, "session=first; other=value", verified.RequestCookie)
	require.Equal(t, "second-state", verified.ResponseState)
	require.Equal(t, []string{"session=second; Path=/; Secure"}, verified.ResponseCookies)
	require.Equal(t, "first-state", recorder.requests[1].Header.Get(openAICodexTurnStateHeader))
}

func TestCodexHypothesisSingleChallengeReportsRateLimitHeaders(t *testing.T) {
	defer SetCodexCanonicalUserAgentResolver(nil)
	response := hypothesisResponse("rate-limited-state", "session=limited; Path=/")
	response.StatusCode = http.StatusTooManyRequests
	response.Header.Set("Retry-After", "45")
	runner, err := NewCodexHypothesisRunner(&httpUpstreamRecorder{responses: []*http.Response{response}}, CodexHypothesisOptions{Token: "secret", ChatGPTAccountID: "account-test", Model: "gpt-6-astra"})
	require.NoError(t, err)
	exchange, err := runner.RunChallenge(context.Background(), CodexHypothesisChallenge{Prompt: "single", ExpectedCount: 292}, "", "")
	require.ErrorContains(t, err, "HTTP 429")
	require.Equal(t, http.StatusTooManyRequests, exchange.StatusCode)
	require.Equal(t, "45", exchange.RetryAfter)
	require.Equal(t, "rate-limited-state", exchange.ResponseState)
	require.Empty(t, exchange.TicketState)
}

func TestCodexHypothesisSingleChallengeLogsRawHeaderButRejectsBlankTicket(t *testing.T) {
	defer SetCodexCanonicalUserAgentResolver(nil)
	response := hypothesisResponse("", "session=one; Path=/")
	response.Header.Set(openAICodexTurnStateHeader, "   ")
	runner, err := NewCodexHypothesisRunner(&httpUpstreamRecorder{responses: []*http.Response{response}}, CodexHypothesisOptions{Token: "secret", ChatGPTAccountID: "account-test", Model: "gpt-6-astra"})
	require.NoError(t, err)
	exchange, err := runner.RunChallenge(context.Background(), CodexHypothesisChallenge{Prompt: "single", ExpectedCount: 292}, "", "")
	require.NoError(t, err)
	require.Equal(t, "   ", exchange.ResponseState)
	require.Empty(t, exchange.TicketState)
	require.Empty(t, exchange.TicketCookie)
}

type concurrentCodexHypothesisUpstream struct {
	*httpUpstreamRecorder
	mu       sync.Mutex
	requests []*http.Request
	started  chan struct{}
	release  chan struct{}
}

func (upstream *concurrentCodexHypothesisUpstream) Do(request *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	upstream.mu.Lock()
	upstream.requests = append(upstream.requests, request)
	upstream.mu.Unlock()
	upstream.started <- struct{}{}
	<-upstream.release
	return hypothesisResponse("response-state", "session=response; Path=/"), nil
}
func TestCodexHypothesisConcurrentNormalHeadersAndTicketInjection(t *testing.T) {
	defer SetCodexCanonicalUserAgentResolver(nil)
	upstream := &concurrentCodexHypothesisUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{}, started: make(chan struct{}, 3), release: make(chan struct{})}
	runner, err := NewCodexHypothesisRunner(upstream, CodexHypothesisOptions{Token: "secret", ChatGPTAccountID: "account-test", Model: "gpt-6-astra"})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	results := make(chan error, 6)
	challenge := CodexHypothesisChallenge{Prompt: "single", ExpectedCount: 292}
	for index := 0; index < 3; index++ {
		go func() { _, err := runner.RunChallenge(ctx, challenge, "", ""); results <- err }()
	}
	for index := 0; index < 3; index++ {
		select {
		case <-upstream.started:
		case <-ctx.Done():
			t.Fatal("three baseline requests did not reach upstream concurrently")
		}
	}
	close(upstream.release)
	for index := 0; index < 3; index++ {
		require.NoError(t, <-results)
	}
	for index := 0; index < 3; index++ {
		go func() {
			_, err := runner.RunChallenge(ctx, challenge, "pinned-state", "session=pinned")
			results <- err
		}()
	}
	for index := 0; index < 3; index++ {
		require.NoError(t, <-results)
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	require.Len(t, upstream.requests, 6)
	injected := 0
	for _, request := range upstream.requests {
		require.Equal(t, buildCodexCLIUserAgent(codexHypothesisClientVersion), request.Header.Get("User-Agent"))
		require.Equal(t, "codex-tui", request.Header.Get("originator"))
		require.Equal(t, codexHypothesisClientVersion, request.Header.Get("version"))
		if request.Header.Get(openAICodexTurnStateHeader) == "pinned-state" {
			injected++
			require.Equal(t, "session=pinned", request.Header.Get("Cookie"))
		}
	}
	require.Equal(t, 3, injected)
}

type replayHypothesisUpstream struct {
	*httpUpstreamRecorder
	mu       sync.Mutex
	requests []*http.Request
}

func (upstream *replayHypothesisUpstream) Do(request *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	upstream.mu.Lock()
	upstream.requests = append(upstream.requests, request)
	upstream.mu.Unlock()
	return hypothesisResponse("response-state", "session=response; Path=/"), nil
}

func TestCodexHypothesisReplayConcurrentMetadataAndTicket(t *testing.T) {
	defer SetCodexCanonicalUserAgentResolver(nil)
	upstream := &replayHypothesisUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{}}
	replay := &CodexHypothesisReplay{
		Instructions:   "fixed system",
		InstallationID: "88b082f2-973a-4af1-a14c-88392661f6b3",
		Messages: []CodexHypothesisReplayMessage{
			{Role: "developer", Parts: []string{"fixed app context", "compact memory"}},
			{Role: "user", Parts: []string{"timezone Asia/Singapore"}},
			{Role: "user", Parts: []string{CodexHypothesisPromptPlaceholder}},
		},
	}
	runner, err := NewCodexHypothesisRunner(upstream, CodexHypothesisOptions{Token: "secret", ChatGPTAccountID: "account-test", Model: "gpt-6-astra", Replay: replay})
	require.NoError(t, err)
	other, err := NewCodexHypothesisRunner(upstream, CodexHypothesisOptions{Token: "secret", ChatGPTAccountID: "account-test", Model: "gpt-6-astra", Replay: replay})
	require.NoError(t, err)
	require.NotEqual(t, runner.sessionID, other.sessionID)
	for _, sessionID := range []string{runner.sessionID, runner.windowID} {
		parsed, err := uuid.Parse(sessionID)
		require.NoError(t, err)
		require.Equal(t, uuid.Version(7), parsed.Version())
	}
	challenge := CodexHypothesisChallenge{Prompt: "fresh ModelTrace question", ExpectedCount: 292}
	for phase := 0; phase < 2; phase++ {
		var workers sync.WaitGroup
		for index := 0; index < 3; index++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				state, cookie := "", ""
				if phase == 1 {
					state, cookie = "pinned-state", "session=pinned"
				}
				_, runErr := runner.RunChallenge(context.Background(), challenge, state, cookie)
				if runErr != nil {
					t.Errorf("run replay challenge: %v", runErr)
				}
			}()
		}
		workers.Wait()
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	require.Len(t, upstream.requests, 6)
	turns := make(map[string]bool)
	var injected int
	for _, request := range upstream.requests {
		var body struct {
			Instructions   string `json:"instructions"`
			PromptCacheKey string `json:"prompt_cache_key"`
			Input          []struct {
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
			ClientMetadata map[string]string `json:"client_metadata"`
		}
		data, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(data, &body))
		require.Equal(t, "fixed system", body.Instructions)
		require.Equal(t, runner.sessionID, body.PromptCacheKey)
		require.Len(t, body.Input, 3)
		require.Equal(t, "developer", body.Input[0].Role)
		require.Equal(t, "user", body.Input[1].Role)
		require.Equal(t, "timezone Asia/Singapore", body.Input[1].Content[0].Text)
		require.Equal(t, "fresh ModelTrace question", body.Input[2].Content[0].Text)
		require.NotContains(t, string(data), "324 个")
		require.NotContains(t, string(data), "secret")
		require.Equal(t, runner.sessionID, request.Header.Get("session_id"))
		require.Equal(t, runner.sessionID, request.Header.Get("session-id"))
		require.Equal(t, runner.sessionID, request.Header.Get("thread-id"))
		require.Equal(t, runner.sessionID, request.Header.Get("x-client-request-id"))
		require.Equal(t, runner.windowID, request.Header.Get("x-codex-window-id"))
		require.Equal(t, replay.InstallationID, request.Header.Get("x-codex-installation-id"))
		require.Equal(t, buildCodexCLIUserAgent(codexHypothesisClientVersion), request.Header.Get("User-Agent"))
		require.Equal(t, "codex-tui", request.Header.Get("originator"))
		require.Equal(t, "0.156.1", request.Header.Get("version"))
		require.Empty(t, request.Header.Get("conversation_id"))
		require.Equal(t, runner.sessionID, body.ClientMetadata["session_id"])
		require.Equal(t, runner.sessionID, body.ClientMetadata["thread_id"])
		require.Equal(t, runner.windowID, body.ClientMetadata["x-codex-window-id"])
		require.Equal(t, replay.InstallationID, body.ClientMetadata["x-codex-installation-id"])
		require.Equal(t, request.Header.Get("x-codex-turn-metadata"), body.ClientMetadata["x-codex-turn-metadata"])
		var turnMetadata struct {
			SessionID   string `json:"session_id"`
			ThreadID    string `json:"thread_id"`
			TurnID      string `json:"turn_id"`
			WindowID    string `json:"window_id"`
			RequestKind string `json:"request_kind"`
			StartedAt   int64  `json:"turn_started_at_unix_ms"`
		}
		require.NoError(t, json.Unmarshal([]byte(request.Header.Get("x-codex-turn-metadata")), &turnMetadata))
		require.Equal(t, runner.sessionID, turnMetadata.SessionID)
		require.Equal(t, runner.sessionID, turnMetadata.ThreadID)
		require.Equal(t, runner.windowID, turnMetadata.WindowID)
		require.Equal(t, "turn", turnMetadata.RequestKind)
		require.Positive(t, turnMetadata.StartedAt)
		require.Equal(t, body.ClientMetadata["turn_id"], turnMetadata.TurnID)
		parsed, err := uuid.Parse(turnMetadata.TurnID)
		require.NoError(t, err)
		require.Equal(t, uuid.Version(7), parsed.Version())
		require.False(t, turns[turnMetadata.TurnID])
		turns[turnMetadata.TurnID] = true
		if request.Header.Get(openAICodexTurnStateHeader) != "" {
			injected++
			require.Equal(t, "pinned-state", request.Header.Get(openAICodexTurnStateHeader))
			require.Equal(t, "session=pinned", request.Header.Get("Cookie"))
		}
	}
	require.Equal(t, 3, injected)
	tampered, err := runner.buildRequest(context.Background(), challenge)
	require.NoError(t, err)
	tampered.Header.Set("thread-id", "unrelated-thread")
	require.ErrorContains(t, runner.checkReplayIdentity(tampered), "headers and body identity differ")
}
