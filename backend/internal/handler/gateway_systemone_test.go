package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/repository"
	middleware2 "github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const validSystemOneHandlerBody = `{"model":"jev-latest","state":"sample","questions":{"q":{"type":"noul","instructions":"Evaluate"}}}`

func newSystemOneHandlerContext(body string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, recorder
}

func TestSystemOneRequiresAuthentication(t *testing.T) {
	c, recorder := newSystemOneHandlerContext(validSystemOneHandlerBody)
	(&GatewayHandler{}).SystemOne(c)
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	require.Contains(t, recorder.Body.String(), "authentication_error")
}

func TestSystemOneRejectsNonTypeSafeGroupBeforeScheduling(t *testing.T) {
	c, recorder := newSystemOneHandlerContext(validSystemOneHandlerBody)
	groupID := int64(3)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 4, UserID: 5, GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI}})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 5, Concurrency: 1})

	(&GatewayHandler{cfg: &config.Config{Gateway: config.GatewayConfig{MaxBodySize: 1 << 20}}}).SystemOne(c)
	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Contains(t, recorder.Body.String(), "only available for TypeSafe")
}

func newTypeSafeGroupContext(t *testing.T, path, body, groupPlatform string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, recorder := newSystemOneHandlerContext(body)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	groupID := int64(9)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 4, UserID: 5, GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: groupPlatform}})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 5, Concurrency: 1})
	return c, recorder
}

func TestRejectSystemOneOnlyPlatform(t *testing.T) {
	write := func(c *gin.Context, status int, errType, message string) {
		c.JSON(status, gin.H{"type": errType, "message": message})
	}

	c, recorder := newTypeSafeGroupContext(t, "/v1/messages", `{}`, service.PlatformTypeSafe)
	require.True(t, rejectSystemOneOnlyPlatform(c, mustAPIKey(t, c), write))
	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Contains(t, recorder.Body.String(), systemOneOnlyPlatformMessage)

	c, _ = newTypeSafeGroupContext(t, "/v1/messages", `{}`, service.PlatformComposite)
	require.False(t, rejectSystemOneOnlyPlatform(c, mustAPIKey(t, c), write))
	c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), service.PlatformTypeSafe))
	require.True(t, rejectSystemOneOnlyPlatform(c, mustAPIKey(t, c), write))

	c, _ = newTypeSafeGroupContext(t, "/v1/messages", `{}`, service.PlatformAnthropic)
	require.False(t, rejectSystemOneOnlyPlatform(c, mustAPIKey(t, c), write))

	c, _ = newTypeSafeGroupContext(t, "/antigravity/v1/messages", `{}`, service.PlatformTypeSafe)
	c.Set(string(middleware2.ContextKeyForcePlatform), service.PlatformAntigravity)
	require.False(t, rejectSystemOneOnlyPlatform(c, mustAPIKey(t, c), write))
}

func mustAPIKey(t *testing.T, c *gin.Context) *service.APIKey {
	t.Helper()
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	require.True(t, ok)
	return apiKey
}

