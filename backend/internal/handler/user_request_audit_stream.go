package handler

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const userRequestAuditResponseLimit = 256 * 1024

// auditResponseWriter records the bytes actually written to the downstream
// client. It deliberately delegates all optional interfaces to gin's writer.
type auditResponseWriter struct {
	gin.ResponseWriter
	mu     sync.Mutex
	body   bytes.Buffer
	status int
}

func (w *auditResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	if w.status == 0 {
		w.status = status
	}
	w.mu.Unlock()
	w.ResponseWriter.WriteHeader(status)
}
func (w *auditResponseWriter) Write(p []byte) (int, error) {
	w.capture(p)
	return w.ResponseWriter.Write(p)
}
func (w *auditResponseWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *auditResponseWriter) capture(p []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.body.Len() < userRequestAuditResponseLimit {
		n := userRequestAuditResponseLimit - w.body.Len()
		if len(p) > n {
			p = p[:n]
		}
		_, _ = w.body.Write(p)
	}
}
func (w *auditResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *auditResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}
func (w *auditResponseWriter) CloseNotify() <-chan bool {
	if n, ok := w.ResponseWriter.(http.CloseNotifier); ok {
		return n.CloseNotify()
	}
	return make(chan bool)
}
func (w *auditResponseWriter) Pusher() http.Pusher {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p
	}
	return nil
}

// userRequestAuditRecorder owns one inbound request key. It is intentionally
// fire-and-forget: database queue pressure must never delay the API response.
type userRequestAuditRecorder struct {
	service  *service.UserRequestAuditService
	key      string
	writer   *auditResponseWriter
	finished bool
}

func beginUserRequestAudit(c *gin.Context, svc *service.UserRequestAuditService, protocol, endpoint, model string, body []byte, apiKey *service.APIKey, userID int64) *userRequestAuditRecorder {
	if svc == nil || c == nil || apiKey == nil {
		return nil
	}
	var groupID *int64
	groupName := ""
	if apiKey.GroupID != nil {
		v := *apiKey.GroupID
		groupID = &v
	}
	if apiKey.Group != nil {
		groupName = apiKey.Group.Name
	}
	capture := service.UserRequestAuditCapture{UserID: userID, APIKeyID: apiKey.ID, GroupID: groupID, GroupName: groupName, Protocol: protocol, Endpoint: endpoint, RequestedModel: model, ClientRequestID: strings.TrimSpace(c.GetHeader("X-Request-ID")), ResponseID: strings.TrimSpace(gjson.GetBytes(body, "response.id").String()), PreviousResponseID: strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()), Body: body, Metadata: map[string]any{"user_agent": c.GetHeader("User-Agent")}}
	rec := &userRequestAuditRecorder{service: svc, key: svc.Enqueue(capture)}
	if rec.key == "" {
		return rec
	}
	if c.Writer != nil {
		rec.writer = &auditResponseWriter{ResponseWriter: c.Writer}
		c.Writer = rec.writer
	}
	return rec
}

func (r *userRequestAuditRecorder) Finish(status int, err error, metadata map[string]any) {
	if r == nil || r.finished {
		return
	}
	r.finished = true
	if r.key == "" {
		return
	}
	var raw []byte
	if r.writer != nil {
		r.writer.mu.Lock()
		raw = append([]byte(nil), r.writer.body.Bytes()...)
		if status == 0 {
			status = r.writer.status
		}
		r.writer.mu.Unlock()
	}
	responseID, model := auditResponseFields(raw)
	if status == 0 {
		status = http.StatusOK
	}
	if err == nil && auditStreamIncomplete(raw) {
		err = errors.New("stream ended before terminal response")
	}
	state := "completed"
	if err != nil || status >= 400 {
		state = "failed"
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["response_bytes"] = len(raw)
	if len(raw) > 0 {
		metadata["response_sha256"] = auditSHA256(raw)
	}
	r.service.Complete(service.UserRequestAuditCompletion{LogicalKey: r.key, ResponseID: responseID, UpstreamModel: model, ResponseChatML: auditResponseChatML(raw), Status: state, LastError: auditErrorString(err), Metadata: metadata})
}

func auditErrorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
func auditSHA256(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func auditResponseChatML(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	return "<|assistant|>\n" + string(raw)
}
func auditResponseFields(raw []byte) (string, string) {
	if len(raw) == 0 {
		return "", ""
	}
	var id, model string
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) {
			continue
		}
		var v any
		if json.Unmarshal(line, &v) != nil {
			continue
		}
		if id == "" {
			id = strings.TrimSpace(gjson.GetBytes(line, "id").String())
			if id == "" {
				id = strings.TrimSpace(gjson.GetBytes(line, "response.id").String())
			}
		}
		if model == "" {
			model = strings.TrimSpace(gjson.GetBytes(line, "model").String())
			if model == "" {
				model = strings.TrimSpace(gjson.GetBytes(line, "response.model").String())
			}
		}
	}
	return id, model
}

