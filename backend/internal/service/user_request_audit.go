package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	UserRequestAuditDefaultRetentionDays = 7
	userRequestAuditMaxChatML            = 32 * 1024
	userRequestAuditQueueSize            = 2048
	userRequestAuditShutdownDrainLimit   = 256
	userRequestAuditCleanupBatch         = 500
	userRequestAuditCleanupPasses        = 16
)

var userRequestAuditProtocols = map[string]struct{}{
	"anthropic_messages": {}, "openai_responses": {},
	"openai_chat_completions": {}, "openai_responses_ws": {},
}

type UserRequestAudit struct {
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
	ConversationKey    string         `json:"conversation_key,omitempty"`
	RequestChatML      string         `json:"request_chatml,omitempty"`
	ResponseChatML     string         `json:"response_chatml,omitempty"`
	ChatML             string         `json:"chatml,omitempty"`
	Status             string         `json:"status"`
	InputUsage         map[string]any `json:"input_usage,omitempty"`
	OutputUsage        map[string]any `json:"output_usage,omitempty"`
	CacheUsage         map[string]any `json:"cache_usage,omitempty"`
	LastError          string         `json:"last_error,omitempty"`
	Metadata           map[string]any `json:"metadata,omitempty"`
	LogicalKey         string         `json:"-"`
}

type UserRequestAuditCompletion struct {
	LogicalKey     string
	ResponseID     string
	UpstreamModel  string
	ResponseChatML string
	Status         string
	InputUsage     map[string]any
	OutputUsage    map[string]any
	CacheUsage     map[string]any
	LastError      string
	Metadata       map[string]any
}

type UserRequestAuditFilter struct {
	Page            int
	PageSize        int
	UserID          *int64
	APIKeyID        *int64
	GroupID         *int64
	Protocol        string
	RequestedModel  string
	ResponseID      string
	ClientRequestID string
	Status          string
	StartTime       *time.Time
	EndTime         *time.Time
}

type UserRequestAuditRepository interface {
	Create(context.Context, *UserRequestAudit) error
	Complete(context.Context, *UserRequestAuditCompletion) error
	List(context.Context, UserRequestAuditFilter) ([]*UserRequestAudit, int64, error)
	GetByID(context.Context, int64) (*UserRequestAudit, error)
	DeleteExpired(context.Context, time.Time, int) (int64, error)
}

type UserRequestAuditCapture struct {
	UserID             int64
	APIKeyID           int64
	GroupID            *int64
	GroupName          string
	Protocol           string
	Endpoint           string
	RequestedModel     string
	ClientRequestID    string
	ResponseID         string
	PreviousResponseID string
	Body               []byte
	Metadata           map[string]any
}

type UserRequestAuditService struct {
	repo      UserRequestAuditRepository
	settings  SettingRepository
	queue     chan func(context.Context)
	stop      chan struct{}
	done      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

func NewUserRequestAuditService(repo UserRequestAuditRepository, settings SettingRepository) *UserRequestAuditService {
	return &UserRequestAuditService{repo: repo, settings: settings, queue: make(chan func(context.Context), userRequestAuditQueueSize)}
}

func ProvideUserRequestAuditService(repo UserRequestAuditRepository, settings SettingRepository) *UserRequestAuditService {
	s := NewUserRequestAuditService(repo, settings)
	s.Start()
	return s
}

func (s *UserRequestAuditService) Start() {
	if s == nil || s.repo == nil {
		return
	}
	s.startOnce.Do(func() {
		s.stop = make(chan struct{})
		s.done = make(chan struct{})
		go s.run()
	})
}

func (s *UserRequestAuditService) Stop() {
	if s == nil || s.done == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stop)
		<-s.done
	})
}