func TestTypeSafeGroupsRejectNonSystemOneProtocolsBeforeScheduling(t *testing.T) {
	h := &GatewayHandler{
		cfg:            &config.Config{Gateway: config.GatewayConfig{MaxBodySize: 1 << 20}},
		gatewayService: &service.GatewayService{},
	}
	for _, tc := range []struct {
		name    string
		path    string
		body    string
		handler func(*gin.Context)
	}{
		{"messages", "/v1/messages", `{"model":"jev-latest","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, h.Messages},
		{"count_tokens", "/v1/messages/count_tokens", `{"model":"jev-latest","messages":[{"role":"user","content":"hi"}]}`, h.CountTokens},
		{"chat_completions", "/v1/chat/completions", `{"model":"jev-latest","messages":[{"role":"user","content":"hi"}]}`, h.ChatCompletions},
		{"responses", "/v1/responses", `{"model":"jev-latest","input":"hi"}`, h.Responses},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := newTypeSafeGroupContext(t, tc.path, tc.body, service.PlatformTypeSafe)
			tc.handler(c)
			require.Equal(t, http.StatusNotFound, recorder.Code, recorder.Body.String())
			require.Contains(t, recorder.Body.String(), systemOneOnlyPlatformMessage)
		})
	}
}

func TestSystemOneAuditNativeEvidenceAndGroupSelection(t *testing.T) {
	const requestBody = `{"model":"jev-latest","state":{"text":"keep  two\nlines","turn":9007199254740993,"items":[true,{"nested":"state"}]},"questions":{"pick":{"type":"choice","instructions":"Choose","criteria":{"a":"A","b":"B"}}},"instructions":"provider extension must not replace native state"}`
	const responseBody = "{\n\"model\":\"jev-1.13.0\",\n\"answers\":{\"pick\":{\"type\":\"choice\",\"choice\":\"b\"}},\n\"usage\":{\"input_tokens\":123,\"output_tokens\":\"7\",\"provider_units\":0.5},\n\"provider_extension\":{\"kept\":true}\n}"
	for _, tc := range []struct {
		name     string
		groupIDs string
		groupID  int64
		captured bool
	}{
		{name: "selected", groupIDs: "[3]", groupID: 3, captured: true},
		{name: "unselected", groupIDs: "[3]", groupID: 4},
		{name: "missing selection", groupID: 3},
		{name: "invalid selection", groupIDs: "[3,-1]", groupID: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATA_DIR", t.TempDir())
			archive := repository.NewFileUserRequestAuditRepository(nil)
			settings := &contentModerationHandlerSettingRepo{values: map[string]string{
				service.SettingKeyUserRequestAuditGroupIDs: tc.groupIDs,
			}}
			svc := service.NewUserRequestAuditService(archive, settings)
			t.Cleanup(svc.Stop)
			apiKey := &service.APIKey{ID: 4, UserID: 5, GroupID: &tc.groupID, Group: &service.Group{ID: tc.groupID, Name: "TypeSafe", Platform: service.PlatformTypeSafe}}
			router := gin.New()
			router.POST("/v1/systemone", func(c *gin.Context) {
				body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, &config.Config{Gateway: config.GatewayConfig{MaxBodySize: 1 << 20}})
				require.NoError(t, err)
				recorder := beginUserRequestAudit(c, svc, service.ContentModerationProtocolTypeSafeSystemOne, endpointForAudit(c, "/v1/systemone"), "jev-latest", body, apiKey, 5)
				defer func() { recorder.Finish(c.Writer.Status(), nil, nil) }()
				c.Data(http.StatusOK, "application/json", []byte(responseBody))
			})
			result := httptest.NewRecorder()
			router.ServeHTTP(result, httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(requestBody)))
			require.Equal(t, http.StatusOK, result.Code)
			require.Equal(t, responseBody, result.Body.String(), "auditing must not alter native wire bytes")
			svc.Stop() // Drain create/completion before inspecting the real archive.
			rows, total, err := svc.List(context.Background(), service.UserRequestAuditFilter{Protocol: service.ContentModerationProtocolTypeSafeSystemOne})
			require.NoError(t, err)
			if !tc.captured {
				require.Zero(t, total)
				require.Empty(t, rows)
				_, err := os.ReadDir(archive.UserRequestAuditRoot())
				require.True(t, os.IsNotExist(err), "disabled capture must not create archive files")
				return
			}
			require.EqualValues(t, 1, total)
			require.Len(t, rows, 1)
			record, err := svc.GetByID(context.Background(), rows[0].ID)
			require.NoError(t, err)
			require.Equal(t, "completed", record.Status)
			require.Equal(t, "/v1/systemone", record.Endpoint)
			require.Equal(t, "jev-latest", record.RequestedModel)
			require.Equal(t, "jev-1.13.0", record.UpstreamModel)
			require.JSONEq(t, requestBody, record.RequestChatML)
			require.Contains(t, record.RequestChatML, "9007199254740993", "native numbers must not lose precision")
			require.Equal(t, responseBody, record.ResponseChatML)
			require.NotContains(t, record.RequestChatML, "<|im_start|>")
			require.NotContains(t, record.ResponseChatML, "<|assistant|>")
			require.Equal(t, float64(123), record.InputUsage["input_tokens"])
			require.Equal(t, "7", record.OutputUsage["output_tokens"], "preserve upstream usage types")
			_, total, err = svc.List(context.Background(), service.UserRequestAuditFilter{Protocol: service.ContentModerationProtocolOpenAIResponses})
			require.NoError(t, err)
			require.Zero(t, total, "native records must not appear under a chat protocol")
			reopened := repository.NewFileUserRequestAuditRepository(nil)
			persisted, err := reopened.GetByID(context.Background(), record.ID)
			require.NoError(t, err)
			require.Equal(t, record.RequestChatML, persisted.RequestChatML)
			require.Equal(t, responseBody, persisted.ResponseChatML)
			require.Equal(t, record.InputUsage, persisted.InputUsage)
			require.Equal(t, record.OutputUsage, persisted.OutputUsage)
		})
	}
}

func TestSystemOneAuditCapturesModerationDenialBeforeForward(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	moderationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/moderations", r.URL.Path)
		_, _ = w.Write([]byte(`{"results":[{"category_scores":{"sexual":0.9}}]}`))
	}))
	defer moderationServer.Close()
	moderationConfig, err := json.Marshal(&service.ContentModerationConfig{
		Enabled: true, Mode: service.ContentModerationModePreBlock,
		BaseURL: moderationServer.URL, Model: "omni-moderation-latest",
		APIKeys: []string{"sk-test"}, SampleRate: 100, AllGroups: true, BlockMessage: "native request denied",
	})
	require.NoError(t, err)
	settings := &contentModerationHandlerSettingRepo{values: map[string]string{
		service.SettingKeyRiskControlEnabled:       "true",
		service.SettingKeyContentModerationConfig:  string(moderationConfig),
		service.SettingKeyUserRequestAuditGroupIDs: "[9]",
	}}
	archive := repository.NewFileUserRequestAuditRepository(nil)
	svc := service.NewUserRequestAuditService(archive, settings)
	t.Cleanup(svc.Stop)
	moderationSvc := service.NewContentModerationService(settings, &contentModerationHandlerTestRepo{}, nil, nil, nil, nil, nil, nil)
	h := &GatewayHandler{
		cfg:                      &config.Config{Gateway: config.GatewayConfig{MaxBodySize: 1 << 20}},
		contentModerationService: moderationSvc,
		userRequestAuditService:  svc,
	}
	c, result := newTypeSafeGroupContext(t, "/v1/systemone", validSystemOneHandlerBody, service.PlatformTypeSafe)
	h.SystemOne(c)
	require.GreaterOrEqual(t, result.Code, 400)
	require.Contains(t, result.Body.String(), "native request denied")
	svc.Stop()
	rows, total, err := svc.List(context.Background(), service.UserRequestAuditFilter{Protocol: service.ContentModerationProtocolTypeSafeSystemOne})
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, rows, 1)
	record, err := svc.GetByID(context.Background(), rows[0].ID)
	require.NoError(t, err)
	require.Equal(t, "failed", record.Status)
	require.JSONEq(t, validSystemOneHandlerBody, record.RequestChatML)
	require.Equal(t, result.Body.String(), record.ResponseChatML)
	require.Nil(t, record.InputUsage)
	require.Nil(t, record.OutputUsage)
}
