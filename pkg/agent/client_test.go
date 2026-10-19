package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

func writePeer(t *testing.T, ctx context.Context, c *websocket.Conn, e *agentv1.Envelope) {
	t.Helper()
	b, err := agentv1.Encode(e)
	if err != nil {
		t.Error(err)
		return
	}
	_ = c.Write(ctx, websocket.MessageBinary, b)
}

func serverOptions(t *testing.T, modify func(*agentv1.Envelope), handle func(context.Context, *websocket.Conn, *agentv1.Envelope)) Options {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != agentv1.ConnectPath || r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", 401)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{agentv1.Subprotocol}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		_, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		hello, err := agentv1.Decode(b)
		if err != nil {
			t.Error(err)
			return
		}
		welcome := &agentv1.Envelope{ProtocolMajor: 1, SessionId: "session", ConnectionEpoch: 2, MessageId: "welcome", ReplyTo: hello.MessageId, Payload: &agentv1.Envelope_Welcome{Welcome: &agentv1.Welcome{Scope: &agentv1.Scope{WorkspaceId: "ws", EnvironmentId: "env", ServiceKey: "svc"}, ControlPlaneEpoch: 1, ServerUnixMs: time.Now().UnixMilli(), Limits: &agentv1.Limits{MaxFrameBytes: agentv1.MaxFrameBytes, MaxResourceBytes: MaxResourceBytes, HeartbeatIntervalMs: 20, HeartbeatTimeoutMs: 100}}}}
		if modify != nil {
			modify(welcome)
		}
		writePeer(t, ctx, c, welcome)
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			e, err := agentv1.Decode(b)
			if err != nil {
				t.Error(err)
				return
			}
			if h := e.GetHeartbeat(); h != nil {
				writePeer(t, ctx, c, &agentv1.Envelope{ProtocolMajor: 1, SessionId: "session", ConnectionEpoch: 2, MessageId: "beat", ReplyTo: e.MessageId, Payload: &agentv1.Envelope_HeartbeatAck{HeartbeatAck: &agentv1.HeartbeatAck{Nonce: h.Nonce, ServerUnixMs: time.Now().UnixMilli()}}})
				continue
			}
			if handle != nil {
				handle(ctx, c, e)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return Options{URL: "wss" + strings.TrimPrefix(srv.URL, "https"), Token: "test-token", HTTPClient: srv.Client(), Hello: &agentv1.Hello{AgentId: "agent", InstanceId: "instance", BootId: "boot"}, RequestTimeout: time.Second}
}

func request(t *testing.T, body proto.Message) *agentv1.Envelope {
	t.Helper()
	e, err := Message(body)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func connect(t *testing.T, o Options) *Client {
	t.Helper()
	c, err := Dial(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func peerReply(e *agentv1.Envelope, body *agentv1.ReleaseStatus) *agentv1.Envelope {
	return &agentv1.Envelope{ProtocolMajor: 1, SessionId: "session", ConnectionEpoch: 2, MessageId: "reply", ReplyTo: e.MessageId, Payload: &agentv1.Envelope_ReleaseStatus{ReleaseStatus: body}}
}

func TestConcurrentCallsAndHeartbeat(t *testing.T) {
	var beats atomic.Int64
	o := serverOptions(t, nil, func(ctx context.Context, c *websocket.Conn, e *agentv1.Envelope) {
		if e.GetReleaseStatusQuery() != nil {
			beats.Add(1)
			writePeer(t, ctx, c, peerReply(e, &agentv1.ReleaseStatus{Release: &agentv1.ReleaseIdentity{ReleaseId: e.GetReleaseStatusQuery().ReleaseId}}))
		}
	})
	c := connect(t, o)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			e := request(t, &agentv1.ReleaseStatusQuery{ReleaseId: "release"})
			response, err := c.Call(t.Context(), e)
			if err != nil {
				t.Error(err)
				return
			}
			if response.GetReleaseStatus().GetRelease().GetReleaseId() != "release" {
				t.Error("mismatched response")
			}
			if e.SessionId != "" {
				t.Error("caller message mutated")
			}
		})
	}
	wg.Wait()
	time.Sleep(60 * time.Millisecond)
	if beats.Load() != 50 || c.Err() != nil {
		t.Fatalf("calls=%d err=%v", beats.Load(), c.Err())
	}
	copy := c.Welcome()
	copy.SessionId = "mutated"
	if c.Welcome().SessionId != "session" {
		t.Fatal("shared welcome")
	}
}

func TestCloseUnblocksCalls(t *testing.T) {
	received := make(chan struct{}, 1)
	c := connect(t, serverOptions(t, nil, func(context.Context, *websocket.Conn, *agentv1.Envelope) { received <- struct{}{} }))
	result := make(chan error, 1)
	go func() { _, err := c.Call(t.Context(), request(t, &agentv1.ReleaseStatusQuery{})); result <- err }()
	<-received
	_ = c.Close()
	_ = c.Close()
	select {
	case err := <-result:
		if !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("call leaked")
	}
	for range c.Events() {
	}
}

func TestRequestTimeoutDoesNotPoisonSession(t *testing.T) {
	c := connect(t, serverOptions(t, nil, nil))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, err := c.Call(ctx, request(t, &agentv1.ReleaseStatusQuery{}))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if c.Err() != nil {
		t.Fatal(c.Err())
	}
}

func TestMutationAndDirectionValidation(t *testing.T) {
	c := connect(t, serverOptions(t, nil, nil))
	if err := c.Send(t.Context(), request(t, &agentv1.Register{})); err == nil {
		t.Fatal("missing operation accepted")
	}
	if _, err := Message(&agentv1.Activate{}); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
	var typedNil *agentv1.Register
	if _, err := Message(typedNil); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
}

func TestWelcomeValidation(t *testing.T) {
	tests := map[string]func(*agentv1.Envelope){
		"correlation": func(e *agentv1.Envelope) { e.ReplyTo = "wrong" },
		"scope":       func(e *agentv1.Envelope) { e.GetWelcome().Scope = nil },
		"heartbeat":   func(e *agentv1.Envelope) { e.GetWelcome().Limits.HeartbeatTimeoutMs = 1 },
		"frame":       func(e *agentv1.Envelope) { e.GetWelcome().Limits.MaxFrameBytes = agentv1.MaxFrameBytes + 1 },
	}
	for name, modify := range tests {
		t.Run(name, func(t *testing.T) {
			o := serverOptions(t, modify, nil)
			if c, err := Dial(t.Context(), o); err == nil {
				_ = c.Close()
				t.Fatal("accepted invalid welcome")
			}
		})
	}
}

func TestParentCancellation(t *testing.T) {
	o := serverOptions(t, nil, nil)
	ctx, cancel := context.WithCancel(t.Context())
	c, err := Dial(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cancel()
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("cancellation leaked")
	}
	if !errors.Is(c.Err(), context.Canceled) {
		t.Fatal(c.Err())
	}
}

func TestFencingAndEventBackpressure(t *testing.T) {
	for _, mode := range []string{"fencing", "backpressure", "text"} {
		t.Run(mode, func(t *testing.T) {
			o := serverOptions(t, nil, func(ctx context.Context, conn *websocket.Conn, e *agentv1.Envelope) {
				if mode == "text" {
					_ = conn.Write(ctx, websocket.MessageText, []byte("{}"))
					return
				}
				for i := range 3 {
					push := &agentv1.Envelope{ProtocolMajor: 1, SessionId: "session", ConnectionEpoch: 2, MessageId: "push", Payload: &agentv1.Envelope_Invalidated{Invalidated: &agentv1.Invalidated{Revision: uint64(i + 1)}}}
					if mode == "fencing" {
						push.ConnectionEpoch = 1
					}
					writePeer(t, ctx, conn, push)
				}
			})
			o.EventCapacity = 1
			c := connect(t, o)
			_ = c.Send(t.Context(), request(t, &agentv1.Subscribe{}))
			select {
			case <-c.Done():
			case <-time.After(time.Second):
				t.Fatal("session did not stop")
			}
			want := ErrProtocol
			if mode == "backpressure" {
				want = ErrBackpressure
			}
			if !errors.Is(c.Err(), want) {
				t.Fatalf("got %v want %v", c.Err(), want)
			}
		})
	}
}

func TestRemoteError(t *testing.T) {
	o := serverOptions(t, nil, func(ctx context.Context, c *websocket.Conn, e *agentv1.Envelope) {
		reply := peerReply(e, nil)
		reply.Payload = &agentv1.Envelope_Error{Error: &agentv1.Error{Code: agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, Reason: "secret"}}
		writePeer(t, ctx, c, reply)
	})
	c := connect(t, o)
	_, err := c.Call(t.Context(), request(t, &agentv1.ReleaseStatusQuery{}))
	remote, ok := errors.AsType[*RemoteError](err)
	if !ok || remote.Detail.Reason != "secret" || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
}

func TestRedirectDoesNotForwardToken(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer target.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer origin.Close()
	o := Options{URL: "wss" + strings.TrimPrefix(origin.URL, "https"), Token: "private-token", HTTPClient: origin.Client(), Hello: &agentv1.Hello{AgentId: "a", InstanceId: "i", BootId: "b"}}
	_, err := Dial(t.Context(), o)
	if err == nil || leaked.Load() || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("err=%v leaked=%v", err, leaked.Load())
	}
}

