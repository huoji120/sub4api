package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexPassthroughAcceptMatchesUpstreamProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name        string
		accountType string
		path        string
		wantAccept  string
	}{
		{"oauth_stream", AccountTypeOAuth, "/v1/responses", "text/event-stream"},
		{"oauth_compact", AccountTypeOAuth, "/v1/responses/compact", "application/json"},
		{"api_key_stream", AccountTypeAPIKey, "/v1/responses", "application/json"},
		{"api_key_compact", AccountTypeAPIKey, "/v1/responses/compact", "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"codex-test","stream":true,"input":"hello"}`)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(string(body)))
			c.Request.Header.Set("Accept", "application/json")
			account := &Account{
				ID: 17, Platform: PlatformOpenAI, Type: tc.accountType,
				Credentials: map[string]any{"chatgpt_account_id": "test-account"},
			}
			svc := &OpenAIGatewayService{}
			request, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "test-token")
			require.NoError(t, err)
			require.Equal(t, tc.wantAccept, request.Header.Get("Accept"))
			require.Equal(t, "application/json", c.Request.Header.Get("Accept"), "ingress negotiation must not be mutated during failover")
			_ = request.Body.Close()
		})
	}
}

// These fixtures deliberately supply different session/thread and header/body
// identities. Check the outbound protocol invariant, not a private hash formula.
func requireCodexDistinctThreadMetadata(t *testing.T, headers http.Header, body []byte) {
	t.Helper()
	cm := gjson.GetBytes(body, "client_metadata")
	session, thread := cm.Get("session_id").String(), cm.Get("thread_id").String()
	require.NotEmpty(t, session)
	require.NotEmpty(t, thread)
	require.NotEqual(t, session, thread, "independent threads must not collapse into the root session")
	require.NotEqual(t, "body-thread", thread, "client identity must remain credential-scoped")
	require.Equal(t, session, headers.Get("session-id"))
	require.Equal(t, session, headers.Get("session_id"))
	require.Equal(t, thread, headers.Get("thread-id"))
	require.Equal(t, thread, headers.Get("x-client-request-id"))
	require.Equal(t, thread+":0", cm.Get("x-codex-window-id").String())
	require.Equal(t, cm.Get("x-codex-window-id").String(), headers.Get("x-codex-window-id"))
	require.Equal(t, session, gjson.GetBytes(body, "prompt_cache_key").String())
	metadata := cm.Get(openAIWSTurnMetadataHeader).String()
	require.JSONEq(t, metadata, headers.Get(openAIWSTurnMetadataHeader))
	require.Equal(t, session, gjson.Get(metadata, "session_id").String())
	require.Equal(t, thread, gjson.Get(metadata, "thread_id").String())
	require.Equal(t, "seatbelt", gjson.Get(metadata, "sandbox").String())
	require.Positive(t, gjson.Get(metadata, "turn_started_at_unix_ms").Int(), "Codex metadata should include a turn start timestamp")
}

func TestCodexRequestMetadataPreservesCanonicalCompactionAndPrecision(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	body := []byte(`{"input":[{"type":"compaction_trigger"}],"client_metadata":{"session_id":"stale-flat-session","thread_id":"stale-flat-thread","x-codex-turn-metadata":"{\"session_id\":\"body-session\",\"thread_id\":\"body-thread\",\"turn_id\":\"same-turn\",\"window_id\":\"body-thread:3\",\"request_kind\":\"turn\",\"turn_started_at_unix_ms\":9007199254740993,\"compaction\":{\"trigger\":\"manual\",\"reason\":\"user_requested\"},\"sandbox\":\"seatbelt\"}"}}`)
	headers := http.Header{}
	headers.Set("session-id", "stale-header-session")
	headers.Set("thread-id", "stale-header-thread")
	headers.Set("x-client-request-id", "stale-request")
	headers.Set(openAIWSTurnMetadataHeader, `{"session_id":"stale-header-session","thread_id":"stale-header-thread"}`)
	next, changed, err := normalizeCodexRequestMetadata(body, headers, account, false)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "body-session", headers.Get("session-id"))
	require.Equal(t, "body-thread", headers.Get("thread-id"))
	require.Equal(t, "body-thread", headers.Get("x-client-request-id"))
	metadata := gjson.GetBytes(next, "client_metadata.x-codex-turn-metadata").String()
	require.Equal(t, metadata, headers.Get(openAIWSTurnMetadataHeader))
	require.Equal(t, "9007199254740993", gjson.Get(metadata, "turn_started_at_unix_ms").Raw)
	require.Equal(t, "same-turn", gjson.Get(metadata, "turn_id").String())
	require.Equal(t, "compaction", gjson.Get(metadata, "request_kind").String())
	require.Equal(t, "seatbelt", gjson.Get(metadata, "sandbox").String())
	require.JSONEq(t, `{"trigger":"manual","reason":"user_requested"}`, gjson.Get(metadata, "compaction").Raw)
	require.True(t, HasCompactionTriggerInInput(next))
	repeated, changed, err := normalizeCodexRequestMetadata(next, headers, account, false)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, next, repeated)
	public, changed, err := normalizeCodexRequestMetadata(body, nil, &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, false)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, public)
}

