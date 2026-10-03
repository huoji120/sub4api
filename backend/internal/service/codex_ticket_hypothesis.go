package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const codexHypothesisClientVersion = "0.156.1"
const CodexHypothesisPromptPlaceholder = "{{MODELTRACE_PROMPT}}"

type CodexHypothesisReplayMessage struct {
	Role  string
	Parts []string
}

type CodexHypothesisReplay struct {
	Instructions   string
	Messages       []CodexHypothesisReplayMessage
	InstallationID string
	RecordedModel  string
}

type CodexHypothesisChallenge struct {
	Prompt        string `json:"prompt"`
	System        string `json:"system"`
	UserPrefix    string `json:"user_prefix"`
	ExpectedCount int    `json:"expected_count"`
}

type CodexHypothesisOptions struct {
	Token            string
	ChatGPTAccountID string
	Model            string
	ProxyURL         string
	Replay           *CodexHypothesisReplay
}

type CodexHypothesisOutput struct {
	Text          string `json:"text"`
	ExpectedCount int    `json:"expected_count"`
}

type CodexHypothesisPhase struct {
	Outputs      []CodexHypothesisOutput
	TicketState  string
	TicketCookie string
	Statuses     []int
	Headers      map[string]string
}

type CodexHypothesisRunner struct {
	service         *OpenAIGatewayService
	account         *Account
	options         CodexHypothesisOptions
	sessionID       string
	windowID        string
	baselineMu      sync.Mutex
	baselineHeaders http.Header
}

func NewCodexHypothesisRunner(upstream HTTPUpstream, options CodexHypothesisOptions) (*CodexHypothesisRunner, error) {
	if upstream == nil || strings.TrimSpace(options.Token) == "" || strings.TrimSpace(options.ChatGPTAccountID) == "" || strings.TrimSpace(options.Model) == "" {
		return nil, errors.New("HTTP upstream, access token, ChatGPT account ID and model are required")
	}
	if options.ProxyURL != "" {
		if err := validateCodexHypothesisProxyURL(options.ProxyURL); err != nil {
			return nil, err
		}
	}
	sessionID := uuid.NewString()
	windowID := ""
	if options.Replay != nil {
		if strings.TrimSpace(options.Replay.Instructions) == "" || len(options.Replay.Messages) == 0 {
			return nil, errors.New("invalid Codex Desktop replay context")
		}
		installationID, err := uuid.Parse(options.Replay.InstallationID)
		if err != nil || installationID.Version() != 4 {
			return nil, errors.New("replay installation ID must be UUIDv4")
		}
		var placeholderCount int
		for _, message := range options.Replay.Messages {
			if message.Role != "developer" && message.Role != "user" {
				return nil, errors.New("invalid replay message role")
			}
			for _, part := range message.Parts {
				placeholderCount += strings.Count(part, CodexHypothesisPromptPlaceholder)
			}
		}
		if placeholderCount != 1 {
			return nil, errors.New("replay requires exactly one challenge placeholder")
		}
		generated, err := uuid.NewV7()
		if err != nil {
			return nil, fmt.Errorf("generate replay session ID: %w", err)
		}
		sessionID = generated.String()
		generated, err = uuid.NewV7()
		if err != nil {
			return nil, fmt.Errorf("generate replay window ID: %w", err)
		}
		windowID = generated.String()
	}
	SetCodexCanonicalUserAgentResolver(func() string { return buildCodexCLIUserAgent(codexHypothesisClientVersion) })
	SetCodexIdentityEnforcementEnabled(true)
	account := &Account{
		ID:       1,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       options.Token,
			"chatgpt_account_id": options.ChatGPTAccountID,
		},
	}
	cfg := &config.Config{}
	return &CodexHypothesisRunner{
		service:   &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream},
		account:   account,
		options:   options,
		sessionID: sessionID,
		windowID:  windowID,
	}, nil
}