func TestHeartbeatTimeout(t *testing.T) {
	o := serverOptions(t, func(e *agentv1.Envelope) {
		e.GetWelcome().Limits.HeartbeatIntervalMs = 5
		e.GetWelcome().Limits.HeartbeatTimeoutMs = 15
	}, func(ctx context.Context, c *websocket.Conn, e *agentv1.Envelope) {
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	})
	c := connect(t, o)
	if err := c.SendMessage(t.Context(), &agentv1.ReleaseStatusQuery{}, Metadata{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("heartbeat timeout not enforced")
	}
	if c.Err() == nil {
		t.Fatal("missing heartbeat failure")
	}
}

func TestHeartbeatDoesNotWaitForPendingSlots(t *testing.T) {
	o := serverOptions(t, nil, nil)
	o.MaxPending = 1
	c := connect(t, o)
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	_, err := c.Call(ctx, request(t, &agentv1.ReleaseStatusQuery{}))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if c.Err() != nil {
		t.Fatal(c.Err())
	}
}

func TestTypedOperations(t *testing.T) {
	seen := make(chan *agentv1.Envelope, 16)
	o := serverOptions(t, func(e *agentv1.Envelope) { e.GetWelcome().Limits.MaxEventsPerBatch = 10 }, func(ctx context.Context, c *websocket.Conn, e *agentv1.Envelope) {
		seen <- e
		reply := peerReply(e, &agentv1.ReleaseStatus{})
		switch e.Payload.(type) {
		case *agentv1.Envelope_ReleaseStatusQuery:
			reply.Payload = &agentv1.Envelope_ReleaseStatus{ReleaseStatus: &agentv1.ReleaseStatus{Release: &agentv1.ReleaseIdentity{ReleaseId: e.GetReleaseStatusQuery().ReleaseId, Generation: 1, BundleHash: strings.Repeat("a", 64)}, State: agentv1.ReleaseState_RELEASE_STATE_ACTIVATING, ReleaseRevision: 3, ActivationArtifactRevision: 1, RequiredSources: 1, PendingSourceIds: []string{"source"}}}
		case *agentv1.Envelope_Register:
			reply.Payload = &agentv1.Envelope_Registered{Registered: &agentv1.Registered{Instance: e.GetRegister().Instance}}
		case *agentv1.Envelope_RenewLease:
			reply.Payload = &agentv1.Envelope_LeaseRenewed{LeaseRenewed: &agentv1.LeaseRenewed{Lease: e.GetRenewLease().Lease}}
		case *agentv1.Envelope_FetchResource:
			reply.Payload = &agentv1.Envelope_ResourceRef{ResourceRef: &agentv1.ResourceRef{Revision: 7}}
		case *agentv1.Envelope_EventBatch:
			reply.Payload = &agentv1.Envelope_EventBatchAck{EventBatchAck: &agentv1.EventBatchAck{Results: []*agentv1.EventResult{{EventId: "event", Accepted: true}}}}
		}
		writePeer(t, ctx, c, reply)
	})
	c := connect(t, o)
	registered, err := c.Register(t.Context(), "register-operation", &agentv1.Register{Instance: &agentv1.Instance{BootId: "boot"}})
	if err != nil || registered.GetInstance().GetBootId() != "boot" {
		t.Fatalf("%v %v", registered, err)
	}
	if e := <-seen; e.OperationId != "register-operation" || e.ConnectionEpoch != 2 {
		t.Fatal("lost operation or fencing")
	}
	renewed, err := c.RenewLease(t.Context(), "renew-operation", &agentv1.RenewLease{Lease: &agentv1.Lease{Epoch: 3}})
	if err != nil || renewed.GetLease().GetEpoch() != 3 {
		t.Fatalf("%v %v", renewed, err)
	}
	<-seen
	resource, err := c.FetchResource(t.Context(), &agentv1.FetchResource{Key: "policy"})
	if err != nil || resource.Revision != 7 {
		t.Fatalf("%v %v", resource, err)
	}
	<-seen
	if _, err := c.ReleaseStatus(t.Context(), &agentv1.ReleaseStatusQuery{ReleaseId: "release-1"}); err != nil {
		t.Fatal(err)
	}
	<-seen
	ack, err := c.Collect(t.Context(), &agentv1.EventBatch{Events: []*agentv1.Event{{EventId: "event"}}})
	if err != nil || len(ack.Results) != 1 {
		t.Fatalf("%v %v", ack, err)
	}
	<-seen
	if err := c.Subscribe(t.Context(), "sub", &agentv1.Cursor{Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: "p", Revision: 2}); err != nil {
		t.Fatal(err)
	}
	if e := <-seen; e.SubscriptionId != "sub" || e.GetSubscribe().Scope.ServiceKey != "svc" {
		t.Fatal("subscription scope lost")
	}
	if err := c.Resume(t.Context(), "sub", []byte("cursor")); err != nil {
		t.Fatal(err)
	}
	<-seen
	if err := c.Unsubscribe(t.Context(), "sub"); err != nil {
		t.Fatal(err)
	}
	<-seen
	if err := c.SendMessage(t.Context(), &agentv1.Applied{Release: &agentv1.ReleaseIdentity{Generation: 5}}, Metadata{OperationID: "applied"}); err != nil {
		t.Fatal(err)
	}
	if e := <-seen; e.GetApplied().Release.Generation != 5 {
		t.Fatal("applied lost")
	}
}

func TestGoAwayTransfersEventOwnership(t *testing.T) {
	o := serverOptions(t, nil, func(ctx context.Context, c *websocket.Conn, e *agentv1.Envelope) {
		reply := peerReply(e, nil)
		reply.ReplyTo = ""
		reply.Payload = &agentv1.Envelope_GoAway{GoAway: &agentv1.GoAway{RetryAfterMs: 1200}}
		writePeer(t, ctx, c, reply)
	})
	c := connect(t, o)
	if err := c.SendMessage(t.Context(), &agentv1.ReleaseStatusQuery{}, Metadata{}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-c.Events():
		event.GetGoAway().RetryAfterMs = 0
	case <-time.After(time.Second):
		t.Fatal("missing goaway")
	}
	<-c.Done()
	away, ok := errors.AsType[*GoAwayError](c.Err())
	if !ok || away.RetryAfter != 1200*time.Millisecond {
		t.Fatal(c.Err())
	}
}

func TestServerClockBudget(t *testing.T) {
	for _, offset := range []time.Duration{-6 * time.Second, 6 * time.Second} {
		t.Run(offset.String(), func(t *testing.T) {
			options := serverOptions(t, func(e *agentv1.Envelope) { e.GetWelcome().ServerUnixMs = time.Now().Add(offset).UnixMilli() }, nil)
			if c, err := Dial(t.Context(), options); !errors.Is(err, ErrProtocol) {
				if c != nil {
					c.Close()
				}
				t.Fatalf("server clock outside budget accepted: %v", err)
			}
		})
	}
}

func TestReplyImmediatelyBeforeDisconnect(t *testing.T) {
	for i := range 20 {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			options := serverOptions(t, nil, func(ctx context.Context, conn *websocket.Conn, request *agentv1.Envelope) {
				reply := peerReply(request, nil)
				reply.Payload = &agentv1.Envelope_Error{Error: &agentv1.Error{Code: agentv1.ErrorCode_ERROR_CODE_FENCED, Reason: "superseded"}}
				writePeer(t, ctx, conn, reply)
				_ = conn.CloseNow()
			})
			c := connect(t, options)
			_, err := c.CallMessage(t.Context(), &agentv1.ReleaseStatusQuery{}, Metadata{})
			remote, ok := errors.AsType[*RemoteError](err)
			if !ok || remote.Detail.Code != agentv1.ErrorCode_ERROR_CODE_FENCED {
				t.Fatalf("correlated NACK lost to close: %v", err)
			}
		})
	}
}

