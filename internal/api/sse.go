package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// SSEHub fans out manifest-revision invalidation notices to subscribed SDKs
// (§3: GoLab 对外 SSE 发送 revision/失效通知，客户端重拉完整快照).
//
// Semantics:
//   - Events carry id = manifest revision so reconnects can send
//     Last-Event-ID; the hub always replays the CURRENT revision on
//     connect, which covers expired-log cases with a full resync (§3:
//     若日志已过期则全量重同步 — there is no partial log by design).
//   - Notifications are best-effort acceleration; SDKs additionally poll,
//     so a lost event never causes permanent staleness.
//   - Per-environment connection cap (quota, §11).
type SSEHub struct {
	mu          sync.RWMutex
	subscribers map[string]map[chan *sseEvent]struct{} // scopeKey → channels
	current     map[string]int64                       // scopeKey → latest revision
	maxPerScope int
}

type sseEvent struct {
	Revision int64  `json:"revision"`
	Scope    string `json:"scope"`
}

// NewSSEHub creates the hub. maxPerScope bounds SSE connections per
// (workspace, environment) — default 256.
func NewSSEHub() *SSEHub {
	return &SSEHub{
		subscribers: map[string]map[chan *sseEvent]struct{}{},
		current:     map[string]int64{},
		maxPerScope: 256,
	}
}

func sseScope(wsID, envID string) string { return wsID + "/" + envID }

// Notify publishes a new manifest revision to all subscribers of the scope.
// Called from the admin publish pipeline AFTER read-back confirmation.
func (h *SSEHub) Notify(wsID, envID string, revision int64) {
	scope := sseScope(wsID, envID)
	h.mu.Lock()
	if revision > h.current[scope] {
		h.current[scope] = revision
	}
	subs := make([]chan *sseEvent, 0, len(h.subscribers[scope]))
	for ch := range h.subscribers[scope] {
		subs = append(subs, ch)
	}
	h.mu.Unlock()

	ev := &sseEvent{Revision: revision, Scope: scope}
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
			// Slow consumer: drop the notification. The SDK's polling
			// fallback converges (§3), so dropping is safe and keeps one
			// stuck client from blocking the publish path.
		}
	}
}

// Subscribe registers a channel for one scope and returns the current known
// revision plus an unsubscribe function.
func (h *SSEHub) Subscribe(wsID, envID string) (<-chan *sseEvent, int64, func(), error) {
	scope := sseScope(wsID, envID)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subscribers[scope]) >= h.maxPerScope {
		return nil, 0, nil, fmt.Errorf("sse connection cap (%d) reached for scope", h.maxPerScope)
	}
	ch := make(chan *sseEvent, 16)
	if h.subscribers[scope] == nil {
		h.subscribers[scope] = map[chan *sseEvent]struct{}{}
	}
	h.subscribers[scope][ch] = struct{}{}
	unsubscribe := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if set, ok := h.subscribers[scope]; ok {
			delete(set, ch)
		}
	}
	return ch, h.current[scope], unsubscribe, nil
}

// SetCurrent primes the hub's revision memory (used at startup so late
// subscribers immediately learn the published revision).
func (h *SSEHub) SetCurrent(wsID, envID string, revision int64) {
	scope := sseScope(wsID, envID)
	h.mu.Lock()
	defer h.mu.Unlock()
	if revision > h.current[scope] {
		h.current[scope] = revision
	}
}

// handleSDKStream serves GET /api/v1/sdk/stream.
func (s *Server) handleSDKStream(w http.ResponseWriter, r *http.Request) {
	tok := tokenFrom(r)
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "response writer cannot stream")
		return
	}
	ch, current, unsubscribe, err := s.Hub.Subscribe(tok.WorkspaceID, tok.EnvironmentID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "sse_cap", err.Error())
		return
	}
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Always send the current revision immediately: covers both fresh
	// connects and Last-Event-ID reconnects (full-resync semantics; the
	// client compares against its cached revision and re-pulls if newer).
	lastID := r.Header.Get("Last-Event-ID")
	_ = lastID // current-revision replay subsumes it; header acknowledged for observability
	if err := writeSSE(w, flusher, current, tok.WorkspaceID+"/"+tok.EnvironmentID); err != nil {
		return
	}

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if err := writeSSE(w, flusher, ev.Revision, ev.Scope); err != nil {
				return
			}
		case <-heartbeat.C:
			if err := writeSSEComment(w, flusher); err != nil {
				return
			}
		}
	}
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, revision int64, scope string) error {
	data, _ := json.Marshal(sseEvent{Revision: revision, Scope: scope})
	if _, err := fmt.Fprintf(w, "id: %d\nevent: revision\ndata: %s\n\n", revision, data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeSSEComment(w http.ResponseWriter, flusher http.Flusher) error {
	if _, err := fmt.Fprintf(w, ": ping %s\n\n", strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