func (runner *CodexHypothesisRunner) RunPhase(ctx context.Context, challenges []CodexHypothesisChallenge, ticketState, ticketCookie string) (CodexHypothesisPhase, error) {
	phase := CodexHypothesisPhase{}
	if len(challenges) != 3 {
		return phase, errors.New("ModelTrace requires three challenges")
	}
	if ticketState != "" && strings.TrimSpace(ticketState) == "" {
		return phase, errors.New("turn-state is empty")
	}
	for index, challenge := range challenges {
		if strings.TrimSpace(challenge.Prompt) == "" || challenge.ExpectedCount <= 0 {
			return phase, errors.New("invalid ModelTrace challenge")
		}
		requestCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
		request, err := runner.buildRequest(requestCtx, challenge)
		if err != nil {
			cancel()
			return phase, err
		}
		if ticketState != "" {
			request.Header.Set(openAICodexTurnStateHeader, ticketState)
			applyCodexHypothesisCookie(request.Header, ticketCookie)
		}
		if ticketState != "" && request.Header.Get(openAICodexTurnStateHeader) != ticketState {
			cancel()
			return phase, errors.New("normal request path did not inject the ticket")
		}
		if ticketCookie != "" && !strings.Contains(request.Header.Get("Cookie"), ticketCookie) {
			cancel()
			return phase, errors.New("normal request path did not inject the cookie")
		}
		if err := runner.checkHeaders(request, ticketState != ""); err != nil {
			cancel()
			return phase, err
		}
		if phase.Headers == nil {
			phase.Headers = safeCodexHypothesisHeaders(request)
		}
		response, err := runner.service.doOpenAIUpstream(request, runner.options.ProxyURL, runner.account)
		if err != nil {
			cancel()
			return phase, fmt.Errorf("upstream request failed: %w", err)
		}
		if response == nil {
			cancel()
			return phase, errors.New("upstream returned a nil response")
		}
		phase.Statuses = append(phase.Statuses, response.StatusCode)
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			cancel()
			return phase, fmt.Errorf("upstream HTTP %d: inconclusive", response.StatusCode)
		}
		text, readErr := readCodexHypothesisStream(response.Body)
		_ = response.Body.Close()
		cancel()
		if readErr != nil {
			return phase, fmt.Errorf("challenge %d: %w", index+1, readErr)
		}
		if state := extractOpenAICodexTurnState(response.Header); state != "" {
			phase.TicketState = state
			phase.TicketCookie = extractCodexHypothesisCookies(response, request)
		}
		phase.Outputs = append(phase.Outputs, CodexHypothesisOutput{Text: text, ExpectedCount: challenge.ExpectedCount})
	}
	return phase, nil
}

