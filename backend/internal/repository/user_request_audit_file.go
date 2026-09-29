package repository

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
)

const userRequestAuditShardMaxBytes int64 = 64 * 1024 * 1024
const userRequestAuditScanCap = 200000

type userRequestAuditFileLine struct {
	Event      string `json:"event"`
	LogicalKey string `json:"logical_key"`
	*service.UserRequestAudit
}

// FileUserRequestAuditRepository is an append-only NDJSON archive. Complete
// appends a replacement snapshot; readers fold snapshots by logical key.
type FileUserRequestAuditRepository struct {
	root          string
	legacy        *UserRequestAuditRepository
	mu            sync.Mutex
	maxShardBytes int64
	lastCleanup   time.Time
	nextID        atomic.Int64
}

func NewFileUserRequestAuditRepository(db *sql.DB) *FileUserRequestAuditRepository {
	root := strings.TrimSpace(os.Getenv("DATA_DIR"))
	if root == "" {
		root = "./data"
	}
	root = filepath.Join(root, "user-request-audit")
	r := &FileUserRequestAuditRepository{root: filepath.Clean(root), maxShardBytes: 64 * 1024 * 1024}
	if db != nil {
		r.legacy = NewUserRequestAuditRepository(db)
	}
	if cfg, ok := r.readConfig(); ok {
		r.maxShardBytes = cfg.MaxShardBytes
	}
	var max int64
	for _, item := range r.readAll(context.Background(), false) {
		if item.ID > max {
			max = item.ID
		}
	}
	r.nextID.Store(max)
	return r
}

func (r *FileUserRequestAuditRepository) Create(ctx context.Context, audit *service.UserRequestAudit) error {
	if audit == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(r.root, 0o750); err != nil {
		return err
	}
	now := time.Now().UTC()
	if audit.CreatedAt.IsZero() {
		audit.CreatedAt = now
	}
	if audit.UpdatedAt.IsZero() {
		audit.UpdatedAt = audit.CreatedAt
	}
	if audit.ExpiresAt.IsZero() {
		audit.ExpiresAt = audit.CreatedAt.AddDate(0, 0, service.UserRequestAuditDefaultRetentionDays)
	}
	if audit.ID <= 0 {
		audit.ID = r.nextID.Add(1)
	}
	if audit.ConversationKey == "" {
		if audit.PreviousResponseID != "" {
			for _, prior := range r.readAllLocked() {
				if prior.UserID == audit.UserID && sameGroup(prior.GroupID, audit.GroupID) && prior.Protocol == audit.Protocol && prior.ResponseID == audit.PreviousResponseID && prior.ConversationKey != "" {
					audit.ConversationKey = prior.ConversationKey
					break
				}
			}
		}
		if audit.ConversationKey == "" && audit.FallbackHash != "" {
			audit.ConversationKey = "fallback:" + audit.FallbackHash
		}
		if audit.ConversationKey == "" {
			audit.ConversationKey = "request:" + audit.LogicalKey
		}
	}
	return r.appendLocked(ctx, "create", audit)
}

