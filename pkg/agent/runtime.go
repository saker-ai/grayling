package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
)

var (
	ErrRuntimeNotReady    = errors.New("agent: runtime is not ready")
	ErrRuntimeResources   = errors.New("agent: runtime application resources incomplete or incompatible")
	ErrRuntimeUnavailable = errors.New("agent: no prepared endpoint for selected build")
	ErrRuntimeDecision    = errors.New("agent: policy did not produce an authorized build")
	ErrRuntimeIdentity    = errors.New("agent: routing identity missing")
)

type RuntimeOptions struct {
	Directory, Identity                                 string
	Scope                                               Scope
	PolicyKey, InstanceID, BootID, BuildID, ImageDigest string
	AuthenticationKey                                   []byte
	Verifier                                            *Verifier
	Minimum                                             RuntimeWatermark
	MinimumRevocation                                   RevocationFloor
	MaxAge                                              time.Duration
	Now                                                 func() time.Time
	Builder                                             func(context.Context, RuntimeSpec) (RuntimeResources, error)
}
type RuntimeMetadata struct {
	Scope                                  Scope
	InstanceID, BootID, BuildID, CommandID string
	Watermark                              RuntimeWatermark
}
type EndpointKey struct {
	InstanceID, BootID, LeaseID string
	LeaseEpoch                  uint64
}

func RuntimeEndpoint(m DiscoveryMember) EndpointKey {
	return EndpointKey{InstanceID: m.InstanceID, BootID: m.BootID, LeaseID: m.Lease.ID, LeaseEpoch: m.Lease.Epoch}
}

// RuntimeSpec is an isolated construction input. Builder must transfer exclusive
// ownership of returned handlers/transports and not retain mutable aliases.
type RuntimeSpec struct {
	Metadata RuntimeMetadata
	Bundle   BundleDocument
	Members  []ActivationMember
}
type RuntimeResources struct {
	Handler    http.Handler
	Transports map[EndpointKey]http.RoundTripper
	Artifacts  map[string][]byte
	Close      func() error
}
type runtimeCandidate struct {
	activation   *ActivationView
	bundle       *Bundle
	policy       *Policy
	claims       [3]Claims
	metadata     RuntimeMetadata
	source       ActivationSource
	sourceMember DiscoveryMember
	endpoints    map[string][]DiscoveryMember
	resources    RuntimeResources
	data         [3][]byte
}
type runtimeOperation struct{ cancel context.CancelFunc }
type RuntimeManager struct {
	journal        *runtimeJournal
	opts           RuntimeOptions
	guard          *cacheGuard
	writeGate      chan struct{}
	mu             sync.Mutex
	closed         bool
	active         atomic.Pointer[runtimeSnapshot]
	views          map[*runtimeSnapshot]bool
	operations     map[*runtimeOperation]bool
	opsWG, viewsWG sync.WaitGroup
	done           chan struct{}
	closeErr       error
}

func InitializeRuntime(ctx context.Context, o RuntimeOptions) (*RuntimeManager, error) {
	return openRuntime(ctx, o, true)
}

