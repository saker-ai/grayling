// Package api exposes GoLab's HTTP surface (docs/技术方案.md §11):
//
//	/api/v1/admin/**   control plane — admin tokens + object-level RBAC
//	/api/v1/sdk/config scoped snapshot per SDK token (ETag/304)
//	/api/v1/sdk/eval   remote evaluation for thin clients
//	/api/v1/sdk/stream SSE revision invalidation
//	/api/v1/collect    exposure/conversion ingestion (collect tokens)
//	/api/v1/health     liveness
//
// Security posture (§9): authentication happens first and the
// workspace/environment of every request is derived from the presented
// credential — never from query parameters or body fields. Admin, SDK and
// collect credentials are distinct audiences and are rejected on each
// other's endpoints.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/godeps/golab/internal/admin"
	aisvc "github.com/godeps/golab/internal/ai"
	"github.com/godeps/golab/internal/configstore"
	"github.com/godeps/golab/internal/platform"
	"github.com/godeps/golab/pkg/model"
)

// Server wires the HTTP surface.
type Server struct {
	Admin   *admin.Service
	Store   *platform.Store
	Config  configstore.Store
	Hub     *SSEHub
	limiter *rateLimiter

	// ai is optional; when nil, AI endpoints answer 501.
	ai *aisvc.Service

	snapshotCache sync.Map // namespaceID -> *evaluatorCacheEntry

	logger *log.Logger
}

type cachedSnapshot struct {
	revision int64
	etag     string
	body     []byte
	snapshot *model.Snapshot
}

// Options configures server construction.
type Options struct {
	Logger *log.Logger
	// RequestsPerSecond bounds per-token request rate on sdk/collect
	// endpoints (0 = default 200).
	RequestsPerSecond float64
}