func (r *FileUserRequestAuditRepository) Complete(ctx context.Context, completion *service.UserRequestAuditCompletion) error {
	if completion == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := r.readAllLocked()
	var found *service.UserRequestAudit
	for _, row := range rows {
		if row.LogicalKey == completion.LogicalKey {
			found = row
		}
	}
	if found == nil {
		return sql.ErrNoRows
	}
	found.UpdatedAt = time.Now().UTC()
	if completion.ResponseID != "" {
		found.ResponseID = completion.ResponseID
		if completion.ResponseID != "" && (strings.HasPrefix(found.ConversationKey, "fallback:") || strings.HasPrefix(found.ConversationKey, "request:")) {
			found.ConversationKey = "response:" + completion.ResponseID
		}
	}
	if completion.UpstreamModel != "" {
		found.UpstreamModel = completion.UpstreamModel
	}
	if completion.ResponseChatML != "" {
		found.ResponseChatML = appendAuditChatMLFile(found.ResponseChatML, completion.ResponseChatML)
	}
	if completion.Status != "" {
		found.Status = completion.Status
	}
	if completion.InputUsage != nil {
		found.InputUsage = completion.InputUsage
	}
	if completion.OutputUsage != nil {
		found.OutputUsage = completion.OutputUsage
	}
	if completion.CacheUsage != nil {
		found.CacheUsage = completion.CacheUsage
	}
	if completion.LastError != "" {
		found.LastError = completion.LastError
	}
	if len(completion.Metadata) != 0 {
		if found.Metadata == nil {
			found.Metadata = map[string]any{}
		}
		for k, v := range completion.Metadata {
			found.Metadata[k] = v
		}
	}
	if err := r.appendLocked(ctx, "complete", found); err != nil {
		return err
	}
	return nil
}

func (r *FileUserRequestAuditRepository) List(ctx context.Context, filter service.UserRequestAuditFilter) ([]*service.UserRequestAudit, int64, error) {
	if filter.Legacy && r.legacy != nil {
		return r.legacy.List(ctx, filter)
	}
	rows := r.filtered(ctx, filter)
	if len(rows) == 0 && r.legacy != nil && len(r.readAll(ctx, false)) == 0 {
		return r.legacy.List(ctx, filter)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].ID > rows[j].ID
		}
		return rows[i].CreatedAt.After(rows[j].CreatedAt)
	})
	total := int64(len(rows))
	page, size := filter.Page, filter.PageSize
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if size > 200 {
		size = 200
	}
	start := (page - 1) * size
	if start >= len(rows) {
		return []*service.UserRequestAudit{}, total, nil
	}
	end := start + size
	if end > len(rows) {
		end = len(rows)
	}
	return rows[start:end], total, nil
}

func (r *FileUserRequestAuditRepository) GetByID(ctx context.Context, id int64) (*service.UserRequestAudit, error) {
	for _, row := range r.readAll(ctx, true) {
		if row.ID == id {
			row.ChatML = row.RequestChatML + "\n" + row.ResponseChatML
			return row, nil
		}
	}
	if r.legacy != nil {
		return r.legacy.GetByID(ctx, id)
	}
	return nil, sql.ErrNoRows
}

func (r *FileUserRequestAuditRepository) DeleteExpired(ctx context.Context, before time.Time, batchSize int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := r.readAllLocked()
	var deleted int64
	kept := make([]*service.UserRequestAudit, 0, len(rows))
	limit := batchSize
	if limit <= 0 {
		limit = 500
	}
	for _, row := range rows {
		if row.ExpiresAt.Before(before) && int(deleted) < limit {
			deleted++
			continue
		}
		kept = append(kept, row)
	}
	if deleted == 0 {
		return 0, nil
	}
	if err := r.rewriteLocked(kept); err != nil {
		return 0, err
	}
	r.lastCleanup = time.Now().UTC()
	return deleted, nil
}