func (runner *CodexHypothesisRunner) buildRequest(ctx context.Context, challenge CodexHypothesisChallenge) (*http.Request, error) {
	userText := challenge.Prompt
	if challenge.UserPrefix != "" {
		userText = challenge.UserPrefix + "\n\n" + userText
	}
	body := map[string]any{
		"model": runner.options.Model, "store": false, "stream": true,
		"input": []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_text", "text": userText}}}},
	}
	if challenge.System != "" {
		body["instructions"] = challenge.System
	}
	var replayHeaders http.Header
	if replay := runner.options.Replay; replay != nil {
		identity, err := newCodexReplayIdentity(replay.InstallationID, runner.sessionID, runner.windowID, time.Now())
		if err != nil {
			return nil, fmt.Errorf("generate replay identity: %w", err)
		}
		body, replayHeaders = buildCodexReplayRequest(replay, runner.options.Model, challenge.System, identity,
			strings.NewReplacer(CodexHypothesisPromptPlaceholder, userText).Replace)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	context := &gin.Context{}
	context.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	context.Request.Header.Set("session_id", runner.sessionID)
	request, err := runner.service.buildUpstreamRequestOpenAIPassthrough(ctx, context, runner.account, payload, runner.options.Token)
	if err != nil || runner.options.Replay == nil {
		return request, err
	}
	for name, values := range replayHeaders {
		request.Header[name] = values
	}
	request.Header.Del("conversation_id")
	if err := runner.checkReplayIdentity(request); err != nil {
		return nil, err
	}
	return request, nil
}

func (runner *CodexHypothesisRunner) checkReplayIdentity(request *http.Request) error {
	if request.GetBody == nil {
		return errors.New("replay request body cannot be checked")
	}
	reader, err := request.GetBody()
	if err != nil {
		return fmt.Errorf("read replay request body: %w", err)
	}
	defer reader.Close()
	var body struct {
		PromptCacheKey string            `json:"prompt_cache_key"`
		ClientMetadata map[string]string `json:"client_metadata"`
	}
	if err := json.NewDecoder(reader).Decode(&body); err != nil {
		return fmt.Errorf("decode replay request body: %w", err)
	}
	metadata := request.Header.Get("x-codex-turn-metadata")
	var turn struct {
		SessionID      string `json:"session_id"`
		ThreadID       string `json:"thread_id"`
		TurnID         string `json:"turn_id"`
		InstallationID string `json:"installation_id"`
		WindowID       string `json:"window_id"`
		RequestKind    string `json:"request_kind"`
		StartedAt      int64  `json:"turn_started_at_unix_ms"`
	}
	if err := json.Unmarshal([]byte(metadata), &turn); err != nil {
		return errors.New("invalid outbound replay turn metadata")
	}
	if body.PromptCacheKey != runner.sessionID || body.ClientMetadata["session_id"] != runner.sessionID ||
		body.ClientMetadata["thread_id"] != runner.sessionID || body.ClientMetadata["turn_id"] != turn.TurnID ||
		body.ClientMetadata["x-codex-installation-id"] != runner.options.Replay.InstallationID ||
		body.ClientMetadata["x-codex-window-id"] != runner.windowID ||
		body.ClientMetadata["x-codex-turn-metadata"] != metadata ||
		request.Header.Get("session_id") != runner.sessionID || request.Header.Get("session-id") != runner.sessionID ||
		request.Header.Get("thread-id") != runner.sessionID || request.Header.Get("x-client-request-id") != runner.sessionID ||
		request.Header.Get("x-codex-installation-id") != runner.options.Replay.InstallationID ||
		request.Header.Get("x-codex-window-id") != runner.windowID ||
		turn.SessionID != runner.sessionID || turn.ThreadID != runner.sessionID || turn.TurnID == "" ||
		turn.InstallationID != runner.options.Replay.InstallationID || turn.WindowID != runner.windowID ||
		turn.RequestKind != "turn" || turn.StartedAt <= 0 {
		return errors.New("outbound replay headers and body identity differ")
	}
	return nil
}

func (runner *CodexHypothesisRunner) checkHeaders(request *http.Request, ticketed bool) error {
	if request.Method != http.MethodPost || request.URL.String() != chatgptCodexURL || request.Host != "chatgpt.com" {
		return errors.New("normal request path changed method, URL or Host")
	}
	if request.Header.Get("User-Agent") != buildCodexCLIUserAgent(codexHypothesisClientVersion) ||
		request.Header.Get("originator") != "codex-tui" || request.Header.Get("version") != codexHypothesisClientVersion {
		return errors.New("outbound Codex identity does not match pinned version")
	}
	runner.baselineMu.Lock()
	defer runner.baselineMu.Unlock()
	current := request.Header.Clone()
	current.Del("Cookie")
	current.Del(openAICodexTurnStateHeader)
	// Turn metadata varies by logical prompt; transport identity and stable
	// session/thread headers remain part of the baseline comparison.
	current.Del("x-codex-turn-metadata")
	if !ticketed && runner.baselineHeaders == nil {
		runner.baselineHeaders = current
	}
	if runner.baselineHeaders != nil && !reflect.DeepEqual(runner.baselineHeaders, current) {
		return errors.New("non-ticket outbound headers differ from the baseline")
	}
	return nil
}

func safeCodexHypothesisHeaders(request *http.Request) map[string]string {
	result := map[string]string{"Host": request.Host, "URL": request.URL.String(), "Method": request.Method}
	for _, name := range []string{"Accept", "Content-Type", "User-Agent", "originator", "version", "OpenAI-Beta"} {
		if value := request.Header.Get(name); value != "" {
			result[name] = value
		}
	}
	names := make([]string, 0, len(request.Header))
	for name := range request.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	result["Header-Names"] = strings.Join(names, ", ")
	return result
}

func readCodexHypothesisStream(body io.Reader) (string, error) {
	scanner := bufio.NewScanner(io.LimitReader(body, 4<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var output strings.Builder
	completed := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			Delta    string `json:"delta"`
			Response struct {
				Output []struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"output"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		switch event.Type {
		case "response.output_text.delta":
			output.WriteString(event.Delta)
		case "response.completed":
			completed = true
			if output.Len() == 0 {
				for _, item := range event.Response.Output {
					for _, content := range item.Content {
						output.WriteString(content.Text)
					}
				}
			}
		case "response.failed", "error":
			return "", errors.New("upstream stream reported failure: inconclusive")
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read upstream stream after %d text bytes: %w", output.Len(), err)
	}
	if !completed || strings.TrimSpace(output.String()) == "" {
		return "", errors.New("upstream stream did not complete with text: inconclusive")
	}
	return output.String(), nil
}

func applyCodexHypothesisCookie(headers http.Header, cookie string) {
	if cookie == "" {
		return
	}
	if existing := headers.Get("Cookie"); existing != "" {
		cookie = existing + "; " + cookie
	}
	headers.Set("Cookie", cookie)
}

func extractCodexHypothesisCookies(response *http.Response, request *http.Request) string {
	if response == nil || request == nil || request.URL == nil {
		return ""
	}
	host := request.URL.Hostname()
	path := request.URL.Path
	seen := make(map[string]bool)
	pairs := make([]string, 0)
	for _, cookie := range response.Cookies() {
		if cookie.Name == "" || seen[cookie.Name] || cookie.MaxAge < 0 || (!cookie.Expires.IsZero() && !cookie.Expires.After(time.Now())) {
			continue
		}
		domain := strings.TrimPrefix(strings.ToLower(cookie.Domain), ".")
		if domain != "" && !strings.EqualFold(host, domain) && !strings.HasSuffix(strings.ToLower(host), "."+domain) {
			continue
		}
		cookiePath := cookie.Path
		if cookiePath != "" && path != cookiePath && !(strings.HasPrefix(path, cookiePath) && (strings.HasSuffix(cookiePath, "/") || strings.HasPrefix(strings.TrimPrefix(path, cookiePath), "/"))) {
			continue
		}
		if cookie.Secure && request.URL.Scheme != "https" {
			continue
		}
		seen[cookie.Name] = true
		pairs = append(pairs, cookie.Name+"="+cookie.Value)
	}
	return strings.Join(pairs, "; ")
}

func validateCodexHypothesisProxyURL(raw string) error {
	proxy, err := url.Parse(raw)
	if err != nil || proxy.Hostname() == "" || proxy.Port() == "" {
		return errors.New("proxy URL requires a scheme, host and port")
	}
	switch proxy.Scheme {
	case "http", "https", "socks5", "socks5h":
		return nil
	default:
		return errors.New("unsupported proxy URL scheme")
	}
}

type CodexHypothesisExchange struct {
	Output          CodexHypothesisOutput
	RequestState    string
	RequestCookie   string
	ResponseState   string
	ResponseCookies []string
	TicketState     string
	TicketCookie    string
	StatusCode      int
	RetryAfter      string
	RequestPrepared bool
}

func (runner *CodexHypothesisRunner) RunChallenge(ctx context.Context, challenge CodexHypothesisChallenge, ticketState, ticketCookie string) (CodexHypothesisExchange, error) {
	result := CodexHypothesisExchange{}
	if strings.TrimSpace(challenge.Prompt) == "" || challenge.ExpectedCount <= 0 {
		return result, errors.New("invalid ModelTrace challenge")
	}
	if ticketState != "" && strings.TrimSpace(ticketState) == "" {
		return result, errors.New("turn-state is empty")
	}
	requestCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	request, err := runner.buildRequest(requestCtx, challenge)
	if err != nil {
		return result, err
	}
	if ticketState != "" {
		request.Header.Set(openAICodexTurnStateHeader, ticketState)
		applyCodexHypothesisCookie(request.Header, ticketCookie)
	}
	if request.Header.Get(openAICodexTurnStateHeader) != ticketState {
		return result, errors.New("normal request path did not inject the ticket")
	}
	if ticketCookie != "" && !strings.Contains(request.Header.Get("Cookie"), ticketCookie) {
		return result, errors.New("normal request path did not inject the cookie")
	}
	if err := runner.checkHeaders(request, ticketState != ""); err != nil {
		return result, err
	}
	result.RequestState = request.Header.Get(openAICodexTurnStateHeader)
	result.RequestCookie = request.Header.Get("Cookie")
	result.RequestPrepared = true
	response, err := runner.service.doOpenAIUpstream(request, runner.options.ProxyURL, runner.account)
	if err != nil {
		return result, fmt.Errorf("upstream request failed: %w", err)
	}
	if response == nil {
		return result, errors.New("upstream returned a nil response")
	}
	defer response.Body.Close()
	result.StatusCode = response.StatusCode
	result.ResponseState = response.Header.Get(openAICodexTurnStateHeader)
	result.ResponseCookies = append([]string(nil), response.Header.Values("Set-Cookie")...)
	result.RetryAfter = response.Header.Get("Retry-After")
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("upstream HTTP %d: inconclusive", response.StatusCode)
	}
	text, err := readCodexHypothesisStream(response.Body)
	if err != nil {
		return result, err
	}
	result.Output = CodexHypothesisOutput{Text: text, ExpectedCount: challenge.ExpectedCount}
	if state := extractOpenAICodexTurnState(response.Header); state != "" {
		result.TicketState = state
		result.TicketCookie = extractCodexHypothesisCookies(response, request)
	}
	return result, nil
}
