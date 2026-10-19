package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/pkg/model"
	"google.golang.org/protobuf/proto"
)

const resourcePrefix = "/api/v1/agent/resources/"

type subscription struct {
	key      string
	kind     agentv1.ResourceKind
	revision uint64
	sequence uint64
	active   bool
}

func scopeMatches(scope *agentv1.Scope, se Session) bool {
	return scope == nil || scope.WorkspaceId == se.Grant.Scope.WorkspaceID && scope.EnvironmentId == se.Grant.Scope.EnvironmentID && scope.ServiceKey == se.Grant.Scope.ServiceKey
}
func resourceReference(host string, se Session, kind agentv1.ResourceKind, key string, resource StoredResource) *agentv1.ResourceRef {
	scope := se.Grant.Scope
	expires := resource.ExpiresAt
	if se.Grant.ExpiresAt.Before(expires) {
		expires = se.Grant.ExpiresAt
	}
	return &agentv1.ResourceRef{Scope: &agentv1.Scope{WorkspaceId: scope.WorkspaceID, EnvironmentId: scope.EnvironmentID, ServiceKey: scope.ServiceKey}, Kind: kind, Key: key, Revision: resource.Revision, HttpsUrl: "https://" + host + resourcePrefix + strconv.Itoa(int(kind)) + "/" + url.PathEscape(key) + "/" + strconv.FormatUint(resource.Revision, 10), Sha256: hex.EncodeToString(resource.Hash[:]), SizeBytes: uint64(len(resource.Body)), ExpiresUnixMs: expires.UnixMilli()}
}
func resourceMessage(host string, se Session, request *agentv1.Envelope, kind agentv1.ResourceKind, key string, resource StoredResource, snapshot bool) *agentv1.Envelope {
	ref := resourceReference(host, se, kind, key, resource)
	var e *agentv1.Envelope
	if snapshot {
		body := &agentv1.Snapshot{Scope: ref.Scope, Revision: resource.Revision, Content: &agentv1.Snapshot_Reference{Reference: ref}}
		if len(resource.Body) <= 192<<10 {
			r := new(agentv1.SignedResource)
			if err := proto.Unmarshal(resource.Body, r); err == nil {
				body.Content = &agentv1.Snapshot_InlineResource{InlineResource: r}
			}
		}
		e = response(se, request.MessageId, body)
	} else {
		e = response(se, request.MessageId, ref)
	}
	e.SubscriptionId = request.SubscriptionId
	e.ResourceKind = ref.Kind
	e.ResourceKey = key
	e.ResourceRevision = resource.Revision
	return e
}
func (c *Channel) serveResourceHTTP(w http.ResponseWriter, r *http.Request, g Grant) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, resourcePrefix), "/")
	if len(parts) != 3 || (parts[0] != "1" && parts[0] != "2" && parts[0] != "3" && parts[0] != "4" && parts[0] != "5") || !identifier(parts[1]) {
		http.NotFound(w, r)
		return
	}
	revision, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil || revision == 0 || len(r.Header.Values("X-Grayling-Agent-Session")) != 1 || len(r.Header.Values("X-Grayling-Agent-Epoch")) != 1 {
		http.Error(w, "invalid agent resource request", 400)
		return
	}
	epoch, err := strconv.ParseUint(r.Header.Get("X-Grayling-Agent-Epoch"), 10, 64)
	if err != nil {
		http.Error(w, "invalid agent session", 403)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	kind := agentv1.ResourceKind_RESOURCE_KIND_POLICY
	if parts[0] == "2" {
		kind = agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY
	} else if parts[0] == "3" {
		kind = agentv1.ResourceKind_RESOURCE_KIND_BUNDLE
	} else if parts[0] == "4" {
		kind = agentv1.ResourceKind_RESOURCE_KIND_REVOCATION
	} else if parts[0] == "5" {
		kind = agentv1.ResourceKind_RESOURCE_KIND_ACTIVATION
	}
	resource, err := c.readResource(ctx, Session{ID: r.Header.Get("X-Grayling-Agent-Session"), Epoch: epoch, Grant: g}, kind, parts[1], revision)
	cancel()
	if err != nil {
		status := http.StatusServiceUnavailable
		if pe, ok := errors.AsType[*agentv1.ProtocolError](err); ok {
			switch pe.Code {
			case agentv1.ErrorCode_ERROR_CODE_NOT_READY:
				status = 404
			case agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED:
				status = 410
			default:
				status = 403
			}
		}
		http.Error(w, "agent resource unavailable", status)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Header().Set("Content-Length", strconv.Itoa(len(resource.Body)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A slow download cannot hold a session row lock or the WebSocket writer.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, _ = w.Write(resource.Body)
}

// ResourceHeads performs one bounded metadata query for all local subscriptions.
// Polling the authority closes notification gaps without requiring a sticky node.
func (s *Sessions) ResourceHeads(ctx context.Context, se Session, keys []string) (map[string]uint64, error) {
	if len(keys) > 128 {
		return nil, denied()
	}
	for _, key := range keys {
		if !model.KeyPattern.MatchString(key) {
			return nil, denied()
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := lockSession(ctx, tx, se, ConfigRead); err != nil {
		return nil, err
	}
	scope := se.Grant.Scope
	rows, err := tx.Query(ctx, `SELECT h.resource_key,h.revision::text FROM agent_resource_heads h JOIN agent_resources r USING(workspace_id,environment_id,service_key,resource_key,revision) WHERE h.workspace_id=$1 AND h.environment_id=$2 AND h.service_key=$3 AND h.resource_key=ANY($4) AND r.expires_at>clock_timestamp()`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]uint64)
	for rows.Next() {
		var key, revision string
		if err := rows.Scan(&key, &revision); err != nil {
			return nil, err
		}
		n, err := strconv.ParseUint(revision, 10, 64)
		if err != nil {
			return nil, err
		}
		result[key] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// authorizeSubscription never queries resource existence before checking scope
// and capability. Resume tokens are unsupported and require a fresh snapshot.
func (c *Channel) authorizeSubscription(se Session, e *agentv1.Envelope) (string, error) {
	p := e.GetSubscribe()
	if p == nil || !scopeMatches(p.Scope, se) {
		return "", denied()
	}
	if p.Cursor == nil {
		return "", protocolError(agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE, "resource capability unavailable")
	}
	switch p.Cursor.Kind {
	case agentv1.ResourceKind_RESOURCE_KIND_POLICY:
		if se.Grant.Permissions&ConfigRead == 0 {
			return "", denied()
		}
	case agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY:
		if se.Grant.Permissions&DiscoveryRead == 0 {
			return "", denied()
		}
		if c.discovery == nil {
			return "", protocolError(agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE, "discovery unavailable")
		}
		if _, err := discoveryBundle(p.Cursor.Key); err != nil {
			return "", err
		}
	default:
		return "", protocolError(agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE, "resource capability unavailable")
	}
	if !identifier(p.Cursor.Key) || e.SubscriptionId == "" || e.ResourceKind != 0 && e.ResourceKind != p.Cursor.Kind || e.ResourceKey != "" && e.ResourceKey != p.Cursor.Key {
		return "", protocolError(agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid subscription")
	}
	return p.Cursor.Key, nil
}
func resourceError(err error) *agentv1.Error {
	e := publicError(err)
	if e.Code == agentv1.ErrorCode_ERROR_CODE_NOT_READY {
		e.Retryable = true
		e.RetryAfterMs = 1000
	}
	return e
}
