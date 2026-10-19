package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/pkg/model"
)

type runtimeSnapshot struct {
	manager    *RuntimeManager
	candidate  runtimeCandidate
	verifiedMs int64
	roots      [3][32]byte
	deadline   time.Time
	mu         sync.Mutex
	refs       int64
	closed     bool
	requests   map[*runtimeOperation]bool
}

func newRuntimeSnapshot(m *RuntimeManager, c runtimeCandidate, verifiedMs int64, now time.Time) *runtimeSnapshot {
	remaining := time.Duration(c.claims[0].ExpiresUnixMs-now.UnixMilli()) * time.Millisecond
	ageRemaining := time.UnixMilli(verifiedMs).Add(m.opts.MaxAge).Sub(now)
	remaining = min(remaining, ageRemaining)
	v := &runtimeSnapshot{manager: m, candidate: c, verifiedMs: verifiedMs, refs: 1, requests: map[*runtimeOperation]bool{}, deadline: time.Now().Add(remaining)}
	trust := m.guard.verifier.Load()
	for i, claims := range c.claims {
		copy(v.roots[i][:], trust.keys[claims.KeyID].PublicKey)
	}
	return v
}
func (v *runtimeSnapshot) acquire() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || v.refs == math.MaxInt64 {
		return false
	}
	v.refs++
	return true
}
func (v *runtimeSnapshot) release() {
	v.mu.Lock()
	v.refs--
	last := v.refs == 0
	if last {
		v.closed = true
	}
	v.mu.Unlock()
	if last {
		go func() {
			defer v.manager.viewsWG.Done()
			err := closeRuntimeResources(v.candidate.resources)
			v.manager.mu.Lock()
			delete(v.manager.views, v)
			v.manager.closeErr = errors.Join(v.manager.closeErr, err)
			v.manager.mu.Unlock()
		}()
	}
}
func (v *runtimeSnapshot) check() error {
	g := v.manager.guard
	if g.closed.Load() {
		return ErrCacheClosed
	}
	if failure := g.failure.Load(); failure != nil {
		return failure.err
	}
	now, err := g.time()
	if err != nil {
		return err
	}
	if !time.Now().Before(v.deadline) {
		return ErrExpired
	}
	if set := g.revoked.Load(); set != nil && (*set)[v.candidate.metadata.Watermark.BundleHash] {
		return ErrPolicyRevoked
	}
	trust := g.verifier.Load()
	if trust == nil {
		return ErrTrustChanged
	}
	for i, claims := range v.candidate.claims {
		root, ok := trust.keys[claims.KeyID]
		if !ok || !bytes.Equal(root.PublicKey, v.roots[i][:]) || now.Before(root.NotBefore) || !now.Before(root.NotAfter) || time.UnixMilli(claims.IssuedUnixMs).Before(root.NotBefore) || time.UnixMilli(claims.ExpiresUnixMs).After(root.NotAfter) {
			return ErrTrustChanged
		}
		if now.UnixMilli() < claims.IssuedUnixMs || now.UnixMilli() >= claims.ExpiresUnixMs {
			return ErrExpired
		}
	}
	if !now.Before(time.UnixMilli(v.verifiedMs).Add(v.manager.opts.MaxAge)) {
		return ErrPolicyStale
	}
	return nil
}
func (v *runtimeSnapshot) applied() *agentv1.Applied {
	meta := v.candidate.metadata
	mark := meta.Watermark
	return &agentv1.Applied{Release: &agentv1.ReleaseIdentity{ReleaseId: v.candidate.activation.document.ReleaseID, Generation: mark.Generation, BundleHash: v.candidate.activation.document.BundleHash}, InstanceId: meta.InstanceID, BootId: meta.BootID, PolicyRevision: mark.PolicyRevision, DiscoveryRevision: mark.DiscoveryRevision, PreparedRevision: mark.PreparedRevision, RevocationEpoch: mark.RevocationEpoch, ActivationArtifactRevision: mark.ActivationRevision, ControlPlaneEpoch: mark.ControlPlaneEpoch}
}

type RuntimeLease struct {
	view     *runtimeSnapshot
	released atomic.Bool
}

func (m *RuntimeManager) Capture() (*RuntimeLease, error) {
	for {
		if m.guard.closed.Load() {
			return nil, ErrCacheClosed
		}
		if failure := m.guard.failure.Load(); failure != nil {
			return nil, failure.err
		}
		v := m.active.Load()
		if v == nil {
			return nil, ErrRuntimeNotReady
		}
		if !v.acquire() {
			continue
		}
		if m.active.Load() != v {
			v.release()
			continue
		}
		if err := v.check(); err != nil {
			v.release()
			return nil, err
		}
		return &RuntimeLease{view: v}, nil
	}
}
func (l *RuntimeLease) Release() {
	if l != nil && l.released.CompareAndSwap(false, true) {
		l.view.release()
	}
}
func (l *RuntimeLease) borrow() (func(), error) {
	if l == nil || l.released.Load() || !l.view.acquire() {
		return nil, ErrCacheClosed
	}
	if l.released.Load() {
		l.view.release()
		return nil, ErrCacheClosed
	}
	if err := l.view.check(); err != nil {
		l.view.release()
		return nil, err
	}
	return l.view.release, nil
}
func (l *RuntimeLease) Metadata() (RuntimeMetadata, error) {
	done, err := l.borrow()
	if err != nil {
		return RuntimeMetadata{}, err
	}
	defer done()
	return l.view.candidate.metadata, nil
}
func (m *RuntimeManager) Applied() (*agentv1.Applied, error) {
	lease, err := m.Capture()
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	return lease.view.applied(), nil
}