// NewServer assembles the API. The admin service listener is wired to the
// SSE hub here so publications fan out immediately.
func NewServer(store *platform.Store, cfg configstore.Store, opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = log.New(io.Discard, "", 0)
	}
	rps := opts.RequestsPerSecond
	if rps <= 0 {
		rps = 200
	}
	s := &Server{
		Store:   store,
		Config:  cfg,
		Hub:     NewSSEHub(),
		limiter: newRateLimiter(rps, int(rps)),
		logger:  opts.Logger,
	}
	s.Admin = admin.NewService(store, cfg, func(wsID, envID string, rev int64) {
		s.Hub.Notify(wsID, envID, rev)
	})
	return s
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/health", s.handleHealth)

	// Admin (admin tokens only).
	mux.HandleFunc("POST /api/v1/admin/workspaces", s.adminOnly(s.handleCreateWorkspace))
	mux.HandleFunc("GET /api/v1/admin/workspaces", s.adminOnly(s.handleListWorkspaces))
	mux.HandleFunc("POST /api/v1/admin/workspaces/{ws}/environments", s.adminOnly(s.handleCreateEnvironment))
	mux.HandleFunc("GET /api/v1/admin/workspaces/{ws}/environments", s.adminOnly(s.handleListEnvironments))
	mux.HandleFunc("POST /api/v1/admin/workspaces/{ws}/tokens", s.adminOnly(s.handleCreateToken))
	mux.HandleFunc("GET /api/v1/admin/workspaces/{ws}/tokens", s.adminOnly(s.handleListTokens))
	mux.HandleFunc("DELETE /api/v1/admin/tokens/{id}", s.adminOnly(s.handleRevokeToken))
	mux.HandleFunc("GET /api/v1/admin/workspaces/{ws}/audit", s.adminOnly(s.handleAudit))
	mux.HandleFunc("PUT /api/v1/admin/workspaces/{ws}/quotas", s.adminOnly(s.handleSetQuotas))

	mux.HandleFunc("GET /api/v1/admin/workspaces/{ws}/environments/{env}/flags", s.adminOnly(s.handleListFlags))
	mux.HandleFunc("GET /api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}", s.adminOnly(s.handleGetFlag))
	mux.HandleFunc("PUT /api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}/draft", s.adminOnly(s.handleSaveDraft))
	mux.HandleFunc("POST /api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}/submit", s.adminOnly(s.handleSubmitDraft))
	mux.HandleFunc("POST /api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}/approve", s.adminOnly(s.handleApproveDraft))
	mux.HandleFunc("POST /api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}/reject", s.adminOnly(s.handleRejectDraft))
	mux.HandleFunc("POST /api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}/publish", s.adminOnly(s.handlePublishFlag))
	mux.HandleFunc("POST /api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}/rollback", s.adminOnly(s.handleRollback))
	mux.HandleFunc("DELETE /api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}", s.adminOnly(s.handleMarkDeleted))
	mux.HandleFunc("POST /api/v1/admin/workspaces/{ws}/environments/{env}/flags/{key}/import-gff", s.adminOnly(s.handleImportGff))

	mux.HandleFunc("GET /api/v1/admin/workspaces/{ws}/environments/{env}/segments", s.adminOnly(s.handleListSegments))
	mux.HandleFunc("PUT /api/v1/admin/workspaces/{ws}/environments/{env}/segments/{key}/draft", s.adminOnly(s.handleSaveSegmentDraft))
	mux.HandleFunc("POST /api/v1/admin/workspaces/{ws}/environments/{env}/segments/{key}/submit", s.adminOnly(s.handleSubmitSegment))
	mux.HandleFunc("POST /api/v1/admin/workspaces/{ws}/environments/{env}/segments/{key}/approve", s.adminOnly(s.handleApproveSegment))
	mux.HandleFunc("POST /api/v1/admin/workspaces/{ws}/environments/{env}/segments/{key}/publish", s.adminOnly(s.handlePublishSegment))

	mux.HandleFunc("POST /api/v1/admin/workspaces/{ws}/environments/{env}/experiments", s.adminOnly(s.handleCreateExperiment))
	mux.HandleFunc("GET /api/v1/admin/workspaces/{ws}/environments/{env}/experiments", s.adminOnly(s.handleListExperiments))
	mux.HandleFunc("GET /api/v1/admin/experiments/{id}", s.adminOnly(s.handleGetExperiment))
	mux.HandleFunc("POST /api/v1/admin/experiments/{id}/review", s.adminOnly(s.handleReviewExperiment))
	mux.HandleFunc("POST /api/v1/admin/experiments/{id}/bind", s.adminOnly(s.handleBindExperiment))
	mux.HandleFunc("POST /api/v1/admin/experiments/{id}/transition", s.adminOnly(s.handleTransitionExperiment))
	mux.HandleFunc("GET /api/v1/admin/experiments/{id}/report", s.adminOnly(s.handleExperimentReport))

	mux.HandleFunc("POST /api/v1/admin/workspaces/{ws}/environments/{env}/debug/eval", s.adminOnly(s.handleDebugEval))

	// AI resource governance.
	mux.HandleFunc("POST /api/v1/admin/workspaces/{ws}/environments/{env}/ai/resources", s.adminOnly(s.handleRegisterAIResource))
	mux.HandleFunc("GET /api/v1/admin/workspaces/{ws}/environments/{env}/ai/resources", s.adminOnly(s.handleListAIResources))
	mux.HandleFunc("POST /api/v1/admin/workspaces/{ws}/environments/{env}/ai/resources/{type}/{logical}/pin", s.adminOnly(s.handlePinAIVersion))
	mux.HandleFunc("POST /api/v1/admin/workspaces/{ws}/environments/{env}/ai/resources/{type}/{logical}/revoke", s.adminOnly(s.handleRevokeAIResource))
	mux.HandleFunc("POST /api/v1/admin/workspaces/{ws}/environments/{env}/ai/resources/{type}/{logical}/reinstate", s.adminOnly(s.handleReinstateAIResource))
	mux.HandleFunc("GET /api/v1/admin/workspaces/{ws}/environments/{env}/ai/resolve", s.adminOnly(s.handleResolveAIResource))
	mux.HandleFunc("PUT /api/v1/admin/workspaces/{ws}/model-allowlist", s.adminOnly(s.handleSetModelAllowlist))
	mux.HandleFunc("GET /api/v1/admin/workspaces/{ws}/model-allowlist", s.adminOnly(s.handleGetModelAllowlist))

	// SDK data plane (sdk tokens).
	mux.HandleFunc("GET /api/v1/sdk/config", s.sdkOnly(s.handleSDKConfig))
	mux.HandleFunc("POST /api/v1/sdk/eval", s.sdkOnly(s.handleSDKEval))
	mux.HandleFunc("GET /api/v1/sdk/stream", s.sdkOnly(s.handleSDKStream))
	mux.HandleFunc("POST /api/v1/sdk/ai/resolve", s.sdkOnly(s.handleSDKAIResolve))
	mux.HandleFunc("POST /api/v1/sdk/ai/session", s.sdkOnly(s.handleSDKAISession))
	mux.HandleFunc("POST /api/v1/sdk/ai/check", s.sdkOnly(s.handleSDKAICheck))

	// Collection (collect tokens).
	mux.HandleFunc("POST /api/v1/collect", s.collectOnly(s.handleCollect))

	return mux
}

// ---------------------------------------------------------------------------
// Auth plumbing
// ---------------------------------------------------------------------------

type ctxKey int

const (
	ctxToken ctxKey = iota
	ctxActor
)

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, kinds ...platform.TokenKind) (*platform.Token, bool) {
	secret := bearerToken(r)
	if secret == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
		return nil, false
	}
	tok, err := s.Store.ResolveToken(secret)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or revoked token")
		return nil, false
	}
	for _, k := range kinds {
		if tok.Kind == k {
			return tok, true
		}
	}
	writeError(w, http.StatusForbidden, "wrong_audience",
		fmt.Sprintf("%s tokens are not valid on this endpoint", tok.Kind))
	return nil, false
}

