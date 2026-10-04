package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type claudeControlCache struct {
	stubIdentityCache
	headers  map[int64]http.Header
	readErr  error
	writeErr error
	updates  int
}

func (c *claudeControlCache) GetClaudeCodeHeaders(_ context.Context, accountID int64) (http.Header, error) {
	return c.headers[accountID].Clone(), c.readErr
}

func (c *claudeControlCache) UpdateClaudeCodeHeaders(_ context.Context, accountID int64, headers http.Header) error {
	if c.writeErr != nil {
		return c.writeErr
	}
	c.updates++
	if c.headers == nil {
		c.headers = make(map[int64]http.Header)
	}
	if c.headers[accountID] == nil {
		c.headers[accountID] = make(http.Header)
	}
	for name, values := range headers.Clone() {
		c.headers[accountID][name] = values
	}
	return nil
}

func buildClaudeControlTestRequest(t *testing.T, s *GatewayService, endpoint string, account *Account, incoming http.Header, mimic bool) *http.Request {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header = incoming
	body := []byte(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hello"}]}`)
	tokenType := "oauth"
	if account.Type == AccountTypeAPIKey {
		tokenType = "apikey"
	}
	var req *http.Request
	var err error
	if endpoint == "messages" {
		req, _, err = s.buildUpstreamRequest(context.Background(), c, account, body, "test-token", tokenType, "claude-haiku-4-5", false, mimic)
	} else {
		req, _, err = s.buildCountTokensRequest(context.Background(), c, account, body, "test-token", tokenType, "claude-haiku-4-5", mimic)
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = req.Body.Close() })
	return req
}

func TestClaudeCodeControlHeaders_ResponseToNextRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"messages", "count_tokens"} {
		for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
			t.Run(endpoint+"/"+accountType, func(t *testing.T) {
				cache := &claudeControlCache{}
				upstream := &anthropicHTTPUpstreamRecorder{}
				s := &GatewayService{cfg: &config.Config{}, identityService: NewIdentityService(cache), httpUpstream: upstream}
				account := &Account{ID: 41, Platform: PlatformAnthropic, Type: accountType}
				first := buildClaudeControlTestRequest(t, s, endpoint, account, http.Header{}, true)
				require.Empty(t, filterClaudeCodeControlHeaders(first.Header))
				supplied := http.Header{}
				for _, name := range claudeCodeControlHeaderNames {
					supplied[http.CanonicalHeaderKey(name)] = []string{"initial-" + name, "second"}
				}
				supplied.Set("Authorization", "never-cache")
				supplied.Set("X-Unapproved", "never-cache")
				upstream.resp = &http.Response{StatusCode: http.StatusBadRequest, Header: supplied, Body: io.NopCloser(strings.NewReader("error body"))}
				resp, err := s.doClaudeUpstreamRequest(context.Background(), first, "", account, nil)
				require.NoError(t, err)
				require.Same(t, upstream.resp, resp)
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.Equal(t, "error body", string(body))
				_ = resp.Body.Close()
				require.Equal(t, filterClaudeCodeControlHeaders(supplied), cache.headers[account.ID])
				retry := buildClaudeControlTestRequest(t, s, endpoint, account, http.Header{}, true)
				require.Equal(t, cache.headers[account.ID], filterClaudeCodeControlHeaders(retry.Header))

				// A partial response replaces the named field, retains all others,
				// and explicitly clears a prior nonempty value without deleting it.
				upstream.resp = &http.Response{StatusCode: http.StatusOK, Header: http.Header{
					"Anthropic-Usage-Limit":   {"replacement"},
					"X-Cc-Compaction-Request": {""},
				}, Body: io.NopCloser(strings.NewReader("data: unchanged\n\n"))}
				resp, err = s.doClaudeUpstreamRequest(context.Background(), retry, "", account, nil)
				require.NoError(t, err)
				_ = resp.Body.Close()
				next := buildClaudeControlTestRequest(t, s, endpoint, account, http.Header{}, true)
				got := filterClaudeCodeControlHeaders(next.Header)
				require.Equal(t, []string{"replacement"}, got["anthropic-usage-limit"])
				require.Equal(t, []string{""}, got["x-cc-compaction-request"])
				require.Equal(t, []string{"initial-x-claude-code-compaction", "second"}, got["x-claude-code-compaction"])
				require.Equal(t, cache.headers[account.ID], got)

				caller := http.Header{}
				for _, name := range claudeCodeControlHeaderNames {
					caller[name] = []string{"caller-" + name, "caller-second"}
				}
				caller["x-cc-compaction-request"] = []string{""}
				caller["x-claude-code-compaction"] = []string{}
				caller["x-claude-code-context-compacted"] = nil
				explicit := buildClaudeControlTestRequest(t, s, endpoint, account, caller, true)
				got = filterClaudeCodeControlHeaders(explicit.Header)
				for name, values := range caller {
					actual, present := got[name]
					require.True(t, present, name)
					require.Equal(t, values, actual, name)
				}
				passthrough := buildClaudeControlTestRequest(t, s, endpoint, account, caller, false)
				require.Equal(t, caller, filterClaudeCodeControlHeaders(passthrough.Header))
				isolated := buildClaudeControlTestRequest(t, s, endpoint, &Account{ID: 42, Platform: PlatformAnthropic, Type: accountType}, http.Header{}, true)
				require.Empty(t, filterClaudeCodeControlHeaders(isolated.Header))
			})
		}
	}
}