type RuntimeDecision struct {
	Metadata         RuntimeMetadata
	BuildID          string
	Endpoint         DiscoveryMember
	Local, Defaulted bool
	Evaluation       model.Result
}

// Select fixes the build before balancing. Empty capacity never selects a
// different build, including when an explicit default build was used.
func (l *RuntimeLease) Select(input model.EvaluationContext, balance uint64) (RuntimeDecision, error) {
	done, err := l.borrow()
	if err != nil {
		return RuntimeDecision{}, err
	}
	defer done()
	c := l.view.candidate
	d := c.bundle.document
	result := c.policy.Evaluate(d.RouteFlag, input)
	decision := RuntimeDecision{Metadata: c.metadata, Evaluation: result}
	value, ok := result.Value.(string)
	build, mapped := d.ValueBuilds[value]
	if result.Failed || result.ErrorCode != "" || !ok || !mapped {
		if d.DefaultBehavior != "build" {
			if result.ErrorCode == model.ErrMissingBucketKey {
				return decision, ErrRuntimeIdentity
			}
			return decision, ErrRuntimeDecision
		}
		decision.Defaulted = true
		build = d.DefaultBuild
	}
	decision.BuildID = build
	members := c.endpoints[build]
	if len(members) == 0 {
		return decision, ErrRuntimeUnavailable
	}
	if build == c.metadata.BuildID {
		decision.Local = true
		decision.Endpoint = c.sourceMember
	} else {
		decision.Endpoint = members[balance%uint64(len(members))]
	}
	return decision, nil
}
func (v *runtimeSnapshot) request(ctx context.Context) (context.Context, func(), error) {
	if !v.acquire() {
		return nil, nil, ErrCacheClosed
	}
	ctx, cancel := context.WithDeadline(ctx, v.deadline)
	op := &runtimeOperation{cancel: cancel}
	v.mu.Lock()
	if err := v.check(); err != nil {
		v.mu.Unlock()
		cancel()
		v.release()
		return nil, nil, err
	}
	v.requests[op] = true
	v.mu.Unlock()
	var once sync.Once
	return ctx, func() { once.Do(func() { cancel(); v.mu.Lock(); delete(v.requests, op); v.mu.Unlock(); v.release() }) }, nil
}

type runtimeContextKey struct{}

func RuntimeFromContext(ctx context.Context) (RuntimeMetadata, bool) {
	meta, ok := ctx.Value(runtimeContextKey{}).(RuntimeMetadata)
	return meta, ok
}
func (l *RuntimeLease) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	done, err := l.borrow()
	if err != nil {
		return err
	}
	defer done()
	ctx, release, err := l.view.request(r.Context())
	if err != nil {
		return err
	}
	defer release()
	ctx = context.WithValue(ctx, runtimeContextKey{}, l.view.candidate.metadata)
	l.view.candidate.resources.Handler.ServeHTTP(w, r.WithContext(ctx))
	return nil
}

// Forward pins URL authority to the verified endpoint and keeps the view alive
// until response Body EOF/Close, even if the caller releases its capture earlier.
func (l *RuntimeLease) Forward(d RuntimeDecision, r *http.Request) (*http.Response, error) {
	done, err := l.borrow()
	if err != nil {
		return nil, err
	}
	defer done()
	c := l.view.candidate
	if r == nil || r.URL == nil || d.Local || d.Metadata != c.metadata || d.BuildID != d.Endpoint.BuildID {
		return nil, ErrRuntimeDecision
	}
	found := false
	for _, member := range c.endpoints[d.BuildID] {
		if member == d.Endpoint {
			found = true
			break
		}
	}
	if !found {
		return nil, ErrRuntimeUnavailable
	}
	transport := c.resources.Transports[RuntimeEndpoint(d.Endpoint)]
	if transport == nil {
		return nil, ErrRuntimeResources
	}
	ctx, release, err := l.view.request(r.Context())
	if err != nil {
		return nil, err
	}
	request := r.Clone(ctx)
	request.URL.Scheme = "https"
	request.URL.Host = net.JoinHostPort(d.Endpoint.Address, strconv.Itoa(int(d.Endpoint.Port)))
	request.URL.User = nil
	request.URL.Fragment = ""
	request.Host = d.Endpoint.TLSServerName
	request.RequestURI = ""
	response, err := transport.RoundTrip(request)
	if err != nil {
		release()
		return nil, err
	}
	if response == nil {
		release()
		return nil, ErrRuntimeUnavailable
	}
	if response.Body == nil || response.Body == http.NoBody {
		release()
	} else {
		body := &runtimeBody{ReadCloser: response.Body, release: release}
		response.Body = body
		context.AfterFunc(ctx, func() { _ = body.Close() })
	}
	return response, nil
}

type runtimeBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
	err     error
}

func (b *runtimeBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		_ = b.Close()
	}
	return n, err
}
func (b *runtimeBody) Close() error {
	b.once.Do(func() { b.err = b.ReadCloser.Close(); b.release() })
	return b.err
}

func (v *runtimeSnapshot) cancelRequests() {
	v.mu.Lock()
	defer v.mu.Unlock()
	for op := range v.requests {
		op.cancel()
	}
}
