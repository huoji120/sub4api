package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MACOS-DO/sub4api/internal/service"
)

type UserRequestAuditRepository struct{ db *sql.DB }

func NewUserRequestAuditRepository(db *sql.DB) *UserRequestAuditRepository {
	return &UserRequestAuditRepository{db: db}
}

const auditSelectColumns = `id,created_at,updated_at,expires_at,user_id,api_key_id,group_id,group_name,protocol,endpoint,requested_model,upstream_model,client_request_id,response_id,previous_response_id,fallback_hash,conversation_key,request_chatml,response_chatml,status,input_usage,output_usage,cache_usage,last_error,metadata,logical_key`

func (r *UserRequestAuditRepository) Create(ctx context.Context, audit *service.UserRequestAudit) error {
	if r == nil || r.db == nil || audit == nil {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	created := audit.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	expires := audit.ExpiresAt
	if expires.IsZero() {
		expires = created.AddDate(0, 0, service.UserRequestAuditDefaultRetentionDays)
	}
	conversationKey := ""
	if audit.PreviousResponseID != "" {
		lookupErr := tx.QueryRowContext(ctx, `SELECT conversation_key FROM user_request_audits WHERE user_id=$1 AND group_id IS NOT DISTINCT FROM $2 AND protocol=$3 AND response_id=$4 AND conversation_key<>'' ORDER BY created_at DESC,id DESC LIMIT 1`, audit.UserID, audit.GroupID, audit.Protocol, audit.PreviousResponseID).Scan(&conversationKey)
		if lookupErr != nil && lookupErr != sql.ErrNoRows {
			return lookupErr
		}
	}
	if conversationKey == "" {
		if audit.FallbackHash != "" {
			conversationKey = "fallback:" + audit.FallbackHash
		} else {
			conversationKey = "request:" + audit.LogicalKey
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO user_request_audits
		(created_at,updated_at,expires_at,user_id,api_key_id,group_id,group_name,protocol,endpoint,requested_model,upstream_model,client_request_id,response_id,previous_response_id,fallback_hash,conversation_key,request_chatml,response_chatml,status,input_usage,output_usage,cache_usage,last_error,metadata,logical_key)
		VALUES($1,$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19::jsonb,$20::jsonb,$21::jsonb,$22,$23::jsonb,$24)`,
		created, expires, audit.UserID, audit.APIKeyID, audit.GroupID, audit.GroupName, audit.Protocol, audit.Endpoint,
		audit.RequestedModel, audit.UpstreamModel, audit.ClientRequestID, audit.ResponseID, audit.PreviousResponseID,
		audit.FallbackHash, conversationKey, audit.RequestChatML, audit.ResponseChatML, audit.Status,
		jsonValue(audit.InputUsage), jsonValue(audit.OutputUsage), jsonValue(audit.CacheUsage), audit.LastError, jsonValue(audit.Metadata), audit.LogicalKey)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	audit.CreatedAt, audit.ExpiresAt, audit.ConversationKey = created, expires, conversationKey
	return nil
}

func (r *UserRequestAuditRepository) Complete(ctx context.Context, completion *service.UserRequestAuditCompletion) error {
	if r == nil || r.db == nil || completion == nil {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing string
	if err := tx.QueryRowContext(ctx, `SELECT response_chatml FROM user_request_audits WHERE logical_key=$1 FOR UPDATE`, completion.LogicalKey).Scan(&existing); err != nil {
		if err == sql.ErrNoRows {
			return sql.ErrNoRows
		}
		return err
	}
	response := appendAuditChatML(existing, completion.ResponseChatML)
	result, err := tx.ExecContext(ctx, `UPDATE user_request_audits SET updated_at=NOW(), response_id=CASE WHEN $2<>'' THEN $2 ELSE response_id END, upstream_model=CASE WHEN $3<>'' THEN $3 ELSE upstream_model END, response_chatml=$4, status=CASE WHEN $5<>'' THEN $5 ELSE status END, input_usage=COALESCE($6::jsonb,input_usage), output_usage=COALESCE($7::jsonb,output_usage), cache_usage=COALESCE($8::jsonb,cache_usage), last_error=CASE WHEN $9<>'' THEN $9 ELSE last_error END, metadata=CASE WHEN $10::jsonb IS NULL THEN metadata ELSE COALESCE(metadata,'{}'::jsonb) || $10::jsonb END WHERE logical_key=$1`, completion.LogicalKey, completion.ResponseID, completion.UpstreamModel, response, completion.Status, nullableJSON(completion.InputUsage), nullableJSON(completion.OutputUsage), nullableJSON(completion.CacheUsage), completion.LastError, nullableJSON(completion.Metadata))
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (r *UserRequestAuditRepository) List(ctx context.Context, filter service.UserRequestAuditFilter) ([]*service.UserRequestAudit, int64, error) {
	if r == nil || r.db == nil {
		return nil, 0, nil
	}
	where, args := auditWhere(filter)
	var total int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_request_audits "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
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
	args = append(args, size, (page-1)*size)
	rows, err := r.db.QueryContext(ctx, `SELECT `+auditSelectColumns+` FROM user_request_audits `+where+` ORDER BY created_at DESC,id DESC LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]*service.UserRequestAudit, 0)
	for rows.Next() {
		item, scanErr := scanUserRequestAudit(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		item.RequestChatML, item.ResponseChatML = "", ""
		items = append(items, item)
	}
	return items, total, rows.Err()
}

func (r *UserRequestAuditRepository) GetByID(ctx context.Context, id int64) (*service.UserRequestAudit, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrNoRows
	}
	item, err := scanUserRequestAudit(r.db.QueryRowContext(ctx, `SELECT `+auditSelectColumns+` FROM user_request_audits WHERE id=$1`, id))
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT request_chatml,response_chatml FROM user_request_audits WHERE user_id=$1 AND group_id IS NOT DISTINCT FROM $2 AND protocol=$3 AND conversation_key=$4 ORDER BY created_at ASC,id ASC LIMIT 256`, item.UserID, item.GroupID, item.Protocol, item.ConversationKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var merged string
	for rows.Next() {
		var request, response string
		if err := rows.Scan(&request, &response); err != nil {
			return nil, err
		}
		merged = mergeAuditHistory(merged, request)
		merged = appendAuditChatML(merged, response)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	item.ChatML = merged
	return item, nil
}

func (r *UserRequestAuditRepository) DeleteExpired(ctx context.Context, before time.Time, batchSize int) (int64, error) {
	if r == nil || r.db == nil {
		return 0, nil
	}
	if batchSize <= 0 || batchSize > 5000 {
		batchSize = 500
	}
	result, err := r.db.ExecContext(ctx, `DELETE FROM user_request_audits WHERE id IN (SELECT id FROM user_request_audits WHERE expires_at < $1 ORDER BY expires_at,id LIMIT $2)`, before, batchSize)
	if err != nil {
		return 0, err
	}
	count, _ := result.RowsAffected()
	return count, nil
}

type auditScanner interface{ Scan(...any) error }

func scanUserRequestAudit(row auditScanner) (*service.UserRequestAudit, error) {
	var item service.UserRequestAudit
	var groupID sql.NullInt64
	var input, output, cache, metadata []byte
	err := row.Scan(&item.ID, &item.CreatedAt, &item.UpdatedAt, &item.ExpiresAt, &item.UserID, &item.APIKeyID, &groupID, &item.GroupName, &item.Protocol, &item.Endpoint, &item.RequestedModel, &item.UpstreamModel, &item.ClientRequestID, &item.ResponseID, &item.PreviousResponseID, &item.FallbackHash, &item.ConversationKey, &item.RequestChatML, &item.ResponseChatML, &item.Status, &input, &output, &cache, &item.LastError, &metadata, &item.LogicalKey)
	if err != nil {
		return nil, err
	}
	if groupID.Valid {
		item.GroupID = &groupID.Int64
	}
	decodeJSON(input, &item.InputUsage)
	decodeJSON(output, &item.OutputUsage)
	decodeJSON(cache, &item.CacheUsage)
	decodeJSON(metadata, &item.Metadata)
	return &item, nil
}

func auditWhere(filter service.UserRequestAuditFilter) (string, []any) {
	clauses := []string{"1=1"}
	args := make([]any, 0, 10)
	add := func(sqlPart string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(sqlPart, len(args)))
	}
	if filter.UserID != nil {
		add("user_id=$%d", *filter.UserID)
	}
	if filter.APIKeyID != nil {
		add("api_key_id=$%d", *filter.APIKeyID)
	}
	if filter.GroupID != nil {
		add("group_id=$%d", *filter.GroupID)
	}
	if value := strings.TrimSpace(filter.Protocol); value != "" {
		add("protocol=$%d", value)
	}
	if value := strings.TrimSpace(filter.RequestedModel); value != "" {
		add("requested_model=$%d", value)
	}
	if value := strings.TrimSpace(filter.ResponseID); value != "" {
		add("response_id=$%d", value)
	}
	if value := strings.TrimSpace(filter.ClientRequestID); value != "" {
		add("client_request_id=$%d", value)
	}
	if value := strings.TrimSpace(filter.Status); value != "" {
		add("status=$%d", value)
	}
	if filter.StartTime != nil {
		add("created_at >= $%d", *filter.StartTime)
	}
	if filter.EndTime != nil {
		add("created_at < $%d", *filter.EndTime)
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

func jsonValue(value map[string]any) []byte {
	if value == nil {
		return []byte("null")
	}
	raw, _ := json.Marshal(value)
	return raw
}
func nullableJSON(value map[string]any) any {
	if value == nil {
		return nil
	}
	return jsonValue(value)
}
func decodeJSON(raw []byte, dst *map[string]any) {
	if len(raw) > 0 && string(raw) != "null" {
		_ = json.Unmarshal(raw, dst)
	}
}

const auditMergedChatMLLimit = 128 * 1024

func appendAuditChatML(existing, next string) string {
	if next == "" {
		return existing
	}
	if existing == "" {
		return next
	}
	if existing == next {
		return existing
	}
	separator := "\n"
	if strings.HasSuffix(existing, "\n") || strings.HasPrefix(next, "\n") {
		separator = ""
	}
	return boundAuditText(existing+separator+next, auditMergedChatMLLimit)
}

func mergeAuditHistory(existing, request string) string {
	if request == "" {
		return existing
	}
	blocks := chatMLBlocks(request)
	if len(blocks) == 0 {
		return appendAuditChatML(existing, request)
	}
	prior := chatMLBlocks(existing)
	matched := 0
	for matched < len(blocks) && matched < len(prior) && blocks[matched] == prior[matched] {
		matched++
	}
	for _, block := range blocks[matched:] {
		existing = appendAuditChatML(existing, block)
	}
	return existing
}

func chatMLBlocks(value string) []string {
	if value == "" {
		return nil
	}
	var blocks []string
	for len(value) > 0 {
		start := strings.Index(value, "<|im_start|>")
		if start < 0 {
			if len(blocks) == 0 {
				return []string{value}
			}
			break
		}
		if start > 0 && len(blocks) == 0 {
			blocks = append(blocks, value[:start])
		}
		end := strings.Index(value[start:], "<|im_end|>")
		if end < 0 {
			blocks = append(blocks, value[start:])
			break
		}
		end += start + len("<|im_end|>")
		if end < len(value) && value[end] == '\n' {
			end++
		}
		blocks = append(blocks, value[start:end])
		value = value[end:]
	}
	return blocks
}

func boundAuditText(value string, max int) string {
	if len(value) <= max && utf8.ValidString(value) {
		return value
	}
	marker := "\n...[truncated]"
	limit := max - len(marker)
	if limit < 0 {
		return marker[:max]
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit] + marker
}
