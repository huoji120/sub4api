package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/websearch"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const responsesWebSearchKeepaliveInterval = 15 * time.Second

// responsesWebSearchRelay streams every hosted-search model round to the client
// as it is generated, the way a server-side web_search tool behaves natively:
// one response.created, live item/text deltas with output_index renumbered
// across rounds, the internal search function replaced in place by a public
// web_search_call item, and a single terminal event written by the loop.
type responsesWebSearchRelay struct {
	client       gin.ResponseWriter
	functionName string
	publicTools  json.RawMessage
	publicChoice json.RawMessage
	maxTokens    json.RawMessage

	mu        sync.Mutex
	started   bool
	sequence  int
	publicID  string
	lastWrite time.Time
	writeErr  error
	stop      chan struct{}
	stopOnce  sync.Once
}

func newResponsesWebSearchRelay(client gin.ResponseWriter, plan *responsesWebSearchPlan, maxTokens json.RawMessage) *responsesWebSearchRelay {
	relay := &responsesWebSearchRelay{
		client:       client,
		functionName: plan.FunctionName,
		publicTools:  responsesWebSearchPublicTools(plan),
		publicChoice: plan.OriginalToolChoice,
		maxTokens:    maxTokens,
		stop:         make(chan struct{}),
	}
	if len(relay.publicChoice) == 0 {
		relay.publicChoice = json.RawMessage(`"auto"`)
	}
	go relay.keepalive()
	return relay
}

func (r *responsesWebSearchRelay) close() {
	r.stopOnce.Do(func() { close(r.stop) })
}

// keepalive writes SSE comments while a search or a slow upstream round is in
// progress, so proxies with idle timeouts (Cloudflare: 100s) keep the stream.
// It never commits headers: until the first event is relayed the request can
// still fail over to another account with a plain HTTP error.
func (r *responsesWebSearchRelay) keepalive() {
	ticker := time.NewTicker(responsesWebSearchKeepaliveInterval / 3)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.mu.Lock()
			if r.started && r.writeErr == nil && time.Since(r.lastWrite) >= responsesWebSearchKeepaliveInterval {
				r.writeLocked([]byte(": keepalive\n\n"))
			}
			r.mu.Unlock()
		}
	}
}

func (r *responsesWebSearchRelay) isStarted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started
}

// identity is the response id announced in response.created.
func (r *responsesWebSearchRelay) identity() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.publicID
}

func (r *responsesWebSearchRelay) clientErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeErr
}

func (r *responsesWebSearchRelay) writeLocked(data []byte) {
	if r.writeErr != nil {
		return
	}
	if _, err := r.client.Write(data); err != nil {
		r.writeErr = err
		return
	}
	r.client.Flush()
	r.lastWrite = time.Now()
}

func (r *responsesWebSearchRelay) commitLocked(header http.Header) {
	if r.started {
		return
	}
	copyResponsesWebSearchHeaders(r.client.Header(), header)
	r.client.Header().Set("Content-Type", "text/event-stream")
	r.client.Header().Set("Cache-Control", "no-cache")
	r.client.Header().Set("Connection", "keep-alive")
	r.client.Header().Set("X-Accel-Buffering", "no")
	r.client.WriteHeader(http.StatusOK)
	r.started = true
}

// emitLocked writes one event with the relay's own sequence numbering.
func (r *responsesWebSearchRelay) emitLocked(kind string, fields map[string]json.RawMessage) error {
	fields["type"] = rawResponsesSearchJSON(kind)
	fields["sequence_number"] = rawResponsesSearchJSON(r.sequence)
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	r.sequence++
	r.writeLocked([]byte(fmt.Sprintf("event: %s\ndata: %s\n\n", kind, data)))
	return r.writeErr
}

func (r *responsesWebSearchRelay) emit(kind string, fields map[string]any) error {
	encoded := make(map[string]json.RawMessage, len(fields))
	for key, value := range fields {
		encoded[key] = rawResponsesSearchJSON(value)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.emitLocked(kind, encoded)
}

// publicResponse hides the internal function and pins the public identity.
func (r *responsesWebSearchRelay) publicResponse(raw json.RawMessage) json.RawMessage {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return raw
	}
	if r.publicID != "" {
		object["id"] = rawResponsesSearchJSON(r.publicID)
	}
	object["tools"] = r.publicTools
	object["tool_choice"] = r.publicChoice
	if len(r.maxTokens) != 0 {
		object["max_output_tokens"] = r.maxTokens
	}
	return rawResponsesSearchJSON(object)
}