func TestTypedActivationReceiptsRejectUnboundAcknowledgments(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*agentv1.Envelope)
	}{
		{"valid", func(*agentv1.Envelope) {}},
		{"wrong command", func(e *agentv1.Envelope) { e.GetActivationReportAck().CommandId = "other" }},
		{"wrong release", func(e *agentv1.Envelope) { e.GetActivationReportAck().Release.BundleHash = strings.Repeat("b", 64) }},
		{"wrong outcome", func(e *agentv1.Envelope) { e.GetActivationReportAck().Rejected = true }},
		{"premature active", func(e *agentv1.Envelope) {
			e.GetActivationReportAck().State = agentv1.ReleaseState_RELEASE_STATE_ACTIVE
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := serverOptions(t, nil, func(ctx context.Context, c *websocket.Conn, e *agentv1.Envelope) {
				ack := &agentv1.ActivationReportAck{Release: proto.Clone(e.GetApplied().Release).(*agentv1.ReleaseIdentity), CommandId: e.OperationId, ReportRevision: ^uint64(0), ReceivedUnixMs: time.Now().UnixMilli(), State: agentv1.ReleaseState_RELEASE_STATE_VERIFYING, ReleaseRevision: ^uint64(0)}
				reply := peerReply(e, nil)
				reply.Payload = &agentv1.Envelope_ActivationReportAck{ActivationReportAck: ack}
				reply.OperationId = e.OperationId
				tc.change(reply)
				writePeer(t, ctx, c, reply)
			})
			c := connect(t, opts)
			_, err := c.ReportApplied(t.Context(), "activate-1", &agentv1.Applied{Release: &agentv1.ReleaseIdentity{ReleaseId: "release-1", Generation: 1, BundleHash: strings.Repeat("a", 64)}, InstanceId: "instance", BootId: "boot"})
			if (err == nil) != (tc.name == "valid") {
				t.Fatal("unbound activation receipt", err)
			}
		})
	}
}

