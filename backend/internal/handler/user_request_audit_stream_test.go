package handler

import (
	"strings"
	"testing"
)

func TestAuditResponseFieldsExtractsResponseIdentityFromSSE(t *testing.T) {
	raw := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_123\",\"model\":\"gpt-5\"}}\n")
	id, model := auditResponseFields(raw)
	if id != "resp_123" || model != "gpt-5" {
		t.Fatalf("response identity = (%q, %q)", id, model)
	}
	if !strings.Contains(auditResponseChatML(raw), "hello") {
		t.Fatal("audit transcript omitted real response content")
	}
}

func TestAuditStreamIncompleteDistinguishesTerminalAndTruncatedSSE(t *testing.T) {
	if !auditStreamIncomplete([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n")) {
		t.Fatal("truncated stream was not marked incomplete")
	}
	if auditStreamIncomplete([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\ndata: {\"type\":\"response.completed\"}\n")) {
		t.Fatal("terminal stream was marked incomplete")
	}
	if auditStreamIncomplete([]byte("data: [DONE]\n")) {
		t.Fatal("DONE stream was marked incomplete")
	}
}