func TestClaudeCodeControlHeaders_ModeAndProviderBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name        string
		platform    string
		accountType string
		mimic       bool
		learn       bool
		use         bool
	}{
		{"oauth_mimic", PlatformAnthropic, AccountTypeOAuth, true, true, true},
		{"oauth_passthrough", PlatformAnthropic, AccountTypeOAuth, false, true, false},
		{"setup_mimic", PlatformAnthropic, AccountTypeSetupToken, true, true, true},
		{"api_key", PlatformAnthropic, AccountTypeAPIKey, true, false, false},
		{"vertex", PlatformAnthropic, AccountTypeServiceAccount, true, false, false},
		{"openai_oauth", PlatformOpenAI, AccountTypeOAuth, true, false, false},
		{"gemini_oauth", PlatformGemini, AccountTypeOAuth, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := &claudeControlCache{headers: map[int64]http.Header{1: {"anthropic-usage-limit": {"old"}, "authorization": {"poisoned"}}}}
			upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{StatusCode: 503, Header: http.Header{"Anthropic-Usage-Limit": {"new"}}, Body: io.NopCloser(strings.NewReader("busy"))}}
			s := &GatewayService{identityService: NewIdentityService(cache), httpUpstream: upstream}
			account := &Account{ID: 1, Platform: tc.platform, Type: tc.accountType}
			req := httptest.NewRequest(http.MethodPost, "https://upstream.test", nil)
			req.Header.Set("Authorization", "Bearer unchanged")
			resp, err := s.doClaudeUpstreamRequest(context.Background(), req, "", account, nil)
			require.NoError(t, err)
			_ = resp.Body.Close()
			if tc.learn {
				require.Equal(t, 1, cache.updates)
			} else {
				require.Zero(t, cache.updates)
			}
			s.applyClaudeCodeControlHeaders(context.Background(), req, http.Header{}, account, "oauth", tc.mimic)
			if tc.use {
				require.Equal(t, http.Header{"anthropic-usage-limit": {"new"}}, filterClaudeCodeControlHeaders(req.Header))
			} else {
				require.Empty(t, filterClaudeCodeControlHeaders(req.Header))
			}
			require.Equal(t, "Bearer unchanged", req.Header.Get("Authorization"))
		})
	}
}

func TestClaudeCodeControlHeaders_MissingFieldsAndCacheFailure(t *testing.T) {
	cache := &claudeControlCache{headers: map[int64]http.Header{1: {"anthropic-usage-limit": {"retained"}}}}
	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Authorization": {"not-approved"}}, Body: io.NopCloser(strings.NewReader("data: original\n\n"))}}
	s := &GatewayService{identityService: NewIdentityService(cache), httpUpstream: upstream}
	account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	req := httptest.NewRequest(http.MethodPost, "https://upstream.test", nil)
	resp, err := s.doClaudeUpstreamRequest(context.Background(), req, "", account, nil)
	require.NoError(t, err)
	require.Zero(t, cache.updates)
	require.Equal(t, []string{"retained"}, cache.headers[1]["anthropic-usage-limit"])
	cache.writeErr = errors.New("cache unavailable")
	upstream.resp.Header.Set("Anthropic-Usage-Limit", "ignored-on-cache-error")
	resp, err = s.doClaudeUpstreamRequest(context.Background(), req, "", account, nil)
	require.NoError(t, err)
	require.Same(t, upstream.resp, resp)
	require.Equal(t, 200, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "data: original\n\n", string(body))
	_ = resp.Body.Close()
	cache.readErr = errors.New("cache unavailable")
	caller := http.Header{"x-cc-compaction-request": {"caller"}}
	s.applyClaudeCodeControlHeaders(context.Background(), req, caller, account, "oauth", true)
	require.Equal(t, caller, filterClaudeCodeControlHeaders(req.Header))
}