func TestActivationRejectedRequiresArtifactBeforeSending(t *testing.T) {
	// A missing activation revision must fail before touching the connection.
	var c Client
	_, err := c.ReportActivationRejected(t.Context(), "activate-1", &agentv1.Rejected{Release: &agentv1.ReleaseIdentity{Generation: 1}})
	if !errors.Is(err, ErrProtocol) {
		t.Fatal("unbound activation rejection accepted", err)
	}
}

func TestReleaseStatusRejectsMixedActivationArtifacts(t *testing.T) {
	identity := &agentv1.ReleaseIdentity{ReleaseId: "release-1", Generation: 1, BundleHash: strings.Repeat("a", 64)}
	q := &agentv1.ReleaseStatusQuery{ReleaseId: identity.ReleaseId}
	r := &agentv1.ReleaseStatus{Release: identity, State: agentv1.ReleaseState_RELEASE_STATE_VERIFYING, ReleaseRevision: 5, ReportRevision: 2, ActivationArtifactRevision: 2, RequiredSources: 1, Applied: []*agentv1.Applied{{Release: identity, InstanceId: "source", ActivationArtifactRevision: 1}}}
	if validReleaseStatus(q, r) {
		t.Fatal("mixed artifact status accepted")
	}
	r.Applied[0].ActivationArtifactRevision = 2
	if !validReleaseStatus(q, r) {
		t.Fatal("current artifact status rejected")
	}
	q.ExpectedActivationArtifactRevision = new(uint64(1))
	if validReleaseStatus(q, r) {
		t.Fatal("status ignored pinned artifact")
	}
	q.ExpectedActivationArtifactRevision = nil
	r.Applied = nil
	r.Rejected = []*agentv1.Rejected{{Release: identity, InstanceId: "source"}}
	if validReleaseStatus(q, r) {
		t.Fatal("legacy NACK assigned to a newer artifact")
	}
	r.ActivationArtifactRevision = 1
	if !validReleaseStatus(q, r) {
		t.Fatal("historical artifact 1 NACK lost")
	}
}

