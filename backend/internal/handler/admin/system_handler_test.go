//go:build unit

package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newSystemHandlerTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	handler := NewSystemHandler("custom-1.1.6")
	router := gin.New()
	router.GET("/api/v1/admin/system/version", handler.GetVersion)
	router.GET("/api/v1/admin/system/check-updates", handler.SelfUpdateDisabled)
	router.GET("/api/v1/admin/system/rollback-versions", handler.SelfUpdateDisabled)
	router.POST("/api/v1/admin/system/update", handler.SelfUpdateDisabled)
	router.POST("/api/v1/admin/system/rollback", handler.SelfUpdateDisabled)
	router.POST("/api/v1/admin/system/restart", handler.SelfUpdateDisabled)
	return router
}

func TestSystemHandlerGetVersionUsesInjectedLocalVersion(t *testing.T) {
	router := newSystemHandlerTestRouter(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/system/version", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Code int `json:"code"`
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, 0, body.Code)
	require.Equal(t, "custom-1.1.6", body.Data.Version)
}

func TestSystemHandlerRetiredEndpointsAreDisabledWithoutParsingOrCaching(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "check updates", method: http.MethodGet, path: "/api/v1/admin/system/check-updates?force=true"},
		{name: "rollback versions", method: http.MethodGet, path: "/api/v1/admin/system/rollback-versions"},
		{name: "update", method: http.MethodPost, path: "/api/v1/admin/system/update"},
		{name: "rollback", method: http.MethodPost, path: "/api/v1/admin/system/rollback"},
		{name: "restart", method: http.MethodPost, path: "/api/v1/admin/system/restart"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := newSystemHandlerTestRouter(t)
			for range 2 {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(tt.method, tt.path, strings.NewReader("not-json and arbitrary payload"))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Idempotency-Key", "retired-endpoint-key")
				router.ServeHTTP(rec, req)

				require.Equal(t, http.StatusGone, rec.Code)
				var body struct {
					Code   int    `json:"code"`
					Reason string `json:"reason"`
				}
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				require.Equal(t, http.StatusGone, body.Code)
				require.Equal(t, "SELF_UPDATE_DISABLED", body.Reason)
			}
		})
	}
}
