package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/MACOS-DO/sub4api/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newTestGinContext builds a bare gin.Context backed by an httptest recorder.
func newTestGinContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	return c
}

// TestRecordCyberPolicyIfMarked_NoMark verifies that when no cyber mark is set,
// the function returns immediately and does NOT set the recorded flag.
func TestRecordCyberPolicyIfMarked_NoMark(t *testing.T) {
	c := newTestGinContext()
	h := &OpenAIGatewayHandler{}

	h.recordCyberPolicyIfMarked(context.Background(), c, nil, nil, nil, "gpt-5", true, nil, service.ChannelUsageFields{}, "")

	// Flag must NOT be set when there was no mark.
	require.False(t, c.GetBool(cyberPolicyRecordedKey),
		"cyberPolicyRecordedKey must remain false when no cyber mark is present")
}

// TestRecordCyberPolicyIfMarked_WithMark verifies that:
//  1. When a cyber mark is present, the recorded flag is set (guard activated).
//  2. A second call is a no-op (idempotent guard).
//  3. Nil services do not panic.
func TestRecordCyberPolicyIfMarked_WithMark(t *testing.T) {
	c := newTestGinContext()
	service.MarkOpsCyberPolicy(c, service.CyberPolicyMark{
		Message:        "flagged",
		Body:           `{"error":{"code":"cyber_policy"}}`,
		UpstreamStatus: 400,
	})

	h := &OpenAIGatewayHandler{} // nil services — must not panic

	// First call: should set the flag.
	require.NotPanics(t, func() {
		h.recordCyberPolicyIfMarked(context.Background(), c, nil, nil, nil, "gpt-5", true, nil, service.ChannelUsageFields{}, "")
	})
	require.True(t, c.GetBool(cyberPolicyRecordedKey),
		"cyberPolicyRecordedKey must be true after first call with a mark")

	// Second call: flag already set — must be a no-op (idempotent).
	require.NotPanics(t, func() {
		h.recordCyberPolicyIfMarked(context.Background(), c, nil, nil, nil, "gpt-5", false, nil, service.ChannelUsageFields{}, "")
	})
	// Flag should still be true (not toggled or cleared).
	require.True(t, c.GetBool(cyberPolicyRecordedKey),
		"cyberPolicyRecordedKey must remain true after second call (guard)")
}

// TestClearCyberPolicyTurnState verifies F1 at the handler level: after a turn
// is finalized, both the mark and the recorded guard are reset so the next WS
// turn detects/records independently.
func TestClearCyberPolicyTurnState(t *testing.T) {
	c := newTestGinContext()
	h := &OpenAIGatewayHandler{}

	service.MarkOpsCyberPolicy(c, service.CyberPolicyMark{Message: "turn1", UpstreamStatus: 200})
	h.recordCyberPolicyIfMarked(context.Background(), c, nil, nil, nil, "gpt-5", false, nil, service.ChannelUsageFields{}, "")
	require.True(t, c.GetBool(cyberPolicyRecordedKey))

	clearCyberPolicyTurnState(c)
	require.Nil(t, service.GetOpsCyberPolicy(c))
	require.False(t, c.GetBool(cyberPolicyRecordedKey))

	// turn2: a fresh cyber hit must be recordable again.
	service.MarkOpsCyberPolicy(c, service.CyberPolicyMark{Message: "turn2", UpstreamStatus: 200})
	h.recordCyberPolicyIfMarked(context.Background(), c, nil, nil, nil, "gpt-5", false, nil, service.ChannelUsageFields{}, "")
	require.True(t, c.GetBool(cyberPolicyRecordedKey))
	require.Equal(t, "turn2", service.GetOpsCyberPolicy(c).Message)
}

func TestAdvanceOpenAIWSCyberBlockStateDefersBlockAcrossFailover(t *testing.T) {
	failoverErr := &service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests}

	blocked, pending := advanceOpenAIWSCyberBlockState(false, false, true, failoverErr)
	require.False(t, blocked, "the replacement account must receive the current turn")
	require.True(t, pending, "the cyber hit must still block later turns")

	blocked, pending = advanceOpenAIWSCyberBlockState(blocked, pending, false, failoverErr)
	require.False(t, blocked, "additional failover attempts must remain eligible")
	require.True(t, pending)

	blocked, pending = advanceOpenAIWSCyberBlockState(blocked, pending, false, nil)
	require.True(t, blocked, "the next client turn must be blocked after failover finishes")
	require.False(t, pending)
}

