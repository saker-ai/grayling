package agent

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"golang.org/x/time/rate"
	"google.golang.org/protobuf/proto"
)

// ChannelOptions requires an explicit Host allowlist and per-node bound. Empty
// Origin is the supported machine-client policy; browser Origins are rejected.
type ChannelOptions struct {
	AllowedHosts       []string
	MaxConnections     int
	DiscoveryPublisher *Publisher
	// ReleaseVerifier is independently provisioned; nil disables preparation.
	ReleaseVerifier *client.Verifier
}
type Channel struct {
	sessions        *Sessions
	discovery       *Publisher
	releaseVerifier *client.Verifier
	hosts           []string
	slots           chan struct{}
	downloads       chan struct{}
	stop            chan struct{}
	done            chan struct{}
	mu              sync.Mutex
	closed          bool
	wg              sync.WaitGroup
}

// Validate checks deployment bounds without opening storage or listeners.
func (opts ChannelOptions) Validate() error {
	if len(opts.AllowedHosts) == 0 || opts.MaxConnections < 1 || opts.MaxConnections > 100000 || len(opts.AllowedHosts) > 128 {
		return errors.New("agent: explicit channel host and connection limits required")
	}
	for _, host := range opts.AllowedHosts {
		if host == "" || strings.ContainsAny(host, "/*\\\r\n\t ") {
			return errors.New("agent: invalid allowed host")
		}
	}
	return nil
}
func NewChannel(s *Sessions, opts ChannelOptions) (*Channel, error) {
	if s == nil {
		return nil, errors.New("agent: shared sessions required")
	}
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if opts.DiscoveryPublisher != nil && opts.DiscoveryPublisher.sessions != s {
		return nil, errors.New("agent: discovery publisher must share channel sessions")
	}
	hosts := slices.Clone(opts.AllowedHosts)
	for i, host := range hosts {
		hosts[i] = strings.ToLower(host)
	}
	return &Channel{sessions: s, discovery: opts.DiscoveryPublisher, releaseVerifier: opts.ReleaseVerifier, hosts: hosts, slots: make(chan struct{}, opts.MaxConnections), downloads: make(chan struct{}, 16), stop: make(chan struct{}), done: make(chan struct{})}, nil
}
func (c *Channel) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.stop)
		go func() { c.wg.Wait(); close(c.done) }()
	}
	c.mu.Unlock()
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *Channel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Credentials are never accepted from query strings, cookies or proxy scheme
	// headers. TLS must terminate at this listener; Host is an exact allowlist.
	if r.Method != http.MethodGet || r.URL.Path != agentv1.ConnectPath && !strings.HasPrefix(r.URL.Path, resourcePrefix) {
		http.NotFound(w, r)
		return
	}
	if r.TLS == nil || !slices.Contains(c.hosts, strings.ToLower(r.Host)) || len(r.Header.Values("Origin")) > 0 || len(r.Header.Values("Cookie")) > 0 || r.URL.RawQuery != "" || r.URL.ForceQuery {
		http.Error(w, "agent request rejected", http.StatusForbidden)
		return
	}
	if len(r.Header.Values("Authorization")) != 1 {
		http.Error(w, "agent credential required", http.StatusUnauthorized)
		return
	}
	secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		http.Error(w, "agent credential required", http.StatusUnauthorized)
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		http.Error(w, "agent shutting down", http.StatusServiceUnavailable)
		return
	}
	slots := c.slots
	if strings.HasPrefix(r.URL.Path, resourcePrefix) {
		slots = c.downloads
	}
	select {
	case slots <- struct{}{}:
	default:
		c.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		http.Error(w, "agent capacity exceeded", http.StatusTooManyRequests)
		return
	}
	c.wg.Add(1)
	c.mu.Unlock()
	defer c.wg.Done()
	defer func() { <-slots }()
	authCtx, authCancel := context.WithTimeout(r.Context(), 2*time.Second)
	grant, err := c.sessions.Authenticate(authCtx, secret)
	authCancel()
	if err != nil {
		status := http.StatusServiceUnavailable
		var pe *agentv1.ProtocolError
		if errors.As(err, &pe) {
			status = http.StatusUnauthorized
		}
		http.Error(w, "agent authentication failed", status)
		return
	}
	if strings.HasPrefix(r.URL.Path, resourcePrefix) {
		c.serveResourceHTTP(w, r, grant)
		return
	}
	offered := false
	for _, value := range r.Header.Values("Sec-WebSocket-Protocol") {
		for candidate := range strings.SplitSeq(value, ",") {
			if strings.TrimSpace(candidate) == agentv1.Subprotocol {
				offered = true
			}
		}
	}
	if !offered {
		http.Error(w, "agent subprotocol required", http.StatusBadRequest)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentv1.Subprotocol}, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(agentv1.MaxFrameBytes)
	c.serve(r.Context(), conn, secret, r.Host)
}

