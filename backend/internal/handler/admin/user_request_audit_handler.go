package admin

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

type UserRequestAuditHandler struct {
	service *service.UserRequestAuditService
}

func NewUserRequestAuditHandler(svc *service.UserRequestAuditService) *UserRequestAuditHandler {
	return &UserRequestAuditHandler{service: svc}
}

type userRequestAuditListItem struct {
	ID                 int64          `json:"id"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	ExpiresAt          time.Time      `json:"expires_at"`
	UserID             int64          `json:"user_id"`
	APIKeyID           int64          `json:"api_key_id"`
	GroupID            *int64         `json:"group_id,omitempty"`
	GroupName          string         `json:"group_name,omitempty"`
	Protocol           string         `json:"protocol"`
	Endpoint           string         `json:"endpoint"`
	RequestedModel     string         `json:"requested_model"`
	UpstreamModel      string         `json:"upstream_model,omitempty"`
	ClientRequestID    string         `json:"client_request_id,omitempty"`
	ResponseID         string         `json:"response_id,omitempty"`
	PreviousResponseID string         `json:"previous_response_id,omitempty"`
	FallbackHash       string         `json:"fallback_hash"`
	Status             string         `json:"status"`
	InputUsage         map[string]any `json:"input_usage,omitempty"`
	OutputUsage        map[string]any `json:"output_usage,omitempty"`
	CacheUsage         map[string]any `json:"cache_usage,omitempty"`
	LastError          string         `json:"last_error,omitempty"`
	Metadata           map[string]any `json:"metadata,omitempty"`
}

func (h *UserRequestAuditHandler) List(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	filter, err := parseUserRequestAuditFilter(c, page, pageSize)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	items, total, err := h.service.List(c.Request.Context(), filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	out := make([]userRequestAuditListItem, 0, len(items))
	for _, item := range items {
		out = append(out, userRequestAuditListItem{ID: item.ID, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, ExpiresAt: item.ExpiresAt, UserID: item.UserID, APIKeyID: item.APIKeyID, GroupID: item.GroupID, GroupName: item.GroupName, Protocol: item.Protocol, Endpoint: item.Endpoint, RequestedModel: item.RequestedModel, UpstreamModel: item.UpstreamModel, ClientRequestID: item.ClientRequestID, ResponseID: item.ResponseID, PreviousResponseID: item.PreviousResponseID, FallbackHash: item.FallbackHash, Status: item.Status, InputUsage: item.InputUsage, OutputUsage: item.OutputUsage, CacheUsage: item.CacheUsage, LastError: item.LastError, Metadata: item.Metadata})
	}
	response.Paginated(c, out, total, page, pageSize)
}

func (h *UserRequestAuditHandler) Get(c *gin.Context) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid user request audit id")
		return
	}
	item, err := h.service.GetByID(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		response.ErrorWithDetails(c, http.StatusNotFound, "User request audit not found", "USER_REQUEST_AUDIT_NOT_FOUND", nil)
		return
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func parseUserRequestAuditFilter(c *gin.Context, page, pageSize int) (service.UserRequestAuditFilter, error) {
	filter := service.UserRequestAuditFilter{Page: page, PageSize: pageSize, Protocol: strings.TrimSpace(c.Query("protocol")), RequestedModel: strings.TrimSpace(c.Query("requested_model")), ResponseID: strings.TrimSpace(c.Query("response_id")), ClientRequestID: strings.TrimSpace(c.Query("client_request_id")), Status: strings.TrimSpace(c.Query("status"))}
	parseID := func(name string) (*int64, error) {
		raw := strings.TrimSpace(c.Query(name))
		if raw == "" {
			return nil, nil
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			return nil, errors.New("Invalid " + name)
		}
		return &value, nil
	}
	var err error
	if filter.UserID, err = parseID("user_id"); err != nil {
		return filter, err
	}
	if filter.APIKeyID, err = parseID("api_key_id"); err != nil {
		return filter, err
	}
	if filter.GroupID, err = parseID("group_id"); err != nil {
		return filter, err
	}
	parseTime := func(name string) (*time.Time, error) {
		raw := strings.TrimSpace(c.Query(name))
		if raw == "" {
			return nil, nil
		}
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, errors.New("Invalid " + name + ", expect RFC3339")
		}
		return &value, nil
	}
	if filter.StartTime, err = parseTime("start_time"); err != nil {
		return filter, err
	}
	if filter.EndTime, err = parseTime("end_time"); err != nil {
		return filter, err
	}
	return filter, nil
}

type userRequestAuditConfigRequest struct {
	RetentionDays int `json:"retention_days" binding:"required"`
}

func (h *UserRequestAuditHandler) GetConfig(c *gin.Context) {
	days, err := h.service.GetRetentionDays(c.Request.Context())
	if err != nil && days <= 0 {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"retention_days": days})
}

func (h *UserRequestAuditHandler) UpdateConfig(c *gin.Context) {
	var req userRequestAuditConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "retention_days must be an integer between 1 and 365")
		return
	}
	if err := h.service.SetRetentionDays(c.Request.Context(), req.RetentionDays); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, gin.H{"retention_days": req.RetentionDays})
}