func endpointForAudit(c *gin.Context, fallback string) string {
	if c != nil && c.Request != nil && c.Request.URL != nil {
		return c.Request.URL.Path
	}
	return fallback
}

type wsAuditTurn struct {
	key   string
	body  bytes.Buffer
	model string
}
type wsAuditRecorder struct {
	svc    *service.UserRequestAuditService
	apiKey *service.APIKey
	userID int64
	mu     sync.Mutex
	turns  map[int]*wsAuditTurn
	active int
}

func newWSAuditRecorder(svc *service.UserRequestAuditService, apiKey *service.APIKey, userID int64) *wsAuditRecorder {
	if svc == nil || apiKey == nil {
		return nil
	}
	return &wsAuditRecorder{svc: svc, apiKey: apiKey, userID: userID, turns: make(map[int]*wsAuditTurn)}
}
func (r *wsAuditRecorder) Start(turn int, body []byte, model string) {
	if r == nil || turn <= 0 {
		return
	}
	var gid *int64
	name := ""
	if r.apiKey.GroupID != nil {
		v := *r.apiKey.GroupID
		gid = &v
	}
	if r.apiKey.Group != nil {
		name = r.apiKey.Group.Name
	}
	key := r.svc.Enqueue(service.UserRequestAuditCapture{UserID: r.userID, APIKeyID: r.apiKey.ID, GroupID: gid, GroupName: name, Protocol: "openai_responses_ws", Endpoint: "/openai/v1/responses", RequestedModel: model, PreviousResponseID: strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()), Body: body, Metadata: map[string]any{"transport": "websocket", "turn": turn}})
	r.mu.Lock()
	r.turns[turn] = &wsAuditTurn{key: key, model: model}
	r.active = turn
	r.mu.Unlock()
}
func (r *wsAuditRecorder) Observe(direction string, _ int, payload []byte) {
	if r == nil || len(payload) == 0 {
		return
	}
	r.mu.Lock()
	turn := r.active
	t := r.turns[turn]
	if t != nil && len(payload)+t.body.Len() <= userRequestAuditResponseLimit {
		_, _ = t.body.Write(payload)
		_, _ = t.body.Write([]byte("\n"))
	}
	r.mu.Unlock()
	if direction == "server" && openAIWSAuditTerminal(payload) {
		r.Finish(turn, nil)
	}
}
func (r *wsAuditRecorder) Finish(turn int, err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	t := r.turns[turn]
	delete(r.turns, turn)
	if r.active == turn {
		r.active = 0
	}
	var raw []byte
	if t != nil {
		raw = append([]byte(nil), t.body.Bytes()...)
	}
	r.mu.Unlock()
	if t == nil || t.key == "" {
		return
	}
	id, model := auditResponseFields(raw)
	if model == "" {
		model = t.model
	}
	if err == nil && !auditWSHasTerminal(raw) {
		err = errors.New("websocket turn ended before terminal response")
	}
	status := "completed"
	if err != nil {
		status = "failed"
	}
	r.svc.Complete(service.UserRequestAuditCompletion{LogicalKey: t.key, ResponseID: id, UpstreamModel: model, ResponseChatML: auditResponseChatML(raw), Status: status, LastError: auditErrorString(err), Metadata: map[string]any{"transport": "websocket", "response_bytes": len(raw)}})
}
func (r *wsAuditRecorder) FinishAll(err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	keys := make([]int, 0, len(r.turns))
	for turn := range r.turns {
		keys = append(keys, turn)
	}
	r.mu.Unlock()
	for _, turn := range keys {
		r.Finish(turn, err)
	}
}
func openAIWSAuditTerminal(payload []byte) bool {
	switch strings.TrimSpace(gjson.GetBytes(payload, "type").String()) {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		return true
	}
	return false
}
func auditStreamIncomplete(raw []byte) bool {
	if len(raw) == 0 {
		return true
	}
	if !bytes.Contains(raw, []byte("data:")) {
		return false
	}
	if bytes.Contains(raw, []byte("[DONE]")) || bytes.Contains(raw, []byte("message_stop")) {
		return false
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if openAIWSAuditTerminal(line) {
			return false
		}
	}
	return true
}
func auditWSHasTerminal(raw []byte) bool {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if openAIWSAuditTerminal(line) {
			return true
		}
	}
	return bytes.Contains(raw, []byte("[DONE]"))
}