func (r *FileUserRequestAuditRepository) readAll(ctx context.Context, includeContent bool) []*service.UserRequestAudit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readAllLockedCtx(ctx, includeContent)
}
func (r *FileUserRequestAuditRepository) readAllLocked() []*service.UserRequestAudit {
	return r.readAllLockedCtx(context.Background(), true)
}
func (r *FileUserRequestAuditRepository) readAllLockedCtx(ctx context.Context, includeContent bool) []*service.UserRequestAudit {
	files, _ := filepath.Glob(filepath.Join(r.root, "*.ndjson"))
	sort.Strings(files)
	latest := make(map[string]*service.UserRequestAudit)
	byID := make(map[int64]*service.UserRequestAudit)
	scanned := 0
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				_ = f.Close()
				return values(latest, byID, includeContent)
			}
			scanned++
			if scanned > userRequestAuditScanCap {
				break
			}
			var line userRequestAuditFileLine
			if json.Unmarshal(scanner.Bytes(), &line) != nil || line.UserRequestAudit == nil {
				continue
			}
			if line.UserRequestAudit.LogicalKey == "" {
				line.UserRequestAudit.LogicalKey = line.LogicalKey
			}
			row := line.UserRequestAudit
			if !includeContent {
				row.RequestChatML = ""
				row.ResponseChatML = ""
			}
			if row.LogicalKey != "" {
				latest[row.LogicalKey] = row
			}
			byID[row.ID] = row
		}
		_ = f.Close()
		if scanned > userRequestAuditScanCap {
			break
		}
	}
	return values(latest, byID, includeContent)
}
func values(latest map[string]*service.UserRequestAudit, byID map[int64]*service.UserRequestAudit, includeContent bool) []*service.UserRequestAudit {
	out := make([]*service.UserRequestAudit, 0, len(latest))
	seen := map[int64]bool{}
	for _, row := range latest {
		normalizeConversationKey(row)
		if !seen[row.ID] {
			out = append(out, row)
			seen[row.ID] = true
		}
	}
	for id, row := range byID {
		normalizeConversationKey(row)
		if !seen[id] && row.LogicalKey == "" {
			out = append(out, row)
		}
	}
	return out
}

func normalizeConversationKey(row *service.UserRequestAudit) {
	if row == nil || row.ResponseID == "" {
		return
	}
	if strings.HasPrefix(row.ConversationKey, "fallback:") || strings.HasPrefix(row.ConversationKey, "request:") {
		row.ConversationKey = "response:" + row.ResponseID
	}
}

func (r *FileUserRequestAuditRepository) filtered(ctx context.Context, filter service.UserRequestAuditFilter) []*service.UserRequestAudit {
	rows := r.readAll(ctx, strings.TrimSpace(filter.Q) != "")
	out := rows[:0]
	q := strings.ToLower(strings.TrimSpace(filter.Q))
	for _, row := range rows {
		if filter.UserID != nil && row.UserID != *filter.UserID || filter.APIKeyID != nil && row.APIKeyID != *filter.APIKeyID || filter.GroupID != nil && !sameGroup(row.GroupID, filter.GroupID) || filter.GroupName != "" && !strings.EqualFold(row.GroupName, filter.GroupName) || filter.Protocol != "" && !strings.EqualFold(row.Protocol, filter.Protocol) || filter.RequestedModel != "" && !strings.EqualFold(row.RequestedModel, filter.RequestedModel) || filter.ResponseID != "" && !strings.EqualFold(row.ResponseID, filter.ResponseID) || filter.ClientRequestID != "" && !strings.EqualFold(row.ClientRequestID, filter.ClientRequestID) || filter.Status != "" && !strings.EqualFold(row.Status, filter.Status) {
			continue
		}
		if filter.StartTime != nil && row.CreatedAt.Before(*filter.StartTime) {
			continue
		}
		if filter.EndTime != nil && !row.CreatedAt.Before(*filter.EndTime) {
			continue
		}
		if q != "" {
			blob := strings.ToLower(row.GroupName + " " + row.Protocol + " " + row.RequestedModel + " " + row.ClientRequestID + " " + row.ResponseID + " " + row.LastError + " " + string(mustJSON(row.Metadata)) + " " + row.RequestChatML + " " + row.ResponseChatML)
			if !strings.Contains(blob, q) {
				continue
			}
		}
		out = append(out, row)
	}
	return out
}