func (s *UserRequestAuditService) run() {
	defer close(s.done)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case task := <-s.queue:
			s.runTask(task, 5*time.Second)
		case <-ticker.C:
			if deleted, err := s.cleanup(context.Background()); err != nil {
				slog.Warn("user_request_audit.cleanup_failed", "error", err)
			} else if deleted > 0 {
				slog.Info("user_request_audit.cleanup_completed", "deleted", deleted)
			}
		case <-s.stop:
			// Drain a bounded amount on shutdown. The queue is deliberately not
			// closed: an ingress goroutine racing Stop must never panic.
			deadline := time.Now().Add(10 * time.Second)
			for drained := 0; drained < userRequestAuditShutdownDrainLimit && time.Now().Before(deadline); drained++ {
				select {
				case task := <-s.queue:
					remaining := time.Until(deadline)
					if remaining <= 0 {
						return
					}
					s.runTask(task, minDuration(5*time.Second, remaining))
				default:
					return
				}
			}
			return
		}
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (s *UserRequestAuditService) runTask(task func(context.Context), timeout time.Duration) {
	if task == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	task(ctx)
}

func (s *UserRequestAuditService) enqueue(task func(context.Context)) {
	if s == nil || s.repo == nil || task == nil {
		return
	}
	if s.done == nil {
		s.Start()
	}
	select {
	case s.queue <- task:
	default:
		slog.Warn("user_request_audit.queue_dropped")
	}
}

func (s *UserRequestAuditService) Enqueue(capture UserRequestAuditCapture) string {
	if _, ok := userRequestAuditProtocols[capture.Protocol]; !ok {
		return ""
	}
	fallback := UserRequestAuditFallbackHash(capture.UserID, capture.GroupID, capture.Body)
	audit := &UserRequestAudit{
		CreatedAt: time.Now().UTC(),
		UserID:    capture.UserID, APIKeyID: capture.APIKeyID, GroupID: cloneAuditGroupID(capture.GroupID),
		GroupName: bound(capture.GroupName, 256), Protocol: capture.Protocol, Endpoint: bound(capture.Endpoint, 512),
		RequestedModel: bound(capture.RequestedModel, 256), ClientRequestID: bound(capture.ClientRequestID, 256),
		ResponseID: bound(capture.ResponseID, 256), PreviousResponseID: bound(capture.PreviousResponseID, 256),
		FallbackHash: fallback, RequestChatML: ProjectUserRequestChatML(capture.Body), Status: "received",
		Metadata: boundedMap(capture.Metadata), LogicalKey: userRequestAuditLogicalKey(capture, fallback),
	}
	s.enqueue(func(ctx context.Context) {
		retention := s.retentionDays()
		audit.ExpiresAt = audit.CreatedAt.AddDate(0, 0, retention)
		if err := s.repo.Create(ctx, audit); err != nil {
			slog.Warn("user_request_audit.create_failed", "error", err, "protocol", audit.Protocol)
		}
	})
	return audit.LogicalKey
}

func (s *UserRequestAuditService) Complete(completion UserRequestAuditCompletion) {
	if strings.TrimSpace(completion.LogicalKey) == "" {
		return
	}
	completion.ResponseChatML = bound(completion.ResponseChatML, userRequestAuditMaxChatML)
	completion.UpstreamModel = bound(completion.UpstreamModel, 256)
	completion.ResponseID = bound(completion.ResponseID, 256)
	completion.Status = bound(completion.Status, 32)
	completion.LastError = bound(completion.LastError, 1024)
	completion.Metadata = boundedMap(completion.Metadata)
	s.enqueue(func(ctx context.Context) {
		if err := s.repo.Complete(ctx, &completion); err != nil {
			slog.Warn("user_request_audit.complete_failed", "error", err)
		}
	})
}

func (s *UserRequestAuditService) List(ctx context.Context, filter UserRequestAuditFilter) ([]*UserRequestAudit, int64, error) {
	if s == nil || s.repo == nil {
		return nil, 0, nil
	}
	return s.repo.List(ctx, filter)
}

func (s *UserRequestAuditService) GetByID(ctx context.Context, id int64) (*UserRequestAudit, error) {
	if s == nil || s.repo == nil {
		return nil, sql.ErrNoRows
	}
	return s.repo.GetByID(ctx, id)
}