func TestClearCyberPolicyAttemptStatePreservesRecordedGuardDuringFailover(t *testing.T) {
	c := newTestGinContext()
	h := &OpenAIGatewayHandler{}

	service.MarkOpsCyberPolicy(c, service.CyberPolicyMark{Message: "account-a", UpstreamStatus: http.StatusOK})
	h.recordCyberPolicyIfMarked(context.Background(), c, nil, nil, nil, "gpt-5", true, nil, service.ChannelUsageFields{}, "")
	require.True(t, c.GetBool(cyberPolicyRecordedKey))

	clearCyberPolicyAttemptState(c, false)
	require.Nil(t, service.GetOpsCyberPolicy(c))
	require.True(t, c.GetBool(cyberPolicyRecordedKey), "the same logical turn must not record again after failover")

	service.MarkOpsCyberPolicy(c, service.CyberPolicyMark{Message: "account-b", UpstreamStatus: http.StatusOK})
	h.recordCyberPolicyIfMarked(context.Background(), c, nil, nil, nil, "gpt-5", true, nil, service.ChannelUsageFields{}, "")
	require.Equal(t, "account-b", service.GetOpsCyberPolicy(c).Message)
	require.True(t, c.GetBool(cyberPolicyRecordedKey))

	clearCyberPolicyAttemptState(c, true)
	require.Nil(t, service.GetOpsCyberPolicy(c))
	require.False(t, c.GetBool(cyberPolicyRecordedKey), "a completed logical turn must reset the guard")
}

// TestBuildCyberSessionBlockedOpsEntry verifies the locally-rejected request is
// auditable: 403 / phase=request / type=cyber_policy_session_blocked — distinct
// from upstream cyber_policy hits, and it must NOT touch moderation/violation.
func TestBuildCyberSessionBlockedOpsEntry(t *testing.T) {
	entry := buildCyberSessionBlockedOpsEntry(cyberPolicyOpsErrorMeta{
		RequestID: "req-9", Model: "gpt-5", RequestPath: "/openai/v1/responses",
	})
	require.Equal(t, 403, entry.StatusCode)
	require.Equal(t, "cyber_policy_session_blocked", entry.ErrorType)
	require.Equal(t, "request", entry.ErrorPhase)
	require.True(t, entry.IsBusinessLimited)
	require.Equal(t, "gateway_local", entry.ErrorSource)
	require.Equal(t, "platform", entry.ErrorOwner)
	require.Empty(t, entry.ErrorBody, "no session block key → ErrorBody must be empty")

	entryWithKey := buildCyberSessionBlockedOpsEntry(cyberPolicyOpsErrorMeta{
		RequestID: "req-9", Model: "gpt-5", RequestPath: "/openai/v1/responses",
		SessionBlockKey: "abc123",
	})
	require.Equal(t, "session_block_key=abc123", entryWithKey.ErrorBody)
}

// TestRejectIfCyberSessionBlocked_FailOpen verifies fail-open paths: nil handler
// services, no explicit session signal, and (implicitly) disabled switch all
// pass the request through.
func TestRejectIfCyberSessionBlocked_FailOpen(t *testing.T) {
	c := newTestGinContext()
	c.Request = httptest.NewRequest("POST", "/openai/v1/responses", strings.NewReader(`{}`))

	h := &OpenAIGatewayHandler{}
	require.False(t, h.rejectIfCyberSessionBlocked(c, nil, []byte(`{}`), "gpt-5", cyberBlockFormatResponses), "nil apiKey → pass")

	h2 := &OpenAIGatewayHandler{gatewayService: nil}
	key := &service.APIKey{ID: 1}
	require.False(t, h2.rejectIfCyberSessionBlocked(c, key, []byte(`{}`), "gpt-5", cyberBlockFormatResponses), "nil gateway service → pass")
}