type inbound struct {
	envelope *agentv1.Envelope
	err      error
}
type outbound struct {
	envelope *agentv1.Envelope
	size     int64
	ack      chan error
}

func response(se Session, reply string, payload any) *agentv1.Envelope {
	id, err := randomID()
	if err != nil {
		id = "server-response"
	}
	e := &agentv1.Envelope{ProtocolMajor: agentv1.ProtocolMajor, ProtocolMinor: agentv1.ProtocolMinor, SessionId: se.ID, ConnectionEpoch: se.Epoch, MessageId: id, ReplyTo: reply}
	switch p := payload.(type) {
	case *agentv1.Revoke:
		e.Payload = &agentv1.Envelope_Revoke{Revoke: p}
	case *agentv1.Prepare:
		e.Payload = &agentv1.Envelope_Prepare{Prepare: p}
	case *agentv1.PrepareRejectedAck:
		e.Payload = &agentv1.Envelope_PrepareRejectedAck{PrepareRejectedAck: p}
	case *agentv1.Activate:
		e.Payload = &agentv1.Envelope_Activate{Activate: p}
	case *agentv1.ActivationReportAck:
		e.Payload = &agentv1.Envelope_ActivationReportAck{ActivationReportAck: p}
	case *agentv1.ReleaseStatus:
		e.Payload = &agentv1.Envelope_ReleaseStatus{ReleaseStatus: p}
	case *agentv1.PreparedAck:
		e.Payload = &agentv1.Envelope_PreparedAck{PreparedAck: p}
	case *agentv1.Welcome:
		e.Payload = &agentv1.Envelope_Welcome{Welcome: p}
	case *agentv1.Registered:
		e.Payload = &agentv1.Envelope_Registered{Registered: p}
	case *agentv1.LeaseRenewed:
		e.Payload = &agentv1.Envelope_LeaseRenewed{LeaseRenewed: p}
	case *agentv1.InstanceChanged:
		e.Payload = &agentv1.Envelope_InstanceChanged{InstanceChanged: p}
	case *agentv1.HeartbeatAck:
		e.Payload = &agentv1.Envelope_HeartbeatAck{HeartbeatAck: p}
	case *agentv1.Error:
		e.Payload = &agentv1.Envelope_Error{Error: p}
	case *agentv1.Snapshot:
		e.Payload = &agentv1.Envelope_Snapshot{Snapshot: p}
	case *agentv1.ResourceRef:
		e.Payload = &agentv1.Envelope_ResourceRef{ResourceRef: p}
	case *agentv1.Invalidated:
		e.Payload = &agentv1.Envelope_Invalidated{Invalidated: p}
	case *agentv1.ResyncRequired:
		e.Payload = &agentv1.Envelope_ResyncRequired{ResyncRequired: p}
	case *agentv1.GoAway:
		e.Payload = &agentv1.Envelope_GoAway{GoAway: p}
	}
	return e
}
func publicError(err error) *agentv1.Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &agentv1.Error{Code: agentv1.ErrorCode_ERROR_CODE_DEADLINE_EXCEEDED, Reason: "command deadline exceeded"}
	}
	if pe, ok := errors.AsType[*agentv1.ProtocolError](err); ok {
		retryable := pe.Retryable || pe.Code == agentv1.ErrorCode_ERROR_CODE_RATE_LIMITED
		after := pe.RetryAfterMs
		if retryable && after == 0 {
			after = 1000
		}
		return &agentv1.Error{Code: pe.Code, Reason: pe.Reason, Retryable: retryable, RetryAfterMs: after}
	}
	return &agentv1.Error{Code: agentv1.ErrorCode_ERROR_CODE_BACKEND_UNAVAILABLE, Reason: "agent backend unavailable", Retryable: true, RetryAfterMs: 1000}
}
func (c *Channel) serve(parent context.Context, conn *websocket.Conn, secret string, host string) {
	ctx, cancel := context.WithCancel(parent)
	var workers sync.WaitGroup
	defer func() { cancel(); conn.CloseNow(); workers.Wait() }()
	incoming := make(chan inbound)
	outgoing := make(chan outbound, 64)
	resources := make(chan outbound, 128)
	var queueBytes atomic.Int64
	workers.Go(func() {
		for {
			readCtx, done := context.WithTimeout(ctx, 30*time.Second)
			typ, b, err := conn.Read(readCtx)
			done()
			var e *agentv1.Envelope
			if err == nil {
				if typ != websocket.MessageBinary {
					err = protocolError(agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE, "binary protobuf message required")
				} else {
					e, err = agentv1.Decode(b)
				}
			}
			select {
			case incoming <- inbound{e, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	})
	workers.Go(func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			default:
				var item outbound
				select {
				case item = <-outgoing:
				default:
					select {
					case <-ctx.Done():
						return
					case item = <-outgoing:
					case item = <-resources:
					}
				}
				queueBytes.Add(-item.size)
				b, err := agentv1.Encode(item.envelope)
				if err == nil {
					writeCtx, done := context.WithTimeout(ctx, 2*time.Second)
					err = conn.Write(writeCtx, websocket.MessageBinary, b)
					done()
				}
				if item.ack != nil {
					item.ack <- err
				}
				if err != nil {
					return
				}
			}
		}
	})
	send := func(e *agentv1.Envelope, wait bool) bool {
		size := int64(proto.Size(e))
		if size > agentv1.MaxFrameBytes {
			return false
		}
		queue := outgoing
		byteLimit := int64(8 << 20)
		switch e.Payload.(type) {
		case *agentv1.Envelope_Snapshot, *agentv1.Envelope_ResourceRef, *agentv1.Envelope_Invalidated, *agentv1.Envelope_ResyncRequired:
			queue = resources
			byteLimit -= 256 << 10
		}
		// Low-priority resources cannot consume the bytes reserved for control/lease.
		if queueBytes.Add(size) > byteLimit {
			queueBytes.Add(-size)
			return false
		}
		item := outbound{envelope: e, size: size}
		if wait {
			item.ack = make(chan error, 1)
		}
		select {
		case queue <- item:
		default:
			queueBytes.Add(-size)
			return false
		}
		if !wait {
			return true
		}
		timer := time.NewTimer(2500 * time.Millisecond)
		defer timer.Stop()
		select {
		case err := <-item.ack:
			return err == nil
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		}
	}
	var se Session
	fail := func(reply string, err error) { send(response(se, reply, publicError(err)), true) }
	helloTimer := time.NewTimer(5 * time.Second)
	defer helloTimer.Stop()
	var first inbound
	select {
	case first = <-incoming:
	case <-ctx.Done():
		return
	case <-c.stop:
		return
	case <-helloTimer.C:
		fail("", protocolError(agentv1.ErrorCode_ERROR_CODE_DEADLINE_EXCEEDED, "hello timeout"))
		return
	}
	if first.err != nil {
		fail("", first.err)
		return
	}
	h := first.envelope.GetHello()
	if h == nil {
		fail(first.envelope.MessageId, protocolError(agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE, "hello required"))
		return
	}
	capabilities := []string{"session"}
	authCtx, authDone := context.WithTimeout(ctx, 2*time.Second)
	grant, authErr := c.sessions.Authenticate(authCtx, secret)
	authDone()
	if authErr != nil {
		fail(first.envelope.MessageId, authErr)
		return
	}
	if grant.Permissions&ConfigRead != 0 {
		capabilities = append(capabilities, "config.read", "bundle.read")
	}
	if c.discovery != nil && grant.Permissions&DiscoveryRead != 0 {
		capabilities = append(capabilities, "discovery.read")
	}
	if grant.Permissions&InstanceRegister != 0 {
		capabilities = append(capabilities, "instance.register")
	}
	if grant.Permissions&InstanceRenew != 0 {
		capabilities = append(capabilities, "instance.renew")
	}
	if c.releaseVerifier != nil && grant.Permissions&(ConfigRead|ReleaseReport) == ConfigRead|ReleaseReport {
		capabilities = append(capabilities, "release.prepare", "release.prepare.reject")
	}
	prepareEnabled := slices.Contains(capabilities, "release.prepare") && (slices.Contains(h.Capabilities, "release.prepare") || slices.Contains(h.RequiredCapabilities, "release.prepare"))
	rejectEnabled := prepareEnabled && (slices.Contains(h.Capabilities, "release.prepare.reject") || slices.Contains(h.RequiredCapabilities, "release.prepare.reject"))
	if c.releaseVerifier != nil && grant.Permissions&(ConfigRead|DiscoveryRead|ReleaseReport) == ConfigRead|DiscoveryRead|ReleaseReport {
		capabilities = append(capabilities, "release.activation.read", "release.activate")
	}
	if c.releaseVerifier != nil && grant.Permissions&ConfigRead != 0 {
		capabilities = append(capabilities, "release.revoke")
	}
	revokeEnabled := slices.Contains(capabilities, "release.revoke") && (slices.Contains(h.Capabilities, "release.revoke") || slices.Contains(h.RequiredCapabilities, "release.revoke"))
	activateEnabled := slices.Contains(capabilities, "release.activate") && (slices.Contains(h.Capabilities, "release.activate") || slices.Contains(h.RequiredCapabilities, "release.activate"))
	for _, capability := range h.RequiredCapabilities {
		if !slices.Contains(capabilities, capability) {
			fail(first.envelope.MessageId, protocolError(agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE, "required capability unavailable"))
			return
		}
	}
	openCtx, done := context.WithTimeout(ctx, 2*time.Second)
	var err error
	se, err = c.sessions.Connect(openCtx, secret, h)
	done()
	if err != nil {
		fail(first.envelope.MessageId, err)
		return
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer done()
		_ = c.sessions.Disconnect(cleanup, se)
	}()
	stateCtx, stateDone := context.WithTimeout(ctx, 2*time.Second)
	controlEpoch, serverTime, err := c.sessions.ServerState(stateCtx)
	stateDone()
	if err != nil {
		fail(first.envelope.MessageId, err)
		return
	}
	welcome := &agentv1.Welcome{Scope: &agentv1.Scope{WorkspaceId: se.Grant.Scope.WorkspaceID, EnvironmentId: se.Grant.Scope.EnvironmentID, ServiceKey: se.Grant.Scope.ServiceKey}, ServerUnixMs: serverTime.UnixMilli(), ControlPlaneEpoch: controlEpoch, Capabilities: capabilities, Limits: &agentv1.Limits{MaxFrameBytes: agentv1.MaxFrameBytes, HeartbeatIntervalMs: 10000, HeartbeatTimeoutMs: 30000, LeaseTtlMs: 60000, LeaseRenewIntervalMs: 20000, MaxSubscriptions: 128, MaxResourceBytes: 64 << 20, ResourceQueueItems: 128, ControlQueueItems: 64, QueueBytes: 8 << 20, MessagesPerSecond: 50}}
	if !send(response(se, first.envelope.MessageId, welcome), true) {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	limiter := rate.NewLimiter(50, 100)
	subscriptions := make(map[string]subscription)
	var deliveredPrepare string
	var deliveredActivation string
	var deliveredRevocation uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stop:
			send(response(se, "", &agentv1.GoAway{Reason: "server shutdown", RetryAfterMs: 1000, CloseUnixMs: time.Now().Add(2 * time.Second).UnixMilli()}), true)
			return
		case <-ticker.C:
			checkCtx, done := context.WithTimeout(ctx, 2*time.Second)
			err := c.sessions.CheckSession(checkCtx, se)
			done()
			if err != nil {
				fail("", err)
				return
			}
			if revokeEnabled {
				pollCtx, pollDone := context.WithTimeout(ctx, 2*time.Second)
				next, pollErr := c.revocationUpdate(pollCtx, se, host, deliveredRevocation)
				pollDone()
				if pollErr != nil {
					fail("", pollErr)
					return
				}
				if next != nil {
					if !send(next, false) {
						return
					}
					deliveredRevocation = next.GetRevoke().View.Revision
				}
			}
			if prepareEnabled {
				pollCtx, pollDone := context.WithTimeout(ctx, 2*time.Second)
				next, pollErr := c.prepareUpdate(pollCtx, se, host, deliveredPrepare)
				pollDone()
				if pollErr != nil {
					fail("", pollErr)
					return
				}
				if next != nil {
					if !send(next, false) {
						return
					}
					deliveredPrepare = next.OperationId
				}
			}
			if activateEnabled {
				pollCtx, pollDone := context.WithTimeout(ctx, 2*time.Second)
				next, pollErr := c.activationUpdate(pollCtx, se, host, deliveredActivation)
				pollDone()
				if pollErr != nil {
					fail("", pollErr)
					return
				}
				if next != nil {
					if !send(next, false) {
						return
					}
					deliveredActivation = next.OperationId
				}
			}
			if len(subscriptions) > 0 {
				keys := make([]string, 0, len(subscriptions))
				for _, sub := range subscriptions {
					if !sub.active || sub.kind != agentv1.ResourceKind_RESOURCE_KIND_POLICY {
						continue
					}
					keys = append(keys, sub.key)
				}
				heads := make(map[string]uint64)
				if len(keys) > 0 {
					readCtx, readDone := context.WithTimeout(ctx, 2*time.Second)
					heads, err = c.sessions.ResourceHeads(readCtx, se, keys)
					readDone()
				}
				if err != nil {
					fail("", err)
					return
				}
				for id, sub := range subscriptions {
					if !sub.active {
						continue
					}
					if sub.kind == agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY {
						next, updated, pollErr := c.discoveryUpdate(ctx, se, host, id, sub)
						if pollErr != nil {
							failure := response(se, "", resourceError(pollErr))
							failure.SubscriptionId = id
							if !send(failure, false) {
								return
							}
						} else if next != nil {
							if !send(next, false) {
								return
							}
							subscriptions[id] = updated
						}
						continue
					}
					if revision := heads[sub.key]; revision > sub.revision {
						sub.sequence++
						notification := response(se, "", &agentv1.Invalidated{Revision: revision})
						notification.SubscriptionId = id
						notification.ResourceKind = agentv1.ResourceKind_RESOURCE_KIND_POLICY
						notification.ResourceKey = sub.key
						notification.ResourceRevision = revision
						notification.StreamSeq = sub.sequence
						if !send(notification, false) {
							return
						}
						sub.revision = revision
						subscriptions[id] = sub
					}
				}
			}
		case item := <-incoming:
			if item.err != nil {
				fail("", item.err)
				return
			}
			e := item.envelope
			if !limiter.Allow() {
				fail(e.MessageId, protocolError(agentv1.ErrorCode_ERROR_CODE_RATE_LIMITED, "message rate limit"))
				return
			}
			if e.SessionId != se.ID || e.ConnectionEpoch != se.Epoch {
				fail(e.MessageId, fenced())
				return
			}
			failCommand := func(err error) {
				reply := response(se, e.MessageId, publicError(err))
				if e.GetSubscribe() != nil {
					reply.ReplyTo = ""
					reply.SubscriptionId = e.SubscriptionId
				}
				send(reply, true)
			}
			now := time.Now()
			if e.DeadlineUnixMs != 0 && (e.DeadlineUnixMs <= now.UnixMilli() || e.DeadlineUnixMs > now.Add(120*time.Second).UnixMilli()) {
				failCommand(protocolError(agentv1.ErrorCode_ERROR_CODE_DEADLINE_EXCEEDED, "invalid command deadline"))
				continue
			}
			deadline := commandDeadline(e, now)
			checkCtx, done := context.WithDeadline(ctx, deadline)
			if e.GetHeartbeat() != nil {
				err = c.sessions.Heartbeat(checkCtx, se)
			} else {
				err = c.sessions.CheckSession(checkCtx, se)
			}
			done()
			if err != nil {
				failCommand(err)
				if errors.Is(err, context.DeadlineExceeded) {
					continue
				}
				return
			}

			if query := e.GetReleaseStatusQuery(); query != nil {
				if !activateEnabled {
					failCommand(protocolError(agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE, "activation capability unavailable"))
					continue
				}
				statusCtx, statusDone := context.WithDeadline(ctx, deadline)
				status, statusErr := c.sessions.ActivationStatus(statusCtx, se, query)
				statusDone()
				var reply *agentv1.Envelope
				if statusErr != nil {
					reply = response(se, e.MessageId, publicError(statusErr))
				} else {
					reply = response(se, e.MessageId, status)
				}
				if !send(reply, false) {
					return
				}
				continue
			}
			if e.GetApplied() != nil || e.GetRejected() != nil && strings.HasPrefix(e.OperationId, "activate-") {
				if !activateEnabled {
					failCommand(protocolError(agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE, "activation capability unavailable"))
					continue
				}
				reportCtx, reportDone := context.WithDeadline(ctx, deadline)
				reply := c.activationCommand(reportCtx, se, e)
				reportDone()
				if !send(reply, false) {
					return
				}
				continue
			}
			if e.GetPrepared() != nil || e.GetRejected() != nil {
				if !prepareEnabled || e.GetRejected() != nil && !rejectEnabled {
					failCommand(protocolError(agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE, "preparation capability unavailable"))
					continue
				}
				reportCtx, reportDone := context.WithDeadline(ctx, deadline)
				reply := c.preparedCommand(reportCtx, se, e)
				reportDone()
				if !send(reply, false) {
					return
				}
				continue
			}

			registrationCtx, registrationDone := context.WithDeadline(ctx, deadline)
			reply, handled := c.registrationCommand(registrationCtx, se, e)
			registrationDone()
			if handled {
				if !send(reply, false) {
					return
				}
				continue
			}

			switch p := e.Payload.(type) {
			case *agentv1.Envelope_Heartbeat:
				if !send(response(se, e.MessageId, &agentv1.HeartbeatAck{Nonce: p.Heartbeat.Nonce, ServerUnixMs: time.Now().UTC().UnixMilli()}), false) {
					return
				}
			case *agentv1.Envelope_Subscribe:
				key, err := c.authorizeSubscription(se, e)
				asyncError := func(err error) bool {
					reply := response(se, "", resourceError(err))
					reply.SubscriptionId = e.SubscriptionId
					return send(reply, false)
				}
				if err != nil {
					if !asyncError(err) {
						return
					}
					continue
				}
				if _, exists := subscriptions[e.SubscriptionId]; !exists && len(subscriptions) >= 128 {
					if !asyncError(protocolError(agentv1.ErrorCode_ERROR_CODE_RATE_LIMITED, "subscription limit")) {
						return
					}
					continue
				}
				if p.Subscribe.Cursor.Kind == agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY {
					conflict := false
					for id, sub := range subscriptions {
						if id != e.SubscriptionId && sub.active && sub.kind == agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY {
							conflict = true
							break
						}
					}
					if conflict {
						if !asyncError(protocolError(agentv1.ErrorCode_ERROR_CODE_RATE_LIMITED, "one discovery subscription per connection")) {
							return
						}
						continue
					}
				}
				previous := subscriptions[e.SubscriptionId]
				if previous.active && (previous.key != key || previous.kind != p.Subscribe.Cursor.Kind) {
					if !asyncError(protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "subscription resource changed")) {
						return
					}
					continue
				}
				resourceCtx, resourceDone := context.WithDeadline(ctx, deadline)
				resource, err := c.readResource(resourceCtx, se, p.Subscribe.Cursor.Kind, key, 0)
				resourceDone()
				if err != nil {
					if !asyncError(err) {
						return
					}
					continue
				}
				sub := subscription{kind: p.Subscribe.Cursor.Kind, key: key, sequence: previous.sequence, active: true}
				sub.revision = resource.Revision
				sub.sequence++
				subscriptions[e.SubscriptionId] = sub
				snapshot := resourceMessage(host, se, e, p.Subscribe.Cursor.Kind, key, resource, true)
				snapshot.StreamSeq = sub.sequence
				snapshot.ReplyTo = ""
				if !send(snapshot, false) {
					return
				}
			case *agentv1.Envelope_Unsubscribe:
				if sub, ok := subscriptions[e.SubscriptionId]; ok {
					sub.active = false
					subscriptions[e.SubscriptionId] = sub
				}
			case *agentv1.Envelope_Resume:
				resync := response(se, "", &agentv1.ResyncRequired{Reason: "full snapshot required"})
				resync.SubscriptionId = e.SubscriptionId
				if !send(resync, false) {
					return
				}
			case *agentv1.Envelope_FetchResource:
				if e.ResourceKind != 0 && e.ResourceKind != p.FetchResource.Kind || e.ResourceKey != "" && e.ResourceKey != p.FetchResource.Key {
					if !send(response(se, e.MessageId, publicError(protocolError(agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "resource metadata mismatch"))), false) {
						return
					}
					continue
				}

				resourceCtx, resourceDone := context.WithDeadline(ctx, deadline)
				resource, err := c.readResource(resourceCtx, se, p.FetchResource.Kind, p.FetchResource.Key, p.FetchResource.Revision)
				resourceDone()
				if err != nil {
					if !send(response(se, e.MessageId, resourceError(err)), false) {
						return
					}
					continue
				}
				if !send(resourceMessage(host, se, e, p.FetchResource.Kind, p.FetchResource.Key, resource, false), false) {
					return
				}
			case *agentv1.Envelope_Goodbye:
				return
			default:
				if !send(response(se, e.MessageId, &agentv1.Error{Code: agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE, Reason: "message capability unavailable"}), false) {
					return
				}
			}
		}
	}
}