func TestCodexLegacyCompactDoesNotAcquireResponsesMetadataBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		body := []byte(`{"model":"gpt-5.2","input":[{"role":"user","content":"compress this history"}]}`)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(string(body)))
		c.Request.Header.Set("session-id", "caller-session")
		c.Request.Header.Set("thread-id", "caller-thread")
		c.Request.Header.Set(openAIWSTurnMetadataHeader, `{"session_id":"caller-session","thread_id":"caller-thread","request_kind":"turn","compaction":{"trigger":"manual"}}`)
		account := &Account{ID: 19, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "compact-owner"}}
		svc := &OpenAIGatewayService{}
		var request *http.Request
		var err error
		if passthrough {
			request, err = svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "test-token")
		} else {
			request, err = svc.buildUpstreamRequest(context.Background(), c, account, body, "test-token", false, "", true)
		}
		require.NoError(t, err)
		wireBody, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		require.NoError(t, request.Body.Close())
		require.Equal(t, body, wireBody)
		require.False(t, gjson.GetBytes(wireBody, "client_metadata").Exists())
		require.Equal(t, "compaction", gjson.Get(request.Header.Get(openAIWSTurnMetadataHeader), "request_kind").String())
		require.Empty(t, request.Header.Get("x-client-request-id"))
		replay, err := request.GetBody()
		require.NoError(t, err)
		replayed, err := io.ReadAll(replay)
		require.NoError(t, err)
		require.NoError(t, replay.Close())
		require.Equal(t, wireBody, replayed)
		require.Equal(t, int64(len(wireBody)), request.ContentLength)
	}
}

func TestCodexRequestMetadataFillsMissingNativeIdentity(t *testing.T) {
	account := &Account{ID: 41, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	body := []byte(`{"model":"gpt-5.5","stream":true,"input":[{"type":"message","role":"user","content":"hello"}]}`)
	headers := http.Header{}
	next, changed, err := normalizeCodexRequestMetadata(body, headers, account, false)
	require.NoError(t, err)
	require.True(t, changed)
	require.NotEmpty(t, gjson.GetBytes(next, "client_metadata.x-codex-installation-id").String())
	require.NotEmpty(t, gjson.GetBytes(next, "client_metadata.session_id").String())
	require.NotEmpty(t, gjson.GetBytes(next, "client_metadata.thread_id").String())
	metadata := gjson.GetBytes(next, "client_metadata.x-codex-turn-metadata").String()
	require.Equal(t, "turn", gjson.Get(metadata, "request_kind").String())
	require.Positive(t, gjson.Get(metadata, "turn_started_at_unix_ms").Int())
	require.NotEmpty(t, headers.Get("session-id"))
	require.NotEmpty(t, headers.Get("thread-id"))
	require.Equal(t, headers.Get("thread-id"), headers.Get("x-client-request-id"))
}

func TestPromoteOpenAILegacyCompactToNativeResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":"compress"}]}`))
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	body, err := PromoteOpenAILegacyCompactRequest(c, account, []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":"compress"}]}`))
	require.NoError(t, err)
	require.True(t, IsOpenAINativeCompactionV2(c))
	require.True(t, gjson.GetBytes(body, "stream").Bool())
	require.False(t, gjson.GetBytes(body, "store").Bool())
	require.Equal(t, "compaction_trigger", gjson.GetBytes(body, "input.1.type").String())
}
