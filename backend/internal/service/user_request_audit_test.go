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
	mu         sync.Mutex
	value      string
	groupValue string
	set        string
	setErr     error
}

func (s *auditServiceTestSettings) Get(context.Context, string) (*Setting, error) { return nil, nil }
func (s *auditServiceTestSettings) GetValue(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == SettingKeyUserRequestAuditGroupIDs && s.groupValue != "" {
		return s.groupValue, nil
	}
	return s.value, nil
}
func (s *auditServiceTestSettings) Set(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set = value
	if key == SettingKeyUserRequestAuditGroupIDs {
		if s.setErr != nil {
			return s.setErr
		}
		s.groupValue = value
	}
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
	settings := &auditServiceTestSettings{groupValue: "[3]"}
	svc := NewUserRequestAuditService(repo, settings)
	groupID := int64(3)
	first := svc.Enqueue(UserRequestAuditCapture{UserID: 9, APIKeyID: 3, GroupID: &groupID, Protocol: "openai_responses", Body: []byte(`{"input":"same"}`)})
	second := svc.Enqueue(UserRequestAuditCapture{UserID: 9, APIKeyID: 3, GroupID: &groupID, Protocol: "openai_responses", Body: []byte(`{"input":"same"}`)})
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
func TestUserRequestAuditCaptureIsOptInByGroup(t *testing.T) {
	groupID := int64(7)
	otherID := int64(8)
	svc := NewUserRequestAuditService(nil, &auditServiceTestSettings{groupValue: "[7]"})
	if key := svc.Enqueue(UserRequestAuditCapture{GroupID: nil, Protocol: "openai_responses"}); key != "" {
		t.Fatal("group-less capture must be disabled")
	}
	if key := svc.Enqueue(UserRequestAuditCapture{GroupID: &otherID, Protocol: "openai_responses"}); key != "" {
		t.Fatal("unselected group capture must be disabled")
	}
	if key := svc.Enqueue(UserRequestAuditCapture{GroupID: &groupID, Protocol: "openai_responses"}); key == "" {
		t.Fatal("selected group capture must be enabled")
	}
	if key := NewUserRequestAuditService(nil, nil).Enqueue(UserRequestAuditCapture{GroupID: &groupID, Protocol: "openai_responses"}); key != "" {
		t.Fatal("nil settings must fail closed")
	}
}

func TestUserRequestAuditGroupSelectionClearReloadAndFailedSave(t *testing.T) {
	groupID := int64(7)
	settings := &auditServiceTestSettings{groupValue: "[7]"}
	svc := NewUserRequestAuditService(nil, settings)
	if err := svc.SetAuditGroupIDs(context.Background(), []int64{}); err != nil {
		t.Fatalf("clear selection: %v", err)
	}
	if key := svc.Enqueue(UserRequestAuditCapture{GroupID: &groupID, Protocol: "openai_responses"}); key != "" {
		t.Fatal("cleared selection still enabled capture")
	}
	reloaded := NewUserRequestAuditService(nil, settings)
	if key := reloaded.Enqueue(UserRequestAuditCapture{GroupID: &groupID, Protocol: "openai_responses"}); key != "" {
		t.Fatal("cleared selection did not persist across service reload")
	}
	if err := svc.SetAuditGroupIDs(context.Background(), []int64{7}); err != nil {
		t.Fatalf("restore selection: %v", err)
	}
	settings.setErr = errors.New("save failed")
	if err := svc.SetAuditGroupIDs(context.Background(), []int64{8}); err == nil {
		t.Fatal("failed selection save accepted")
	}
	if key := svc.Enqueue(UserRequestAuditCapture{GroupID: &groupID, Protocol: "openai_responses"}); key == "" {
		t.Fatal("failed save changed active selection")
	}
}

func TestUserRequestAuditInvalidSelectionFailsClosed(t *testing.T) {
	groupID := int64(7)
	for _, raw := range []string{"not-json", "[0]"} {
		svc := NewUserRequestAuditService(nil, &auditServiceTestSettings{groupValue: raw})
		if key := svc.Enqueue(UserRequestAuditCapture{GroupID: &groupID, Protocol: "openai_responses"}); key != "" {
			t.Fatalf("invalid selection %q enabled capture", raw)
		}
	}
}