func TestPreparedReceiptCannotExtendRequestedEvidence(t *testing.T) {
	for _, delta := range []int64{0, 1} {
		t.Run(strconv.FormatInt(delta, 10), func(t *testing.T) {
			opts := serverOptions(t, nil, func(ctx context.Context, conn *websocket.Conn, e *agentv1.Envelope) {
				report := e.GetPrepared()
				ack := &agentv1.PreparedAck{Release: report.Release, CommandId: e.OperationId, EvidenceRevision: 1, ReceivedUnixMs: report.PreparedUnixMs, ExpiresUnixMs: report.ExpiresUnixMs + delta}
				reply := peerReply(e, nil)
				reply.Payload = &agentv1.Envelope_PreparedAck{PreparedAck: ack}
				reply.OperationId = e.OperationId
				writePeer(t, ctx, conn, reply)
			})
			c := connect(t, opts)
			now := time.Now().UnixMilli()
			_, err := c.ReportPrepared(t.Context(), "prepare-1", &agentv1.Prepared{Release: &agentv1.ReleaseIdentity{Generation: 1}, PreparedUnixMs: now, ExpiresUnixMs: now + 300000})
			if (err == nil) != (delta == 0) {
				t.Fatal("invalid evidence receipt boundary", err)
			}
		})
	}
}