// OpenRuntime restores only sealed authority and floors, never readiness. Call
// Restore to reconstruct process-owned resources and perform an actual swap.
func OpenRuntime(ctx context.Context, o RuntimeOptions) (*RuntimeManager, error) {
	return openRuntime(ctx, o, false)
}
func openRuntime(ctx context.Context, o RuntimeOptions, initialize bool) (*RuntimeManager, error) {
	if !discoveryID(o.InstanceID) || !discoveryID(o.BootID) || !discoveryID(o.BuildID) || !validText(o.PolicyKey) || len(o.ImageDigest) != 71 || !strings.HasPrefix(o.ImageDigest, "sha256:") || !bundleDigest(strings.TrimPrefix(o.ImageDigest, "sha256:")) || o.Now == nil || o.Builder == nil || !o.Minimum.valid() || !o.MinimumRevocation.valid() {
		return nil, ErrResource
	}
	j, r, err := openRuntimeJournal(ctx, o, initialize)
	if err != nil {
		return nil, err
	}
	m := &RuntimeManager{journal: j, opts: j.options, guard: &cacheGuard{now: o.Now}, operations: map[*runtimeOperation]bool{}, done: make(chan struct{}), writeGate: make(chan struct{}, 1), views: map[*runtimeSnapshot]bool{}}
	m.guard.verifier.Store(o.Verifier)
	m.guard.clockHigh.Store(r.ClockHighMs)
	revoked := revokedSet(r.Revoked)
	m.guard.revoked.Store(&revoked)
	// A rollback clock opens NOT_READY instead of deleting or resetting the floor.
	return m, nil
}
func (m *RuntimeManager) begin(ctx context.Context) (context.Context, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, nil, ErrCacheClosed
	}
	if fail := m.guard.failure.Load(); fail != nil {
		return nil, nil, fail.err
	}
	ctx, cancel := context.WithCancel(ctx)
	op := &runtimeOperation{cancel: cancel}
	m.operations[op] = true
	m.opsWG.Add(1)
	return ctx, func() { cancel(); m.mu.Lock(); delete(m.operations, op); m.mu.Unlock(); m.opsWG.Done() }, nil
}
func (m *RuntimeManager) poison(err error) {
	// A canceled read or a transaction which never reached commit leaves the
	// installed view intact. An ambiguous commit still fails closed.
	if !errors.Is(err, ErrCacheIO) && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return
	}
	m.guard.failure.Store(&cacheFailure{err: err})
}
func (m *RuntimeManager) verify(data [3][]byte, command string, now time.Time) (runtimeCandidate, error) {
	var c runtimeCandidate
	in, err := runtimeInputs(data)
	if err != nil {
		return c, err
	}
	claims, err := ParseClaims(in.Activation.Claims)
	if err != nil {
		return c, err
	}
	generation, err := strconv.ParseUint(strings.TrimPrefix(claims.Key, "activation-"), 10, 64)
	if err != nil || claims.Key != ActivationKey(generation) {
		return c, ErrResource
	}
	v := m.guard.verifier.Load()
	c.activation, c.claims[0], err = VerifyActivation(v, in.Activation, m.opts.Scope, generation, claims.BundleHash, Watermark{}, now)
	if err != nil {
		return c, err
	}
	c.bundle, c.claims[1], err = c.activation.VerifyBundle(v, in.Bundle, Watermark{}, now)
	if err != nil {
		return c, err
	}
	if c.bundle.document.Policy.Key != m.opts.PolicyKey {
		return c, ErrScope
	}
	c.policy, c.claims[2], err = c.bundle.VerifyPolicy(v, in.Policy, Watermark{}, now)
	if err != nil {
		return c, err
	}
	if c.claims[0].ExpiresUnixMs > c.claims[2].ExpiresUnixMs {
		return c, ErrResource
	}
	d := c.activation.document
	c.endpoints = map[string][]DiscoveryMember{}
	for _, s := range d.Sources {
		if s.InstanceID == m.opts.InstanceID && s.BootID == m.opts.BootID {
			c.source = s
			break
		}
	}
	if c.source.CommandID == "" || command != "" && command != c.source.CommandID {
		return c, ErrScope
	}
	for _, member := range d.Members {
		i := member.Instance
		c.endpoints[i.BuildID] = append(c.endpoints[i.BuildID], i)
		if i.InstanceID == m.opts.InstanceID && i.BootID == m.opts.BootID {
			c.sourceMember = i
		}
	}
	if c.sourceMember.BuildID != m.opts.BuildID || c.sourceMember.ImageDigest != m.opts.ImageDigest {
		return c, ErrRuntimeResources
	}
	mark := RuntimeWatermark{ControlPlaneEpoch: d.ControlPlaneEpoch, Generation: d.Generation, ActivationRevision: c.claims[0].Revision, BundleArtifactRevision: d.BundleArtifactRevision, PolicyRevision: d.PolicyRevision, DiscoveryRevision: d.DiscoveryRevision, PreparedRevision: d.PreparedRevision, RevocationEpoch: d.RevocationEpoch, BundleHash: c.claims[0].BundleHash, ActivationHash: c.claims[0].PayloadHash, PolicyHash: c.claims[2].PayloadHash, InputHash: runtimeInputHash(data)}
	c.metadata = RuntimeMetadata{Scope: m.opts.Scope, InstanceID: m.opts.InstanceID, BootID: m.opts.BootID, BuildID: m.opts.BuildID, CommandID: c.source.CommandID, Watermark: mark}
	c.data = data
	return c, nil
}
func (m *RuntimeManager) build(ctx context.Context, c *runtimeCandidate) error {
	spec := RuntimeSpec{Metadata: c.metadata, Bundle: c.bundle.Document(), Members: c.activation.Document().Members}
	resources, err := m.opts.Builder(ctx, spec)
	c.resources = resources
	if err != nil {
		return err
	}
	if resources.Handler == nil || resources.Close == nil {
		return ErrRuntimeResources
	}
	transports := make(map[EndpointKey]http.RoundTripper, len(resources.Transports))
	for key, t := range resources.Transports {
		transports[key] = t
	}
	c.resources.Transports = transports
	for _, members := range c.endpoints {
		for _, member := range members {
			if member.InstanceID == m.opts.InstanceID && member.BootID == m.opts.BootID {
				continue
			}
			if transports[RuntimeEndpoint(member)] == nil {
				return ErrRuntimeResources
			}
		}
	}
	var expected []BundleResource
	for _, build := range c.bundle.document.Builds {
		if build.ID == m.opts.BuildID {
			expected = build.Resources
			break
		}
	}
	if len(resources.Artifacts) != len(expected) {
		return ErrRuntimeResources
	}
	total := 0
	for _, artifact := range expected {
		body, ok := resources.Artifacts[artifact.Key]
		if !ok || len(body) > MaxResourceBytes-total {
			return ErrRuntimeResources
		}
		total += len(body)
		digest := sha256.Sum256(body)
		if hex.EncodeToString(digest[:]) != artifact.SHA256 {
			return ErrRuntimeResources
		}
	}
	c.resources.Artifacts = nil
	return nil
}
func closeRuntimeResources(r RuntimeResources) error {
	for _, transport := range r.Transports {
		if idle, ok := transport.(interface{ CloseIdleConnections() }); ok {
			idle.CloseIdleConnections()
		}
	}
	if r.Close != nil {
		return r.Close()
	}
	return nil
}