func TestBuildCyberSessionBlockWritePlanCombinesExplicitAndTranscriptKeys(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"trigger"}]}`)
	c := newTestGinContext()
	c.Request = httptest.NewRequest("POST", "/openai/v1/responses", strings.NewReader(string(body)))
	c.Request.RemoteAddr = "203.0.113.44:12345"
	c.Request.Header.Set("User-Agent", "client/1.2.3")

	plan := buildCyberSessionBlockWritePlan(7, c, body)
	require.Len(t, plan.keys, 2)
	require.NotEmpty(t, plan.scopeKey)

	c.Request.Header.Set("session_id", "sess-explicit")
	plan = buildCyberSessionBlockWritePlan(7, c, body)
	require.Len(t, plan.keys, 3)
	require.NotEmpty(t, plan.scopeKey)
}

// TestBuildCyberPolicyOpsErrorEntry_StatusCode verifies F6: the ops error log
// records the status the codex client actually received (400 non-stream / 200 stream),
// not a hardcoded 403.
func TestBuildCyberPolicyOpsErrorEntry_StatusCode(t *testing.T) {
	for _, tc := range []struct {
		name           string
		upstreamStatus int
	}{
		{"non_stream_400", 400},
		{"stream_200", 200},
		{"zero_value", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mark := &service.CyberPolicyMark{
				Code:           "cyber_policy",
				Message:        "blocked",
				UpstreamStatus: tc.upstreamStatus,
			}
			entry := buildCyberPolicyOpsErrorEntry(cyberPolicyOpsErrorMeta{
				RequestID: "req-1", Model: "gpt-5", RequestPath: "/openai/v1/responses",
			}, mark)
			require.Equal(t, tc.upstreamStatus, entry.StatusCode)
			require.Equal(t, "cyber_policy", entry.ErrorType)
			require.Equal(t, "request", entry.ErrorPhase)
		})
	}
}

type cyberInflightBillingCache struct {
	*handlerInflightCache
	released chan struct{}
}

func (s *cyberInflightBillingCache) DeductUserBalance(_ context.Context, _ int64, amount float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.balance -= amount
	return nil
}

func (s *cyberInflightBillingCache) ReleaseInflightBalance(ctx context.Context, userID int64, id string) error {
	err := s.handlerInflightCache.ReleaseInflightBalance(ctx, userID, id)
	s.released <- struct{}{}
	return err
}

type cyberInflightBillingRepo struct {
	service.UsageBillingRepository
	started     chan *service.UsageBillingCommand
	allowCommit chan struct{}
	commitOnce  sync.Once
}

func (s *cyberInflightBillingRepo) Apply(ctx context.Context, cmd *service.UsageBillingCommand) (*service.UsageBillingApplyResult, error) {
	s.started <- cmd
	select {
	case <-s.allowCommit:
		return &service.UsageBillingApplyResult{Applied: true}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *cyberInflightBillingRepo) commit() {
	s.commitOnce.Do(func() { close(s.allowCommit) })
}

type cyberInflightHandlerFixture struct {
	handler        *OpenAIGatewayHandler
	account        *service.Account
	apiKey         *service.APIKey
	cache          *cyberInflightBillingCache
	billingRepo    *cyberInflightBillingRepo
	usageRepo      *openAIWSUsageHandlerUsageLogRepoStub
	moderationRepo *contentModerationHandlerTestRepo
}

func newCyberInflightHandlerFixture(t *testing.T, upstreamURL string, upstream service.HTTPUpstream) *cyberInflightHandlerFixture {
	t.Helper()
	groupID := int64(4309)
	account := service.Account{
		ID: 9959, Name: "cyber-inflight", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive,
		Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID},
		Credentials: map[string]any{"api_key": "sk-test", "base_url": upstreamURL},
		Extra: map[string]any{
			"openai_passthrough":                            true,
			"openai_apikey_responses_websockets_v2_enabled": true,
			"openai_apikey_responses_websockets_v2_mode":    service.OpenAIWSIngressModeCtxPool,
		},
	}
	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Billing.InflightReservation = config.InflightReservationConfig{Enabled: true, TTLSeconds: 60}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 3
	cache := &cyberInflightBillingCache{handlerInflightCache: newHandlerInflightCache(1), released: make(chan struct{}, 4)}
	billingCache := service.NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	billingRepo := &cyberInflightBillingRepo{
		started: make(chan *service.UsageBillingCommand, 2), allowCommit: make(chan struct{}),
	}
	t.Cleanup(billingRepo.commit)
	usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 2)}
	moderationRepo := &contentModerationHandlerTestRepo{}
	settings := &contentModerationHandlerSettingRepo{values: map[string]string{
		service.SettingKeyRiskControlEnabled: "true",
	}}
	moderation := service.NewContentModerationService(settings, moderationRepo, nil, nil, nil, nil, nil, nil)
	concurrency := service.NewConcurrencyService(&concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	})
	gateway := service.NewOpenAIGatewayService(
		&openAIWSUsageHandlerAccountRepoStub{account: account}, usageRepo, billingRepo,
		nil, nil, nil, testutil.NewRedisGatewayCache(t), cfg, nil, concurrency,
		service.NewBillingService(cfg, nil), nil, billingCache, upstream, &service.DeferredService{},
		nil, nil, nil, nil, nil, service.NewSettingService(settings, nil), nil,
	)
	t.Cleanup(gateway.CloseOpenAIWSPool)
	apiKey := &service.APIKey{
		ID: 1859, UserID: 1759, Key: "sk-cyber-inflight-test", GroupID: &groupID,
		User:  &service.User{ID: 1759, Status: service.StatusActive, Balance: 1},
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, RateMultiplier: 1},
	}
	return &cyberInflightHandlerFixture{
		handler: NewOpenAIGatewayHandler(gateway, concurrency, billingCache, &service.APIKeyService{}, nil, nil, moderation, nil, cfg),
		account: &account, apiKey: apiKey, cache: cache, billingRepo: billingRepo,
		usageRepo: usageRepo, moderationRepo: moderationRepo,
	}
}

func (f *cyberInflightHandlerFixture) assertBillingStarted(t *testing.T) {
	t.Helper()
	select {
	case cmd := <-f.billingRepo.started:
		require.Equal(t, 11, cmd.InputTokens)
		require.Equal(t, 3, cmd.OutputTokens)
		require.Greater(t, cmd.BalanceCost, 0.0, "real cyber tokens must be charged")
	case <-time.After(3 * time.Second):
		t.Fatal("cyber billing did not reach the blocked commit")
	}
}

func (f *cyberInflightHandlerFixture) assertBillingHeldAfterHandlerReturns(t *testing.T) {
	t.Helper()
	require.Equal(t, 1, f.cache.count(), "handler return must not release pending cyber billing")
	competing, err := f.handler.billingCacheService.ReserveInflight(context.Background(), f.apiKey.User, f.apiKey.Group, nil, 1)
	if competing != nil {
		competing.HandlerDone()
	}
	require.ErrorIs(t, err, service.ErrInsufficientBalance, "another request cannot spend the held balance")
	require.Len(t, f.moderationRepo.logSnapshot(), 1, "the cyber event must still be logged")
}

func (f *cyberInflightHandlerFixture) assertBillingCommitReleasesHold(t *testing.T) {
	t.Helper()
	f.billingRepo.commit()
	select {
	case log := <-f.usageRepo.created:
		require.Equal(t, service.RequestTypeCyberBlocked, log.RequestType)
		require.Equal(t, 11, log.InputTokens)
		require.Equal(t, 3, log.OutputTokens)
		require.Greater(t, log.ActualCost, 0.0)
	case <-time.After(3 * time.Second):
		t.Fatal("cyber usage was not recorded after the billing commit")
	}
	select {
	case <-f.cache.released:
	case <-time.After(3 * time.Second):
		t.Fatal("cyber billing did not release the reservation after commit")
	}
	require.Zero(t, f.cache.count())
	balance, err := f.cache.GetUserBalance(context.Background(), f.apiKey.User.ID)
	require.NoError(t, err)
	require.Less(t, balance, 1.0, "cache balance must reflect committed cyber billing")
	competing, err := f.handler.billingCacheService.ReserveInflight(context.Background(), f.apiKey.User, f.apiKey.Group, nil, 0.01)
	require.NoError(t, err)
	require.NotNil(t, competing)
	competing.HandlerDone()
}

type cyberInflightHTTPUpstream struct {
	service.HTTPUpstream
}

func (s *cyberInflightHTTPUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	body := "event: response.failed\n" +
		`data: {"type":"response.failed","response":{"id":"resp_cyber_inflight","model":"gpt-5.1","status":"failed","error":{"code":"cyber_policy","message":"blocked by upstream policy"},"usage":{"input_tokens":11,"output_tokens":3}}}` + "\n\n"
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(body)),
	}, nil
}

func TestOpenAIResponses_CyberBillingHoldsInflightAfterHandlerReturns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f := newCyberInflightHandlerFixture(t, "https://api.example.test", &cyberInflightHTTPUpstream{})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{"model":"gpt-5.1","input":"test","stream":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Header("X-Request-Id", "req-http-cyber-inflight")
	c.Set(string(middleware.ContextKeyAPIKey), f.apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: f.apiKey.User.ID, Concurrency: 1})

	handlerDone := make(chan struct{})
	go func() {
		f.handler.Responses(c)
		close(handlerDone)
	}()
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP handler did not return while cyber billing was blocked")
	}

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"cyber_policy"`)
	f.assertBillingStarted(t)
	f.assertBillingHeldAfterHandlerReturns(t)
	// A retry/failover observation of the same request must not enqueue a
	// second cyber charge or retain an extra reference.
	f.handler.recordCyberPolicyIfMarked(c.Request.Context(), c, f.apiKey, f.account, nil, "gpt-5.1", true, nil, service.ChannelUsageFields{}, "")
	f.assertBillingCommitReleasesHold(t)
	select {
	case <-f.billingRepo.started:
		t.Fatal("the same cyber request was billed twice")
	default:
	}
}

