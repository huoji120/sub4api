package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
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
	ContentModerationProtocolTypeSafeSystemOne: {},
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
	GroupName       string
	Protocol        string
	RequestedModel  string
	ResponseID      string
	ClientRequestID string
	Status          string
	Q               string
	Legacy          bool
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
	repo           UserRequestAuditRepository
	settings       SettingRepository
	queue          chan func(context.Context)
	stop           chan struct{}
	done           chan struct{}
	startOnce      sync.Once
	stopOnce       sync.Once
	cleanupMu      sync.Mutex
	lastCleanupAt  time.Time
	groupIDsMu     sync.RWMutex
	groupIDs       []int64
	groupIDsLoaded bool
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
			s.cleanupMu.Lock()
			cfg := s.GetArchiveConfig()
			due := s.lastCleanupAt.IsZero() || time.Since(s.lastCleanupAt) >= time.Duration(cfg.CleanupIntervalHours)*time.Hour
			if due {
				s.lastCleanupAt = time.Now().UTC()
			}
			s.cleanupMu.Unlock()
			if !due {
				continue
			}
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
	if !s.auditGroupEnabled(capture.GroupID) {
		return ""
	}
	fallback := UserRequestAuditFallbackHash(capture.UserID, capture.GroupID, capture.Body)
	var requestEvidence string
	if capture.Protocol == ContentModerationProtocolTypeSafeSystemOne {
		requestEvidence = projectNativeUserRequestAudit(capture.Body)
	} else {
		requestEvidence = ProjectUserRequestChatML(capture.Body)
	}
	audit := &UserRequestAudit{
		CreatedAt: time.Now().UTC(),
		UserID:    capture.UserID, APIKeyID: capture.APIKeyID, GroupID: cloneAuditGroupID(capture.GroupID),
		GroupName: bound(capture.GroupName, 256), Protocol: capture.Protocol, Endpoint: bound(capture.Endpoint, 512),
		RequestedModel: bound(capture.RequestedModel, 256), ClientRequestID: bound(capture.ClientRequestID, 256),
		ResponseID: bound(capture.ResponseID, 256), PreviousResponseID: bound(capture.PreviousResponseID, 256),
		FallbackHash: fallback, RequestChatML: requestEvidence, Status: "received",
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

func (s *UserRequestAuditService) auditGroupEnabled(groupID *int64) bool {
	if s == nil || groupID == nil {
		return false
	}
	groupIDs, err := s.GetAuditGroupIDs(context.Background())
	if err != nil {
		return false
	}
	for _, enabledID := range groupIDs {
		if enabledID == *groupID {
			return true
		}
	}
	return false
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

type UserRequestAuditGroupStat struct {
	GroupID     *int64    `json:"group_id,omitempty"`
	GroupName   string    `json:"group_name,omitempty"`
	Count       int64     `json:"count"`
	Completed   int64     `json:"completed"`
	Failed      int64     `json:"failed"`
	InputTotal  int64     `json:"input_total"`
	OutputTotal int64     `json:"output_total"`
	LatestAt    time.Time `json:"latest_at"`
}

const userRequestAuditMaxExportRows = 10000

func (s *UserRequestAuditService) GroupStats(ctx context.Context, filter UserRequestAuditFilter) ([]UserRequestAuditGroupStat, error) {
	filter.Page, filter.PageSize = 1, 200
	groups := make(map[string]*UserRequestAuditGroupStat)
	seen := 0
	for {
		rows, total, err := s.List(ctx, filter)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			key := "none:" + row.GroupName
			if row.GroupID != nil {
				key = fmt.Sprintf("%d:%s", *row.GroupID, row.GroupName)
			}
			stat := groups[key]
			if stat == nil {
				stat = &UserRequestAuditGroupStat{GroupID: cloneAuditGroupID(row.GroupID), GroupName: row.GroupName}
				groups[key] = stat
			}
			stat.Count++
			switch strings.ToLower(row.Status) {
			case "completed", "success", "succeeded":
				stat.Completed++
			case "failed", "error":
				stat.Failed++
			}
			stat.InputTotal += auditUsageNumber(row.InputUsage, "input_tokens", "input")
			stat.OutputTotal += auditUsageNumber(row.OutputUsage, "output_tokens", "output")
			if row.CreatedAt.After(stat.LatestAt) {
				stat.LatestAt = row.CreatedAt
			}
			seen++
		}
		if len(rows) == 0 || seen >= int(total) || seen >= userRequestAuditMaxExportRows {
			break
		}
		filter.Page++
	}
	out := make([]UserRequestAuditGroupStat, 0, len(groups))
	for _, stat := range groups {
		out = append(out, *stat)
	}
	return out, nil
}

func (s *UserRequestAuditService) StreamExport(ctx context.Context, filter UserRequestAuditFilter, format string, w io.Writer) (int, error) {
	if format != "json" {
		format = "jsonl"
	}
	enc := json.NewEncoder(w)
	written := 0
	first := true
	if format == "json" {
		if _, err := io.WriteString(w, "["); err != nil {
			return 0, err
		}
	}
	defer func() {
		if format == "json" {
			_, _ = io.WriteString(w, "]")
		}
	}()
	filter.Page, filter.PageSize = 1, 200
	for written < userRequestAuditMaxExportRows {
		rows, total, err := s.List(ctx, filter)
		if err != nil {
			return written, err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			if written >= userRequestAuditMaxExportRows {
				break
			}
			if format == "jsonl" {
				if err := enc.Encode(row); err != nil {
					return written, err
				}
			} else {
				if !first {
					if _, err := io.WriteString(w, ","); err != nil {
						return written, err
					}
				}
				first = false
				b, err := json.Marshal(row)
				if err != nil {
					return written, err
				}
				if _, err = w.Write(b); err != nil {
					return written, err
				}
			}
			written++
		}
		if written >= int(total) || len(rows) < filter.PageSize {
			break
		}
		filter.Page++
	}
	return written, nil
}

func auditUsageNumber(values map[string]any, keys ...string) int64 {
	for _, key := range keys {
		if v, ok := values[key]; ok {
			switch n := v.(type) {
			case float64:
				return int64(n)
			case int:
				return int64(n)
			case int64:
				return n
			case json.Number:
				i, _ := n.Int64()
				return i
			}
		}
	}
	return 0
}

type UserRequestAuditStorageStatus struct {
	TotalBytes        int64     `json:"total_bytes"`
	FileCount         int       `json:"file_count"`
	CurrentShardBytes int64     `json:"current_shard_bytes"`
	Oldest            time.Time `json:"oldest_at,omitempty"`
	Latest            time.Time `json:"latest_at,omitempty"`
	LastCleanup       time.Time `json:"last_cleanup_at,omitempty"`
}
type userRequestAuditStatusRepo interface {
	StorageStatus() UserRequestAuditStorageStatus
}

func (s *UserRequestAuditService) StorageStatus() UserRequestAuditStorageStatus {
	if s != nil {
		if r, ok := s.repo.(userRequestAuditStatusRepo); ok {
			return r.StorageStatus()
		}
	}
	return UserRequestAuditStorageStatus{}
}
func (s *UserRequestAuditService) CleanupNow(ctx context.Context) (int64, error) {
	return s.cleanup(ctx)
}

type UserRequestAuditConfig struct {
	RetentionDays        int     `json:"retention_days"`
	CleanupIntervalHours int     `json:"cleanup_interval_hours"`
	MaxShardBytes        int64   `json:"max_shard_bytes"`
	GroupIDs             []int64 `json:"group_ids,omitempty"`
}

const UserRequestAuditDefaultCleanupIntervalHours = 24
const UserRequestAuditDefaultMaxShardBytes int64 = 64 * 1024 * 1024

type userRequestAuditConfigRepo interface {
	GetAuditConfig() UserRequestAuditConfig
	SetAuditConfig(UserRequestAuditConfig) error
}

func (s *UserRequestAuditService) GetArchiveConfig() UserRequestAuditConfig {
	groupIDs, _ := s.GetAuditGroupIDs(context.Background())
	if groupIDs == nil {
		groupIDs = []int64{}
	}
	cfg := UserRequestAuditConfig{RetentionDays: s.retentionDays(), CleanupIntervalHours: UserRequestAuditDefaultCleanupIntervalHours, MaxShardBytes: UserRequestAuditDefaultMaxShardBytes, GroupIDs: groupIDs}
	if s != nil {
		if r, ok := s.repo.(userRequestAuditConfigRepo); ok {
			x := r.GetAuditConfig()
			if x.CleanupIntervalHours > 0 {
				cfg.CleanupIntervalHours = x.CleanupIntervalHours
			}
			if x.MaxShardBytes > 0 {
				cfg.MaxShardBytes = x.MaxShardBytes
			}
		}
	}
	return cfg
}

func (s *UserRequestAuditService) SetArchiveConfig(cfg UserRequestAuditConfig) error {
	return s.setArchiveConfig(cfg, true)
}

// SetArchiveConfigPreservingGroups updates archive settings without writing a
// previously-read group selection. This keeps an omitted group_ids field from
// resurrecting a concurrent admin change.
func (s *UserRequestAuditService) SetArchiveConfigPreservingGroups(cfg UserRequestAuditConfig) error {
	return s.setArchiveConfig(cfg, false)
}

func (s *UserRequestAuditService) setArchiveConfig(cfg UserRequestAuditConfig, updateGroups bool) error {
	if cfg.CleanupIntervalHours < 1 || cfg.CleanupIntervalHours > 168 {
		return fmt.Errorf("cleanup interval must be between 1 and 168 hours")
	}
	if cfg.MaxShardBytes < 8*1024*1024 || cfg.MaxShardBytes > 256*1024*1024 {
		return fmt.Errorf("max shard bytes must be between 8MiB and 256MiB")
	}
	if err := s.SetRetentionDays(context.Background(), cfg.RetentionDays); err != nil {
		return err
	}
	if updateGroups {
		if err := s.SetAuditGroupIDs(context.Background(), cfg.GroupIDs); err != nil {
			return err
		}
	}
	if r, ok := s.repo.(userRequestAuditConfigRepo); ok {
		return r.SetAuditConfig(cfg)
	}
	return nil
}

func (s *UserRequestAuditService) GetAuditGroupIDs(ctx context.Context) ([]int64, error) {
	if s == nil {
		return []int64{}, nil
	}
	s.groupIDsMu.Lock()
	defer s.groupIDsMu.Unlock()
	if s.groupIDsLoaded {
		return append([]int64{}, s.groupIDs...), nil
	}
	if s.settings == nil {
		s.groupIDs = []int64{}
		s.groupIDsLoaded = true
		return []int64{}, nil
	}
	raw, err := s.settings.GetValue(ctx, SettingKeyUserRequestAuditGroupIDs)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) || strings.TrimSpace(raw) == "" {
			s.groupIDs = []int64{}
			s.groupIDsLoaded = true
			return []int64{}, nil
		}
		return []int64{}, err
	}
	var ids []int64
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &ids); err != nil {
			s.groupIDs = []int64{}
			s.groupIDsLoaded = true
			return []int64{}, fmt.Errorf("invalid audit group IDs setting: %w", err)
		}
	}
	normalized, err := normalizeAuditGroupIDs(ids)
	if err != nil {
		s.groupIDs = []int64{}
		s.groupIDsLoaded = true
		return []int64{}, err
	}
	s.groupIDs = append([]int64{}, normalized...)
	s.groupIDsLoaded = true
	return append([]int64{}, normalized...), nil
}

func (s *UserRequestAuditService) SetAuditGroupIDs(ctx context.Context, ids []int64) error {
	normalized, err := normalizeAuditGroupIDs(ids)
	if err != nil {
		return err
	}
	if s == nil || s.settings == nil {
		return fmt.Errorf("user request audit settings unavailable")
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	s.groupIDsMu.Lock()
	defer s.groupIDsMu.Unlock()
	if err := s.settings.Set(ctx, SettingKeyUserRequestAuditGroupIDs, string(raw)); err != nil {
		return err
	}
	s.groupIDs = append([]int64{}, normalized...)
	s.groupIDsLoaded = true
	return nil
}

func normalizeAuditGroupIDs(ids []int64) ([]int64, error) {
	seen := make(map[int64]struct{}, len(ids))
	normalized := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("audit group IDs must be positive")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i] < normalized[j] })
	return normalized, nil
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

// System One's state/questions are native structured data, not chat messages.
// Keep the whole envelope (including provider extensions) without inventing roles.
func projectNativeUserRequestAudit(body []byte) string {
	var doc map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil {
		return "[unparsed request redacted]"
	}
	encoded, _ := json.Marshal(userRequestAuditRedactValue(doc))
	return bound(string(encoded), userRequestAuditMaxChatML)
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