func (r *FileUserRequestAuditRepository) appendLocked(ctx context.Context, event string, row *service.UserRequestAudit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, err := json.Marshal(userRequestAuditFileLine{Event: event, LogicalKey: row.LogicalKey, UserRequestAudit: row})
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	path, err := r.shardPathLocked(row.CreatedAt, len(payload))
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	_, err = f.Write(payload)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}
func (r *FileUserRequestAuditRepository) shardPathLocked(at time.Time, add int) (string, error) {
	if err := os.MkdirAll(r.root, 0o750); err != nil {
		return "", err
	}
	limit := r.maxShardBytes
	if limit < 8*1024*1024 {
		limit = 64 * 1024 * 1024
	}
	day := at.UTC().Format("2006-01-02")
	idx := 0
	for {
		name := filepath.Join(r.root, day+"-"+strconv.Itoa(idx)+".ndjson")
		st, err := os.Stat(name)
		if os.IsNotExist(err) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
		if st.Size()+int64(add) <= limit {
			return name, nil
		}
		idx++
	}
}
func (r *FileUserRequestAuditRepository) rewriteLocked(rows []*service.UserRequestAudit) error {
	tmp := r.root + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, row := range rows {
		if err := enc.Encode(userRequestAuditFileLine{Event: "snapshot", LogicalKey: row.LogicalKey, UserRequestAudit: row}); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	files, _ := filepath.Glob(filepath.Join(r.root, "*.ndjson"))
	for _, name := range files {
		_ = os.Remove(name)
	}
	return os.Rename(tmp, filepath.Join(r.root, "retained-"+time.Now().UTC().Format("20060102T150405")+".ndjson"))
}
func sameGroup(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func appendAuditChatMLFile(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" || a == b {
		return a
	}
	const max = 128 * 1024
	v := a + "\n" + b
	if len(v) <= max {
		return v
	}
	return v[:max]
}

// UserRequestAuditRoot reports the archive directory for status/config endpoints.
func (r *FileUserRequestAuditRepository) UserRequestAuditRoot() string {
	if r == nil {
		return ""
	}
	return r.root
}

var _ service.UserRequestAuditRepository = (*FileUserRequestAuditRepository)(nil)

func (r *FileUserRequestAuditRepository) StorageStatus() service.UserRequestAuditStorageStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out service.UserRequestAuditStorageStatus
	out.LastCleanup = r.lastCleanup
	files, _ := filepath.Glob(filepath.Join(r.root, "*.ndjson"))
	out.FileCount = len(files)
	latestMod := time.Time{}
	for _, name := range files {
		st, err := os.Stat(name)
		if err != nil {
			continue
		}
		out.TotalBytes += st.Size()
		if st.ModTime().After(out.Latest) {
			out.Latest = st.ModTime()
		}
		if out.Oldest.IsZero() || st.ModTime().Before(out.Oldest) {
			out.Oldest = st.ModTime()
		}
		if st.ModTime().After(latestMod) {
			latestMod = st.ModTime()
			out.CurrentShardBytes = st.Size()
		}
	}
	return out
}
func (r *FileUserRequestAuditRepository) configPath() string {
	return filepath.Join(r.root, "config.json")
}
func (r *FileUserRequestAuditRepository) readConfig() (service.UserRequestAuditConfig, bool) {
	var cfg service.UserRequestAuditConfig
	b, err := os.ReadFile(r.configPath())
	if err != nil || json.Unmarshal(b, &cfg) != nil {
		return cfg, false
	}
	return cfg, true
}
func (r *FileUserRequestAuditRepository) GetAuditConfig() service.UserRequestAuditConfig {
	if cfg, ok := r.readConfig(); ok {
		return cfg
	}
	return service.UserRequestAuditConfig{CleanupIntervalHours: service.UserRequestAuditDefaultCleanupIntervalHours, MaxShardBytes: service.UserRequestAuditDefaultMaxShardBytes}
}
func (r *FileUserRequestAuditRepository) SetAuditConfig(cfg service.UserRequestAuditConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(r.root, 0o750); err != nil {
		return err
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	tmp := r.configPath() + ".tmp"
	if err = os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	if err = os.Rename(tmp, r.configPath()); err != nil {
		return err
	}
	r.maxShardBytes = cfg.MaxShardBytes
	return nil
}