// round returns the writer for one model round. Output items of the round are
// renumbered from base, the number of public items already produced.
func (r *responsesWebSearchRelay) round(base int, sources []websearch.SearchResult) *responsesWebSearchRoundWriter {
	opening := !r.isStarted()
	return &responsesWebSearchRoundWriter{
		header: make(http.Header), status: http.StatusOK, size: -1,
		relay: &responsesWebSearchRoundRelay{relay: r, base: base, sources: sources, opening: opening, hidden: make(map[int]string), hiddenItems: make(map[string]bool)},
	}
}

// responsesWebSearchRoundRelay is the per-round SSE parser state.
type responsesWebSearchRoundRelay struct {
	relay       *responsesWebSearchRelay
	base        int
	sources     []websearch.SearchResult
	pending     []byte
	event       []byte
	streamed    bool
	opening     bool // this round opens the client stream (created/in_progress)
	hidden      map[int]string // round-local output_index -> public web_search_call id
	hiddenItems map[string]bool
}

func (rr *responsesWebSearchRoundRelay) searchID(localIndex int) (string, bool) {
	id, ok := rr.hidden[localIndex]
	return id, ok
}

func (rr *responsesWebSearchRoundRelay) feed(header http.Header, data []byte) {
	rr.pending = append(rr.pending, data...)
	for {
		newline := bytes.IndexByte(rr.pending, '\n')
		if newline < 0 {
			return
		}
		line := bytes.TrimRight(rr.pending[:newline], "\r")
		rr.pending = rr.pending[newline+1:]
		switch {
		case len(line) == 0:
			rr.dispatch(header)
		case bytes.HasPrefix(line, []byte("data:")):
			value := line[len("data:"):]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			if len(rr.event) > 0 {
				rr.event = append(rr.event, '\n')
			}
			rr.event = append(rr.event, value...)
		}
	}
}

func (rr *responsesWebSearchRoundRelay) dispatch(header http.Header) {
	payload := rr.event
	rr.event = nil
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	var event map[string]json.RawMessage
	if json.Unmarshal(payload, &event) != nil || event == nil {
		return
	}
	rr.streamed = true
	kind := responsesSearchString(event["type"])
	relay := rr.relay
	relay.mu.Lock()
	defer relay.mu.Unlock()
	switch kind {
	case "response.created", "response.in_progress", "response.queued":
		if !rr.opening {
			return
		}
		if relay.publicID == "" {
			relay.publicID = gjson.GetBytes(event["response"], "id").String()
		}
		relay.commitLocked(header)
		event["response"] = relay.publicResponse(event["response"])
		_ = relay.emitLocked(kind, event)
		return
	case "response.completed", "response.incomplete", "response.failed", "response.done", "response.cancelled", "response.canceled", "error":
		return // the hosted loop decides the single terminal event
	}
	if !relay.started {
		relay.commitLocked(header)
	}
	rawIndex, hasIndex := event["output_index"]
	var localIndex int
	if hasIndex && json.Unmarshal(rawIndex, &localIndex) != nil {
		hasIndex = false
	}
	itemID := responsesSearchString(event["item_id"])
	if kind == "response.output_item.added" && hasIndex {
		var item map[string]json.RawMessage
		if json.Unmarshal(event["item"], &item) == nil && responsesSearchString(item["type"]) == "function_call" && responsesSearchString(item["name"]) == relay.functionName {
			searchID := responsesWebSearchIDPrefix + uuid.NewString()
			rr.hidden[localIndex] = searchID
			if id := responsesSearchString(item["id"]); id != "" {
				rr.hiddenItems[id] = true
			}
			global := rr.base + localIndex
			_ = relay.emitLocked("response.output_item.added", map[string]json.RawMessage{
				"output_index": rawResponsesSearchJSON(global),
				"item":         rawResponsesSearchJSON(map[string]any{"type": "web_search_call", "id": searchID, "status": "in_progress"}),
			})
			_ = relay.emitLocked("response.web_search_call.in_progress", map[string]json.RawMessage{
				"output_index": rawResponsesSearchJSON(global), "item_id": rawResponsesSearchJSON(searchID),
			})
			return
		}
	}
	if (hasIndex && rr.hidden[localIndex] != "") || (itemID != "" && rr.hiddenItems[itemID]) {
		return // argument deltas/done of the internal function never escape
	}
	if hasIndex {
		event["output_index"] = rawResponsesSearchJSON(rr.base + localIndex)
	}
	if kind == "response.output_item.done" && len(rr.sources) > 0 {
		items := []json.RawMessage{event["item"]}
		addResponsesWebSearchCitations(items, rr.sources)
		event["item"] = items[0]
	}
	_ = relay.emitLocked(kind, event)
}