type cyberInflightModerationRepo struct {
	*contentModerationHandlerTestRepo
	started chan struct{}
	proceed chan struct{}
	written chan struct{}
}

func (s *cyberInflightModerationRepo) CreateLog(ctx context.Context, log *service.ContentModerationLog) error {
	close(s.started)
	select {
	case <-s.proceed:
	case <-ctx.Done():
		return ctx.Err()
	}
	err := s.contentModerationHandlerTestRepo.CreateLog(ctx, log)
	close(s.written)
	return err
}

func TestRecordCyberPolicyIfMarked_ForwardSuccessLogsWithoutBillingHold(t *testing.T) {
	f := newCyberInflightHandlerFixture(t, "https://api.example.test", nil)
	repo := &cyberInflightModerationRepo{
		contentModerationHandlerTestRepo: f.moderationRepo,
		started:                          make(chan struct{}), proceed: make(chan struct{}), written: make(chan struct{}),
	}
	var proceedOnce sync.Once
	proceed := func() { proceedOnce.Do(func() { close(repo.proceed) }) }
	t.Cleanup(proceed)
	f.handler.contentModerationService = service.NewContentModerationService(
		&contentModerationHandlerSettingRepo{values: map[string]string{service.SettingKeyRiskControlEnabled: "true"}},
		repo, nil, nil, nil, nil, nil, nil,
	)
	c := newInflightTestGinContext()
	done, err := reserveInflightBalance(c, f.handler.billingCacheService, &countingEstimator{cost: 0.9, priced: true}, f.apiKey, nil, tokenInflightEstimate("gpt-5.1", nil))
	require.NoError(t, err)
	t.Cleanup(done)
	service.MarkOpsCyberPolicy(c, service.CyberPolicyMark{Message: "logged only", UpstreamStatus: http.StatusOK})
	f.handler.recordCyberPolicyIfMarked(c.Request.Context(), c, f.apiKey, f.account, nil, "gpt-5.1", false, nil, service.ChannelUsageFields{}, "")
	select {
	case <-repo.started:
	case <-time.After(3 * time.Second):
		t.Fatal("cyber event logging did not start")
	}
	done()
	require.Zero(t, f.cache.count(), "event-only logging must not retain a billing reservation")
	proceed()
	select {
	case <-repo.written:
	case <-time.After(3 * time.Second):
		t.Fatal("cyber event logging did not finish")
	}
	logs := f.moderationRepo.logSnapshot()
	require.Len(t, logs, 1)
	require.Equal(t, service.ContentModerationActionCyberPolicy, logs[0].Action)
	select {
	case <-f.billingRepo.started:
		t.Fatal("a successful forward must not enqueue duplicate cyber billing")
	default:
	}
}