func (s *UserRequestAuditService) GetRetentionDays(ctx context.Context) (int, error) {
	if s == nil || s.settings == nil {
		return UserRequestAuditDefaultRetentionDays, nil
	}
	raw, err := s.settings.GetValue(ctx, SettingKeyUserRequestAuditRetentionDays)
	if err != nil {
		return UserRequestAuditDefaultRetentionDays, err
	}
	var days int
	if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%d", &days); err != nil || days < 1 || days > 365 {
		return UserRequestAuditDefaultRetentionDays, nil
	}
	return days, nil
}

func (s *UserRequestAuditService) SetRetentionDays(ctx context.Context, days int) error {
	if days < 1 || days > 365 {
		return fmt.Errorf("retention days must be between 1 and 365")
	}
	if s == nil || s.settings == nil {
		return fmt.Errorf("user request audit settings unavailable")
	}
	return s.settings.Set(ctx, SettingKeyUserRequestAuditRetentionDays, fmt.Sprintf("%d", days))
}

func (s *UserRequestAuditService) retentionDays() int {
	retention, err := s.GetRetentionDays(context.Background())
	if err != nil {
		return UserRequestAuditDefaultRetentionDays
	}
	return retention
}
func (s *UserRequestAuditService) cleanup(ctx context.Context) (int64, error) {
	if s == nil || s.repo == nil {
		return 0, nil
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var total int64
	for i := 0; i < userRequestAuditCleanupPasses; i++ {
		deleted, err := s.repo.DeleteExpired(cleanupCtx, time.Now().UTC(), userRequestAuditCleanupBatch)
		total += deleted
		if err != nil {
			return total, err
		}
		if deleted < userRequestAuditCleanupBatch {
			break
		}
	}
	return total, nil
}

func cloneAuditGroupID(groupID *int64) *int64 {
	if groupID == nil {
		return nil
	}
	v := *groupID
	return &v
}

func UserRequestAuditFallbackHash(userID int64, groupID *int64, body []byte) string {
	content := UserRequestAuditNormalizeContent(extractAuditContent(body))
	group := "none"
	if groupID != nil {
		group = fmt.Sprint(*groupID)
	}
	if content == "" {
		content = "empty:" + uuid.NewString()
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%s", userID, group, content)))
	return hex.EncodeToString(h[:])
}

// UserRequestAuditNormalizeContent is used only for association, never for saved ChatML.
func UserRequestAuditNormalizeContent(raw string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(raw)), " ")
}

func ProjectUserRequestChatML(body []byte) string {
	if len(body) == 0 {
		return "[empty request body]"
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return "[unparsed request redacted]"
	}
	var out strings.Builder
	emit := func(role, content string) {
		role = bound(role, 32)
		if role == "" || content == "" {
			return
		}
		fmt.Fprintf(&out, "<|im_start|>%s\n%s<|im_end|>\n", role, safeAuditContent(content))
	}
	emitValue := func(role string, value any) {
		if value == nil {
			return
		}
		if text, ok := value.(string); ok {
			emit(role, text)
			return
		}
		encoded, _ := json.Marshal(userRequestAuditRedactValue(value))
		emit(role, string(encoded))
	}
	if system, ok := doc["system"]; ok {
		emitValue("system", system)
	}
	if instructions, ok := doc["instructions"]; ok {
		emitValue("system", instructions)
	}
	if reasoning, ok := doc["reasoning"]; ok {
		emitValue("reasoning", reasoning)
	}
	if tools, ok := doc["tools"]; ok {
		emitValue("tools", tools)
	}
	if messages, ok := doc["messages"].([]any); ok {
		for _, raw := range messages {
			message, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role := userRequestAuditStringValue(message["role"])
			if role == "" {
				role = "user"
			}
			if content, simple := message["content"].(string); simple && len(message) == 2 {
				emit(role, content)
			} else {
				emitValue(role, message)
			}
		}
	}
	if input, ok := doc["input"]; ok {
		switch values := input.(type) {
		case string:
			emit("user", values)
		case []any:
			for _, value := range values {
				if message, ok := value.(map[string]any); ok {
					role := userRequestAuditStringValue(message["role"])
					if role == "" {
						role = responsesItemRole(message)
					}
					emitValue(role, message)
				} else {
					emitValue("user", value)
				}
			}
		default:
			emitValue("user", input)
		}
	}
	if out.Len() == 0 {
		encoded, _ := json.Marshal(userRequestAuditRedactValue(doc))
		return bound(string(encoded), userRequestAuditMaxChatML)
	}
	return bound(out.String(), userRequestAuditMaxChatML)
}