// Install persists exact inputs and independent floors before publishing one
// complete pointer. Applied is generated only after that actual publication.
func (m *RuntimeManager) Install(ctx context.Context, command string, in RuntimeInput) (*agentv1.Applied, error) {
	if !discoveryID(command) {
		return nil, ErrResource
	}
	data, err := runtimeInputBytes(in)
	if err != nil {
		return nil, err
	}
	return m.install(ctx, command, data, false)
}
func (m *RuntimeManager) Restore(ctx context.Context) (*agentv1.Applied, error) {
	return m.install(ctx, "", [3][]byte{}, true)
}
func (m *RuntimeManager) lock(ctx context.Context) error {
	select {
	case m.writeGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (m *RuntimeManager) unlock() { <-m.writeGate }
func (m *RuntimeManager) install(ctx context.Context, command string, data [3][]byte, restore bool) (*agentv1.Applied, error) {
	ctx, done, err := m.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	if err := m.lock(ctx); err != nil {
		return nil, err
	}
	initial, err := m.journal.read(ctx)
	m.unlock()
	if err != nil {
		m.poison(err)
		return nil, err
	}
	if restore {
		if initial.Mark == (RuntimeWatermark{}) {
			return nil, ErrRuntimeNotReady
		}
		data = initial.Inputs
		command = initial.Command
	}
	now, err := m.guard.time()
	if err != nil {
		return nil, err
	}
	candidate, err := m.verify(data, command, now)
	if err != nil {
		return nil, err
	}
	if err := m.checkCandidate(initial, candidate, now); err != nil {
		return nil, err
	}
	mark := candidate.metadata.Watermark
	if current := m.active.Load(); initial.Mark == mark && current != nil && current.candidate.metadata.Watermark == mark {
		return m.Applied()
	}
	// Application work runs outside the durable commit gate. Revocation and
	// a newer publication must never wait for an uninterruptible builder.
	if err := m.build(ctx, &candidate); err != nil {
		_ = closeRuntimeResources(candidate.resources)
		return nil, err
	}
	owned := true
	defer func() {
		if owned {
			_ = closeRuntimeResources(candidate.resources)
		}
	}()
	if err := m.lock(ctx); err != nil {
		return nil, err
	}
	defer m.unlock()
	record, err := m.journal.read(ctx)
	if err != nil {
		m.poison(err)
		return nil, err
	}
	now, err = m.guard.time()
	if err != nil {
		return nil, err
	}
	checked, err := m.verify(data, command, now)
	if err != nil {
		return nil, err
	}
	if checked.metadata != candidate.metadata {
		return nil, ErrResource
	}
	if err := m.checkCandidate(record, candidate, now); err != nil {
		return nil, err
	}
	if current := m.active.Load(); record.Mark == mark && current != nil && current.candidate.metadata.Watermark == mark {
		return m.Applied()
	}
	if record.Mark != mark {
		record.VerifiedMs = now.UnixMilli()
	}
	record.Mark = mark
	record.Command = candidate.source.CommandID
	record.Inputs = data
	record.ClockHighMs = max(record.ClockHighMs, now.UnixMilli())
	if err := m.journal.write(ctx, record); err != nil {
		m.poison(err)
		return nil, err
	}
	// Crash after commit leaves durable authority but no claimed process readiness.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now, err = m.guard.time()
	if err != nil {
		return nil, err
	}
	if _, err := m.verify(data, command, now); err != nil {
		return nil, err
	}
	if err := m.checkCandidate(record, candidate, now); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrCacheClosed
	}
	snapshot := newRuntimeSnapshot(m, candidate, record.VerifiedMs, now)
	m.viewsWG.Add(1)
	m.views[snapshot] = true
	previous := m.active.Swap(snapshot)
	owned = false
	report := snapshot.applied()
	m.mu.Unlock()
	if previous != nil {
		previous.release()
	}
	return report, nil
}
func (m *RuntimeManager) checkCandidate(r runtimeRecord, c runtimeCandidate, now time.Time) error {
	mark := c.metadata.Watermark
	if !revocationAbove(r, m.opts.MinimumRevocation) {
		return ErrRollback
	}
	if r.RevocationControlPlaneEpoch != 0 && mark.ControlPlaneEpoch != r.RevocationControlPlaneEpoch {
		return ErrRollback
	}
	if !runtimeAbove(mark, r.Mark) || !runtimeAbove(mark, m.opts.Minimum) || mark.RevocationEpoch != r.RevocationEpoch {
		return ErrRollback
	}
	if set := m.guard.revoked.Load(); set != nil && (*set)[mark.BundleHash] {
		return ErrPolicyRevoked
	}
	if r.Mark == mark {
		if !equalRuntimeInputs(c.data, r.Inputs) {
			return ErrRollback
		}
		return runtimeFresh(r, now, m.opts.MaxAge)
	}
	return nil
}
func recordWithMark(r runtimeRecord, mark RuntimeWatermark) runtimeRecord { r.Mark = mark; return r }
func (m *RuntimeManager) Watermark(ctx context.Context) (RuntimeWatermark, error) {
	ctx, done, err := m.begin(ctx)
	if err != nil {
		return RuntimeWatermark{}, err
	}
	defer done()
	if err := m.lock(ctx); err != nil {
		return RuntimeWatermark{}, err
	}
	defer m.unlock()
	r, err := m.journal.read(ctx)
	if err != nil {
		m.poison(err)
	}
	return r.Mark, err
}
func (m *RuntimeManager) Close(ctx context.Context) error {
	m.mu.Lock()
	first := !m.closed
	if first {
		m.closed = true
		m.guard.closed.Store(true)
		for op := range m.operations {
			op.cancel()
		}
	}
	previous := m.active.Swap(nil)
	views := make([]*runtimeSnapshot, 0, len(m.views))
	for view := range m.views {
		views = append(views, view)
	}
	m.mu.Unlock()
	if first {
		for _, view := range views {
			view.cancelRequests()
		}
	}
	if previous != nil {
		previous.release()
	}
	if first {
		go func() {
			m.opsWG.Wait()
			m.viewsWG.Wait()
			m.closeErr = errors.Join(m.closeErr, m.journal.close())
			close(m.done)
		}()
	}
	select {
	case <-m.done:
		return m.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