// adminOnly authenticates an admin token and attaches the Actor.
func (s *Server) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(adminLimiterKey(r)) {
			writeError(w, http.StatusTooManyRequests, "rate_limited", "admin request rate exceeded")
			return
		}
		tok, ok := s.authenticate(w, r, platform.TokenAdmin)
		if !ok {
			return
		}
		actor := admin.Actor{
			TokenID: tok.ID, Name: tok.Name, Kind: tok.Kind,
			Role: tok.Role, WorkspaceID: tok.WorkspaceID,
		}
		ctx := context.WithValue(r.Context(), ctxActor, actor)
		next(w, r.WithContext(ctx))
	}
}

func actorFrom(r *http.Request) admin.Actor {
	a, _ := r.Context().Value(ctxActor).(admin.Actor)
	return a
}

func (s *Server) sdkOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := s.authenticate(w, r, platform.TokenSDK)
		if !ok {
			return
		}
		if !s.limiter.allow("sdk:" + tok.ID) {
			writeError(w, http.StatusTooManyRequests, "rate_limited", "sdk request rate exceeded")
			return
		}
		ctx := context.WithValue(r.Context(), ctxToken, tok)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) collectOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := s.authenticate(w, r, platform.TokenCollect)
		if !ok {
			return
		}
		if !s.limiter.allow("collect:" + tok.ID) {
			writeError(w, http.StatusTooManyRequests, "rate_limited", "collect rate exceeded")
			return
		}
		ctx := context.WithValue(r.Context(), ctxToken, tok)
		next(w, r.WithContext(ctx))
	}
}

func tokenFrom(r *http.Request) *platform.Token {
	t, _ := r.Context().Value(ctxToken).(*platform.Token)
	return t
}

func adminLimiterKey(r *http.Request) string {
	if t := bearerToken(r); t != "" {
		if len(t) > 16 {
			return "admin:" + t[len(t)-16:]
		}
		return "admin:" + t
	}
	return "admin:anon"
}

// ---------------------------------------------------------------------------
// JSON helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		log.Printf("api: encode response: %v", err)
	}
}

type apiError struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Error: errorBody{Code: code, Message: msg}})
}

// writeStoreError maps platform/configstore sentinel errors to HTTP statuses.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, platform.ErrNotFound), errors.Is(err, configstore.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, platform.ErrExists):
		writeError(w, http.StatusConflict, "exists", err.Error())
	case errors.Is(err, platform.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, platform.ErrInvalidState):
		writeError(w, http.StatusConflict, "invalid_state", err.Error())
	case errors.Is(err, platform.ErrAccessDenied):
		writeError(w, http.StatusForbidden, "access_denied", err.Error())
	case errors.Is(err, platform.ErrQuotaExceeded):
		writeError(w, http.StatusTooManyRequests, "quota_exceeded", err.Error())
	case errors.Is(err, platform.ErrSelfApproval):
		writeError(w, http.StatusForbidden, "self_approval_denied", err.Error())
	case errors.Is(err, platform.ErrValidation):
		writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
	default:
		var verr *model.ValidationError
		if errors.As(err, &verr) {
			writeJSON(w, http.StatusBadRequest, apiError{Error: errorBody{
				Code: "validation_failed", Message: verr.Error(),
			}})
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func decodeJSON(r *http.Request, dst any, maxBytes int64) error {
	body := http.MaxBytesReader(nil, r.Body, maxBytes)
	defer r.Body.Close()
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: %v", platform.ErrValidation, err)
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": "golab", "schemaVersion": model.SchemaVersion,
	})
}

// ---------------------------------------------------------------------------
// Simple per-key fixed-window rate limiter (bounded memory).
// ---------------------------------------------------------------------------

type rateLimiter struct {
	mu      sync.Mutex
	rps     float64
	burst   int
	windows map[string]*window
}

type window struct {
	start time.Time
	count int
}

func newRateLimiter(rps float64, burst int) *rateLimiter {
	return &rateLimiter{rps: rps, burst: burst, windows: map[string]*window{}}
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	span := time.Duration(float64(time.Second) * float64(l.burst) / l.rps)
	if span < time.Second {
		span = time.Second
	}
	w, ok := l.windows[key]
	if !ok || now.Sub(w.start) >= span {
		// Opportunistic cleanup keeps the map bounded without a sweeper.
		if len(l.windows) > 4096 {
			for k, v := range l.windows {
				if now.Sub(v.start) >= span {
					delete(l.windows, k)
				}
			}
		}
		l.windows[key] = &window{start: now, count: 1}
		return true
	}
	w.count++
	return w.count <= l.burst
}
