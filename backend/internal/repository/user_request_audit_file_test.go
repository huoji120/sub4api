package repository

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
)

func TestFileUserRequestAuditConcurrentAppendReadSearch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	r := NewFileUserRequestAuditRepository(nil)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			gid := int64(i % 2)
			err := r.Create(context.Background(), &service.UserRequestAudit{UserID: int64(i), GroupID: &gid, GroupName: "group", Protocol: "openai_responses", RequestedModel: "model", RequestChatML: "secret-token", Status: "received", LogicalKey: string(rune('a' + i))})
			if err != nil {
				t.Errorf("create: %v", err)
			}
		}(i)
	}
	wg.Wait()
	rows, total, err := r.List(context.Background(), service.UserRequestAuditFilter{Q: "secret-token", Page: 1, PageSize: 100})
	if err != nil || total != 32 || len(rows) != 32 {
		t.Fatalf("search rows=%d total=%d err=%v", len(rows), total, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "user-request-audit")); err != nil {
		t.Fatal(err)
	}
}

func TestFileUserRequestAuditRestartAndComplete(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	r := NewFileUserRequestAuditRepository(nil)
	a := &service.UserRequestAudit{UserID: 1, Protocol: "openai_responses", CreatedAt: time.Now().UTC(), LogicalKey: "logical", RequestChatML: "request", Status: "received"}
	if err := r.Create(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	beforeComplete, beforeErr := r.GetByID(context.Background(), a.ID)
	if beforeErr != nil || beforeComplete.ConversationKey != "request:logical" {
		t.Fatalf("initial conversation key=%q err=%v", beforeComplete.ConversationKey, beforeErr)
	}
	if err := r.Complete(context.Background(), &service.UserRequestAuditCompletion{LogicalKey: "logical", ResponseID: "resp_123", ResponseChatML: "response", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	r2 := NewFileUserRequestAuditRepository(nil)
	got, err := r2.GetByID(context.Background(), a.ID)
	if err != nil || got.Status != "completed" || got.ResponseID != "resp_123" || got.ConversationKey != "response:resp_123" || got.ResponseChatML != "response" {
		t.Fatalf("restart got=%+v err=%v", got, err)
	}
}

func TestFileUserRequestAuditNormalizesExistingResponseConversationKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	r := NewFileUserRequestAuditRepository(nil)
	audit := &service.UserRequestAudit{UserID: 1, Protocol: "openai_responses", ResponseID: "resp_existing", FallbackHash: "old", LogicalKey: "legacy", Status: "completed"}
	if err := r.Create(context.Background(), audit); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetByID(context.Background(), audit.ID)
	if err != nil || got.ConversationKey != "response:resp_existing" {
		t.Fatalf("normalized conversation key=%q err=%v", got.ConversationKey, err)
	}
}

func TestFileUserRequestAuditConfigAndRetention(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	r := NewFileUserRequestAuditRepository(nil)
	cfg := service.UserRequestAuditConfig{RetentionDays: 3, CleanupIntervalHours: 4, MaxShardBytes: 8 * 1024 * 1024}
	if err := r.SetAuditConfig(cfg); err != nil {
		t.Fatal(err)
	}
	r2 := NewFileUserRequestAuditRepository(nil)
	got := r2.GetAuditConfig()
	b, _ := json.Marshal(got)
	if string(b) != "{\"retention_days\":3,\"cleanup_interval_hours\":4,\"max_shard_bytes\":8388608}" {
		t.Fatalf("config=%s", b)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	if err := r2.Create(context.Background(), &service.UserRequestAudit{CreatedAt: old, ExpiresAt: old.Add(time.Hour), LogicalKey: "expired", Protocol: "openai_responses"}); err != nil {
		t.Fatal(err)
	}
	deleted, err := r2.DeleteExpired(context.Background(), time.Now().UTC(), 10)
	if err != nil || deleted != 1 || r2.StorageStatus().LastCleanup.IsZero() {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
}