// emitRoundItems replays a round that arrived as one JSON object instead of SSE
// (an adapter fallback), using the same per-item framing as the buffered path.
func (r *responsesWebSearchRelay) emitRoundItems(header http.Header, response map[string]json.RawMessage, base int, items []json.RawMessage, skip func(int) bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	emit := func(kind string, fields map[string]any) error {
		encoded := make(map[string]json.RawMessage, len(fields))
		for key, value := range fields {
			encoded[key] = rawResponsesSearchJSON(value)
		}
		return r.emitLocked(kind, encoded)
	}
	if !r.started {
		r.commitLocked(header)
		r.publicID = responsesSearchString(response["id"])
		initial := responsesSearchCopy(response)
		initial["status"] = json.RawMessage(`"in_progress"`)
		initial["output"] = json.RawMessage(`[]`)
		initial["usage"] = json.RawMessage(`null`)
		initial["error"] = json.RawMessage(`null`)
		initial["incomplete_details"] = json.RawMessage(`null`)
		published := r.publicResponse(rawResponsesSearchJSON(initial))
		for _, kind := range []string{"response.created", "response.in_progress"} {
			if err := r.emitLocked(kind, map[string]json.RawMessage{"response": published}); err != nil {
				return err
			}
		}
	}
	for index, raw := range items {
		if skip(index) {
			continue
		}
		if err := emitResponsesWebSearchItem(emit, base+index, raw); err != nil {
			return err
		}
	}
	return r.writeErr
}

// startSearchCall announces a public web_search_call that was not announced
// while streaming (non-SSE round), then marks it as searching.
func (r *responsesWebSearchRelay) startSearchCall(index int, searchID string, announced bool) {
	if !announced {
		_ = r.emit("response.output_item.added", map[string]any{"output_index": index, "item": map[string]any{"type": "web_search_call", "id": searchID, "status": "in_progress"}})
		_ = r.emit("response.web_search_call.in_progress", map[string]any{"output_index": index, "item_id": searchID})
	}
}

func (r *responsesWebSearchRelay) searching(index int, searchID string) {
	_ = r.emit("response.web_search_call.searching", map[string]any{"output_index": index, "item_id": searchID})
}

// finishSearchCall closes a public web_search_call with its final state.
func (r *responsesWebSearchRelay) finishSearchCall(index int, searchID string, call json.RawMessage) {
	if gjson.GetBytes(call, "status").String() == "completed" {
		_ = r.emit("response.web_search_call.completed", map[string]any{"output_index": index, "item_id": searchID})
	}
	_ = r.emit("response.output_item.done", map[string]any{"output_index": index, "item": call})
}

// terminal writes the single terminal event of the whole hosted exchange.
func (r *responsesWebSearchRelay) terminal(status string, response json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !strings.HasPrefix(status, "response.") {
		status = "response." + status
	}
	return r.emitLocked(status, map[string]json.RawMessage{"response": r.publicResponse(response)})
}

func responsesWebSearchPublicTools(plan *responsesWebSearchPlan) json.RawMessage {
	var tools []json.RawMessage
	if len(plan.OriginalTools) != 0 {
		_ = json.Unmarshal(plan.OriginalTools, &tools)
	}
	hasSearch := false
	for _, tool := range tools {
		if responsesSearchBuiltin(gjson.GetBytes(tool, "type").String()) {
			hasSearch = true
		}
	}
	if !hasSearch {
		tools = append(tools, json.RawMessage(`{"type":"web_search"}`))
	}
	return rawResponsesSearchJSON(tools)
}