func responsesItemRole(message map[string]any) string {
	switch strings.ToLower(userRequestAuditStringValue(message["type"])) {
	case "function_call_output", "tool":
		return "tool"
	case "function_call":
		return "assistant"
	default:
		return "user"
	}
}

func extractAuditContent(body []byte) string {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	if system, ok := doc["system"]; ok {
		if content := auditAssociationValue(system); content != "" {
			return content
		}
	}
	if instructions, ok := doc["instructions"]; ok {
		if content := auditAssociationValue(instructions); content != "" {
			return content
		}
	}
	if messages, ok := doc["messages"].([]any); ok {
		for _, raw := range messages {
			if message, ok := raw.(map[string]any); ok {
				role := strings.ToLower(strings.TrimSpace(userRequestAuditStringValue(message["role"])))
				if role == "system" || role == "user" {
					if content := auditAssociationValue(message["content"]); content != "" {
						return content
					}
				}
			}
		}
	}
	if input, ok := doc["input"]; ok {
		if content := extractAuditInputContent(input); content != "" {
			return content
		}
	}
	return userRequestAuditStringValue(doc["prompt"])
}

func auditAssociationValue(value any) string {
	if text := auditContentValue(value); text != "" {
		return text
	}
	if value == nil {
		return ""
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
func extractAuditInputContent(input any) string {
	if values, ok := input.([]any); ok {
		for _, value := range values {
			message, ok := value.(map[string]any)
			if !ok {
				continue
			}
			role := strings.ToLower(strings.TrimSpace(userRequestAuditStringValue(message["role"])))
			if role == "system" || role == "user" {
				if content := auditAssociationValue(message["content"]); content != "" {
					return content
				}
			}
		}
		return ""
	}
	return auditAssociationValue(input)
}

func auditContentValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if text := userRequestAuditStringValue(m["text"]); text != "" {
					parts = append(parts, text)
				} else if p := userRequestAuditStringValue(m["content"]); p != "" {
					parts = append(parts, p)
				}
			} else if text := userRequestAuditStringValue(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func userRequestAuditStringValue(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func bound(value string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(value) <= max && utf8.ValidString(value) {
		return value
	}
	marker := "\n...[truncated]"
	if len(marker) >= max {
		return marker[:max]
	}
	limit := max - len(marker)
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit] + marker
}

func boundedMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	raw, _ := json.Marshal(userRequestAuditRedactValue(value))
	if len(raw) > userRequestAuditMaxChatML {
		return map[string]any{"_truncated": "metadata exceeded audit limit"}
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func userRequestAuditRedactValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if auditSensitiveKey(key) {
				out[key] = "[REDACTED]"
			} else {
				out[key] = userRequestAuditRedactValue(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = userRequestAuditRedactValue(item)
		}
		return out
	default:
		return value
	}
}

func auditSensitiveKey(key string) bool {
	lower := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", ""))
	for _, usage := range []string{"inputtokens", "outputtokens", "cachetokens", "reasoningtokens", "prompttokens", "completiontokens", "totaltokens"} {
		if lower == usage {
			return false
		}
	}
	switch lower {
	case "apikey", "authorization", "accesstoken", "refreshtoken", "clientsecret", "password", "secret", "cookie", "setcookie", "token":
		return true
	}
	return strings.HasSuffix(lower, "secret") || strings.HasSuffix(lower, "token")
}

func safeAuditContent(value string) string {
	lower := strings.ToLower(value)
	for _, marker := range []string{"\"api_key\"", "\"apikey\"", "\"authorization\"", "\"access_token\"", "\"refresh_token\"", "\"client_secret\"", "\"password\""} {
		if strings.Contains(lower, marker) {
			return "[REDACTED CONTENT]"
		}
	}
	return value
}

func userRequestAuditLogicalKey(_ UserRequestAuditCapture, _ string) string { return uuid.NewString() }
