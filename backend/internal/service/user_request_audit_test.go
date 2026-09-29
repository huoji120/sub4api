package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type auditServiceTestRepo struct {
	mu         sync.Mutex
	created    []*UserRequestAudit
	completed  []*UserRequestAuditCompletion
	createdCh  chan struct{}
	completeCh chan struct{}
}

func (r *auditServiceTestRepo) Create(_ context.Context, audit *UserRequestAudit) error {
	r.mu.Lock()
	r.created = append(r.created, audit)
	r.mu.Unlock()
	if r.createdCh != nil {
		r.createdCh <- struct{}{}
	}
	return nil
}
func (r *auditServiceTestRepo) Complete(_ context.Context, completion *UserRequestAuditCompletion) error {
	r.mu.Lock()
	r.completed = append(r.completed, completion)
	r.mu.Unlock()
	if r.completeCh != nil {
		r.completeCh <- struct{}{}
	}
	return nil
}
func (r *auditServiceTestRepo) List(context.Context, UserRequestAuditFilter) ([]*UserRequestAudit, int64, error) {
	return nil, 0, nil
}
func (r *auditServiceTestRepo) GetByID(context.Context, int64) (*UserRequestAudit, error) {
	return nil, errors.New("not found")
}
func (r *auditServiceTestRepo) DeleteExpired(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

type auditServiceTestSettings struct {
	value string
	set   string
}

func (s *auditServiceTestSettings) Get(context.Context, string) (*Setting, error) { return nil, nil }
func (s *auditServiceTestSettings) GetValue(context.Context, string) (string, error) {
	return s.value, nil
}
func (s *auditServiceTestSettings) Set(_ context.Context, _, value string) error {
	s.set = value
	return nil
}
func (s *auditServiceTestSettings) GetMultiple(context.Context, []string) (map[string]string, error) {
	return nil, nil
}
func (s *auditServiceTestSettings) SetMultiple(context.Context, map[string]string) error { return nil }
func (s *auditServiceTestSettings) GetAll(context.Context) (map[string]string, error) {
	return nil, nil
}
func (s *auditServiceTestSettings) Delete(context.Context, string) error { return nil }

func TestUserRequestAuditEnqueueUsesUniqueLogicalKeysAndDrains(t *testing.T) {
	repo := &auditServiceTestRepo{createdCh: make(chan struct{}, 2), completeCh: make(chan struct{}, 2)}
	svc := NewUserRequestAuditService(repo, nil)
	first := svc.Enqueue(UserRequestAuditCapture{UserID: 9, APIKeyID: 3, Protocol: "openai_responses", Body: []byte(`{"input":"same"}`)})
	second := svc.Enqueue(UserRequestAuditCapture{UserID: 9, APIKeyID: 3, Protocol: "openai_responses", Body: []byte(`{"input":"same"}`)})
	if first == "" || second == "" || first == second {
		t.Fatalf("logical keys must be unique: %q %q", first, second)
	}
	svc.Complete(UserRequestAuditCompletion{LogicalKey: first, ResponseChatML: "<|im_start|>assistant\na<|im_end|>\n"})
	svc.Stop()
	repo.mu.Lock()
	created, completed := len(repo.created), len(repo.completed)
	repo.mu.Unlock()
	if created != 2 || completed != 1 {
		t.Fatalf("shutdown lost queued work: created=%d completed=%d", created, completed)
	}
}
func TestUserRequestAuditEmptyFallbackIsUnique(t *testing.T) {
	first := UserRequestAuditFallbackHash(9, nil, []byte(`{}`))
	second := UserRequestAuditFallbackHash(9, nil, []byte(`{}`))
	if first == second || first == "" || second == "" {
		t.Fatalf("empty fallback hashes collided: %q", first)
	}
}

func TestProjectUserRequestChatMLRetainsStructuredItemsAndSpacing(t *testing.T) {
	got := ProjectUserRequestChatML([]byte(`{"instructions":"keep  two\nlines","input":[{"type":"function_call","call_id":"c1","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"}],"usage":{"input_tokens":12}}`))
	for _, want := range []string{"keep  two\nlines", `"call_id":"c1"`, "<|im_start|>assistant", "<|im_start|>tool"} {
		if !strings.Contains(got, want) {
			t.Fatalf("projection missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "input_tokens\":\"[REDACTED]") {
		t.Fatal("usage tokens were redacted")
	}
}

func TestUserRequestAuditRetentionSettingsValidation(t *testing.T) {
	settings := &auditServiceTestSettings{value: "14"}
	svc := NewUserRequestAuditService(nil, settings)
	if got, err := svc.GetRetentionDays(context.Background()); err != nil || got != 14 {
		t.Fatalf("retention read: %d %v", got, err)
	}
	if err := svc.SetRetentionDays(context.Background(), 0); err == nil {
		t.Fatal("invalid retention accepted")
	}
	if err := svc.SetRetentionDays(context.Background(), 30); err != nil || settings.set != "30" {
		t.Fatalf("retention write: %v %q", err, settings.set)
	}
}
