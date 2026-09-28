//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWSOAuthRecovery_LocalHandshakeRefreshOnce(t *testing.T) {
	for _, rejectsReplacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "refresh_then_upgrade", true: "refresh_then_401"}[rejectsReplacement], func(t *testing.T) {
			account, repo, executor, provider := newOpenAIRecoveryFixture()
			var calls atomic.Int32
			seen := make(chan string, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				seen <- r.Header.Get("Authorization")
				if r.Header.Get("Authorization") != "Bearer repaired-access" || rejectsReplacement {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()
				kind, payload, err := conn.Read(ctx)
				if err != nil {
					return
				}
				_ = conn.Write(ctx, kind, payload)
				_, _, _ = conn.Read(ctx)
			}))
			defer server.Close()
			cfg := &config.Config{}
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			pool := newOpenAIWSConnPool(cfg)
			defer pool.Close()
			svc := &OpenAIGatewayService{accountRepo: repo, openAITokenProvider: provider}
			headers := make(http.Header)
			headers.Set("Authorization", "Bearer rejected-access")
			request := openAIWSAcquireRequest{Account: account, WSURL: "ws" + strings.TrimPrefix(server.URL, "http") + "/responses", Headers: headers, HeadersFactory: func(ctx context.Context, headers http.Header) (http.Header, error) {
				return svc.refreshOpenAIWSAuthHeaders(ctx, account, headers)
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			lease, err := svc.acquireOpenAIWSWithAuthRecovery(ctx, pool, &request)
			require.Equal(t, int32(2), calls.Load(), "expected original and repaired handshake: %v", err)
			require.Equal(t, "Bearer rejected-access", <-seen)
			require.Equal(t, "Bearer repaired-access", <-seen)
			require.Equal(t, "Bearer rejected-access", headers.Get("Authorization"), "original headers must remain immutable")
			require.Equal(t, "Bearer repaired-access", request.Headers.Get("Authorization"))
			require.Equal(t, int32(1), executor.calls.Load())
			if rejectsReplacement {
				var dialErr *openAIWSDialError
				require.ErrorAs(t, err, &dialErr)
				require.Equal(t, http.StatusUnauthorized, dialErr.StatusCode)
				require.Nil(t, lease)
				_, err = svc.acquireOpenAIWSWithAuthRecovery(ctx, pool, &request)
				require.Error(t, err)
				require.Equal(t, int32(2), calls.Load(), "rejected credential version must not redial")
				require.Equal(t, int32(1), executor.calls.Load())
				return
			}
			require.NoError(t, err)
			defer lease.Release()
			require.NoError(t, lease.WriteJSON(map[string]string{"type": "response.create", "input": "hello"}, time.Second))
			payload, err := lease.ReadMessage(time.Second)
			require.NoError(t, err)
			require.JSONEq(t, `{"type":"response.create","input":"hello"}`, string(payload))
			lease.MarkBroken()
			lease.Release()
			// A later dial with stale caller headers must use the durable replacement.
			fresh, err := svc.refreshOpenAIWSAuthHeaders(ctx, account, headers)
			require.NoError(t, err)
			require.Equal(t, "Bearer repaired-access", fresh.Get("Authorization"))
			require.Zero(t, repo.setError)
		})
	}
}

type openAIRecoveryLocalDialer struct {
	url string
}

func (d *openAIRecoveryLocalDialer) Dial(ctx context.Context, _ string, headers http.Header, proxy string) (openAIWSClientConn, int, http.Header, error) {
	return newDefaultOpenAIWSClientDialer().Dial(ctx, d.url, headers, proxy)
}

func TestOpenAIWSOAuthRecovery_PassthroughLocalHandshake(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, rejectsReplacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "refresh_then_stream", true: "refresh_then_401"}[rejectsReplacement], func(t *testing.T) {
			account, repo, executor, provider := newOpenAIRecoveryFixture()
			account.Extra = map[string]any{"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer repaired-access" || rejectsReplacement {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()
				if _, _, err := conn.Read(ctx); err != nil {
					return
				}
				_ = conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_recovered","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`))
				_, _, _ = conn.Read(ctx)
			}))
			defer upstream.Close()
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIWS.OAuthEnabled = true
			svc := newPassthroughLifecycleService(cfg, nil)
			svc.accountRepo = repo
			svc.openAITokenProvider = provider
			svc.openaiWSPassthroughDialer = &openAIRecoveryLocalDialer{url: "ws" + strings.TrimPrefix(upstream.URL, "http") + "/responses"}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			downstream, serverErr := startPassthroughLifecycleServer(t, ctx, svc, account)
			defer downstream.Close()
			client := dialPassthroughLifecycleClient(t, downstream)
			defer client.CloseNow()
			payload, err := readPassthroughLifecycleFrame(t, client, 4*time.Second)
			if rejectsReplacement {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Contains(t, string(payload), "resp_recovered")
			}
			_ = client.CloseNow()
			select {
			case <-serverErr:
			case <-ctx.Done():
				t.Fatal("passthrough auth recovery did not terminate")
			}
			require.Equal(t, int32(2), calls.Load())
			require.Equal(t, int32(1), executor.calls.Load())
			require.Zero(t, repo.setError)
			if rejectsReplacement {
				_, err := provider.RefreshAfterUnauthorized(context.Background(), account, "repaired-access")
				require.Error(t, err)
				require.Equal(t, int32(1), executor.calls.Load())
			}
		})
	}
}
