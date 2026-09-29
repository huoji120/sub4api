package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/MACOS-DO/sub4api/internal/service"
)

func TestUserRequestAuditCreateLinksPreviousWithinScope(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewUserRequestAuditRepository(db)
	group := int64(4)
	now := time.Now().UTC()
	mock.ExpectBegin()
	query := mock.ExpectQuery(regexp.QuoteMeta("SELECT conversation_key FROM user_request_audits"))
	query.WithArgs(int64(7), group, "openai_responses", "resp-1")
	query.WillReturnRows(sqlmock.NewRows([]string{"conversation_key"}).AddRow("fallback:conversation"))
	insert := mock.ExpectExec(regexp.QuoteMeta("INSERT INTO user_request_audits"))
	insert.WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	if err := r.Create(context.Background(), &service.UserRequestAudit{CreatedAt: now, ExpiresAt: now.AddDate(0, 0, 7), UserID: 7, APIKeyID: 8, GroupID: &group, GroupName: "g", Protocol: "openai_responses", Endpoint: "/v1/responses", RequestedModel: "m", ResponseID: "resp-2", PreviousResponseID: "resp-1", FallbackHash: "hash", RequestChatML: "req", LogicalKey: "logical"}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUserRequestAuditCompleteAppendsWithoutNullingError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewUserRequestAuditRepository(db)
	mock.ExpectBegin()
	query := mock.ExpectQuery(regexp.QuoteMeta("SELECT response_chatml FROM user_request_audits"))
	query.WithArgs("logical")
	query.WillReturnRows(sqlmock.NewRows([]string{"response_chatml"}).AddRow("<|im_start|>assistant\none<|im_end|>\n"))
	update := mock.ExpectExec(regexp.QuoteMeta("UPDATE user_request_audits SET updated_at=NOW()"))
	update.WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := r.Complete(context.Background(), &service.UserRequestAuditCompletion{LogicalKey: "logical", ResponseID: "resp", UpstreamModel: "model", ResponseChatML: "<|im_start|>assistant\ntwo<|im_end|>\n", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMergeAuditHistoryDropsOnlyResentPrefix(t *testing.T) {
	first := "<|im_start|>system\ns<|im_end|>\n<|im_start|>user\none<|im_end|>\n"
	second := first + "<|im_start|>user\ntwo<|im_end|>\n"
	got := mergeAuditHistory(first, second)
	want := first + "<|im_start|>user\ntwo<|im_end|>\n"
	if got != want {
		t.Fatalf("merged history = %q, want %q", got, want)
	}
}
