package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"slices"
	"sync"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

// PreparationOptions describes the actual local workload, independently of the
// server's command. Builder must load application resources and provide a health
// check plus a factory which instantiates complete, separately owned views.
type PreparationOptions struct {
	Scope                                               Scope
	PolicyKey, InstanceID, BootID, BuildID, ImageDigest string
	Verifier                                            *Verifier
	Now                                                 func() time.Time
	Builder                                             func(context.Context, PreparationSpec) (PreparationResources, error)
}
type PreparationSpec struct {
	Release *agentv1.ReleaseIdentity
	Bundle  BundleDocument
	Build   BundleBuild
	Policy  *Policy
}
type PreparationResources struct {
	Artifacts   map[string][]byte
	CheckHealth func(context.Context) error
	Instantiate func(context.Context, RuntimeSpec) (RuntimeResources, error)
	Close       func() error
}

// PreparedWorkload owns a candidate, never an active view. Keep this handle for
// reconnect retries; its report and deadline never slide. Close retires its owner
// reference; instantiated RuntimeViews retain resources until their last user.
// It is intentionally process-local: loss of the workload/boot requires a new
// directed preparation, not restoration of readiness from a receipt on disk.
type PreparedWorkload struct {
	mu             sync.Mutex
	opts           PreparationOptions
	guard          *cacheGuard
	release        *agentv1.ReleaseIdentity
	command        string
	bundle, policy *agentv1.SignedResource
	resources      PreparationResources
	report         *agentv1.Prepared
	until          time.Time
	timer          *time.Timer
	refs           int64
	retired        bool
	cancel         context.CancelFunc
	lifetime       context.Context
	done           chan struct{}
	closeErr       error
}

func preparationArtifacts(build BundleBuild, artifacts map[string][]byte) error {
	if len(artifacts) != len(build.Resources) {
		return ErrRuntimeResources
	}
	total := 0
	for _, expected := range build.Resources {
		body, ok := artifacts[expected.Key]
		if !ok || len(body) > MaxResourceBytes-total {
			return ErrRuntimeResources
		}
		total += len(body)
		hash := sha256.Sum256(body)
		if hex.EncodeToString(hash[:]) != expected.SHA256 {
			return ErrRuntimeResources
		}
	}
	return nil
}
func (p *PreparedWorkload) verifyLocked() (*Bundle, *Policy, BundleBuild, error) {
	var build BundleBuild
	if p.retired {
		return nil, nil, build, ErrCacheClosed
	}
	now, err := p.guard.time()
	if err != nil {
		return nil, nil, build, err
	}
	if !p.until.IsZero() && !time.Now().Before(p.until) {
		return nil, nil, build, ErrExpired
	}
	raw, _ := hex.DecodeString(p.release.BundleHash)
	var hash [32]byte
	copy(hash[:], raw)
	bundle, _, err := VerifyBundle(p.guard.verifier.Load(), p.bundle, p.opts.Scope, hash, Watermark{}, now)
	if err != nil {
		return nil, nil, build, err
	}
	if bundle.document.Policy.Key != p.opts.PolicyKey {
		return nil, nil, build, ErrScope
	}
	policy, _, err := bundle.VerifyPolicy(p.guard.verifier.Load(), p.policy, Watermark{}, now)
	if err != nil {
		return nil, nil, build, err
	}
	for _, candidate := range bundle.document.Builds {
		if candidate.ID == p.opts.BuildID {
			build = candidate
			break
		}
	}
	if build.ID == "" || build.ImageDigest != p.opts.ImageDigest || build.ProtocolVersion != agentv1.ProtocolMajor {
		return nil, nil, build, ErrRuntimeResources
	}
	return bundle, policy, build, nil
}
func (p *PreparedWorkload) acquire(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retired || p.refs == math.MaxInt64 {
		return nil, nil, ErrCacheClosed
	}
	p.refs++
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.lifetime, cancel)
	return ctx, func() { stop(); cancel() }, nil
}
func (p *PreparedWorkload) releaseRef() {
	p.mu.Lock()
	p.refs--
	last := p.refs == 0
	p.mu.Unlock()
	if last {
		go func() {
			var err error
			if p.resources.Close != nil {
				err = p.resources.Close()
			}
			p.mu.Lock()
			p.closeErr = err
			p.mu.Unlock()
			close(p.done)
		}()
	}
}
func (p *PreparedWorkload) retire() {
	p.mu.Lock()
	if p.retired {
		p.mu.Unlock()
		return
	}
	p.retired = true
	p.cancel()
	if p.timer != nil {
		p.timer.Stop()
	}
	p.mu.Unlock()
	p.releaseRef()
}

// Retire stops new reports/instantiations without waiting for active RuntimeViews.
// Their retained references release the workload after their final user drains.
func (p *PreparedWorkload) Retire() { p.retire() }

func (p *PreparedWorkload) Close(ctx context.Context) error {
	p.retire()
	select {
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *PreparedWorkload) UpdateTrustRoots(v *Verifier) error {
	if v == nil {
		return ErrResource
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retired {
		return ErrCacheClosed
	}
	p.guard.verifier.Store(v)
	return nil
}

// PreparedReport rechecks trust, bytes and health, returning the original report.
// Its success is not permission to route and cannot extend the original TTL.
func (p *PreparedWorkload) PreparedReport(ctx context.Context) (*agentv1.Prepared, error) {
	ctx, cancel, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer p.releaseRef()
	p.mu.Lock()
	_, _, build, err := p.verifyLocked()
	report := proto.Clone(p.report).(*agentv1.Prepared)
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := preparationArtifacts(build, p.resources.Artifacts); err != nil {
		return nil, err
	}
	if err := p.resources.CheckHealth(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, _, _, err := p.verifyLocked(); err != nil {
		return nil, err
	}
	if err := preparationArtifacts(build, p.resources.Artifacts); err != nil {
		return nil, err
	}
	return report, nil
}

// BuildRuntime is a RuntimeOptions.Builder. It binds the retained workload to
// the exact signed Bundle/policy and current prepare command in the activation.
func (p *PreparedWorkload) BuildRuntime(ctx context.Context, spec RuntimeSpec) (RuntimeResources, error) {
	ctx, cancel, err := p.acquire(ctx)
	if err != nil {
		return RuntimeResources{}, err
	}
	defer cancel()
	p.mu.Lock()
	bundle, _, build, err := p.verifyLocked()
	p.mu.Unlock()
	if err == nil {
		mark := spec.Metadata.Watermark
		expectedHash := sha256.Sum256(p.policy.Payload)
		if spec.Metadata.Scope != p.opts.Scope || spec.Metadata.InstanceID != p.opts.InstanceID || spec.Metadata.BootID != p.opts.BootID || spec.Metadata.BuildID != p.opts.BuildID || mark.ControlPlaneEpoch != bundle.claims.ControlPlaneEpoch || mark.Generation != p.release.Generation || mark.BundleHash != bundle.Hash() || mark.BundleArtifactRevision != bundle.claims.Revision || mark.PolicyRevision != bundle.document.Policy.Revision || mark.PolicyHash != expectedHash {
			err = ErrScope
		}
		if err == nil {
			canonical, e := MarshalBundle(spec.Bundle)
			if e != nil || !bytes.Equal(canonical, p.bundle.Payload) {
				err = ErrResource
			}
		}
		matched := false
		for _, member := range spec.Members {
			if member.Instance.InstanceID == p.opts.InstanceID && member.Instance.BootID == p.opts.BootID && member.Instance.BuildID == p.opts.BuildID && member.Instance.ImageDigest == p.opts.ImageDigest && member.Instance.ProtocolVersion == agentv1.ProtocolMajor && member.Instance.Ready && !member.Instance.Draining && member.PrepareCommandID == p.command {
				matched = true
			}
		}
		if !matched {
			err = ErrScope
		}
	}
	if err == nil {
		err = preparationArtifacts(build, p.resources.Artifacts)
	}
	if err == nil {
		err = p.resources.CheckHealth(ctx)
	}
	if err != nil {
		p.releaseRef()
		return RuntimeResources{}, err
	}
	resources, err := p.resources.Instantiate(ctx, spec)
	original := resources.Close
	var once sync.Once
	var closeErr error
	resources.Close = func() error {
		once.Do(func() {
			defer p.releaseRef()
			if original != nil {
				closeErr = original()
			}
		})
		return closeErr
	}
	if err == nil && original == nil {
		err = ErrRuntimeResources
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		p.mu.Lock()
		_, _, _, err = p.verifyLocked()
		p.mu.Unlock()
	}
	if err == nil {
		err = preparationArtifacts(build, p.resources.Artifacts)
	}
	return resources, err
}

func newPreparedWorkload(ctx context.Context, command string, release *agentv1.ReleaseIdentity, bundle, policy *agentv1.SignedResource, o PreparationOptions) (*PreparedWorkload, error) {
	life, cancel := context.WithCancel(context.Background())
	p := &PreparedWorkload{opts: o, guard: &cacheGuard{now: o.Now}, release: proto.Clone(release).(*agentv1.ReleaseIdentity), command: command, bundle: proto.Clone(bundle).(*agentv1.SignedResource), policy: proto.Clone(policy).(*agentv1.SignedResource), refs: 1, lifetime: life, cancel: cancel, done: make(chan struct{})}
	p.guard.verifier.Store(o.Verifier)
	b, verifiedPolicy, build, err := p.verifyLocked()
	if err != nil {
		p.retire()
		return p, err
	}
	copyBuild := build
	copyBuild.Resources = slices.Clone(build.Resources)
	p.resources, err = o.Builder(ctx, PreparationSpec{Release: proto.Clone(release).(*agentv1.ReleaseIdentity), Bundle: b.Document(), Build: copyBuild, Policy: verifiedPolicy})
	if err == nil && (p.resources.Close == nil || p.resources.CheckHealth == nil || p.resources.Instantiate == nil) {
		err = ErrRuntimeResources
	}
	if err == nil {
		err = preparationArtifacts(build, p.resources.Artifacts)
	}
	if err == nil {
		err = p.resources.CheckHealth(ctx)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		_, _, _, err = p.verifyLocked()
	}
	if err == nil {
		err = preparationArtifacts(build, p.resources.Artifacts)
	}
	if err != nil {
		p.retire()
		return p, err
	}
	now, err := p.guard.time()
	if err != nil {
		p.retire()
		return p, err
	}
	policyClaims, _ := ParseClaims(p.policy.Claims)
	expires := min(now.Add(300*time.Second).UnixMilli(), b.claims.ExpiresUnixMs, policyClaims.ExpiresUnixMs)
	if expires <= now.UnixMilli() {
		p.retire()
		return p, ErrExpired
	}
	p.report = &agentv1.Prepared{Release: proto.Clone(release).(*agentv1.ReleaseIdentity), InstanceId: o.InstanceID, BootId: o.BootID, BuildId: o.BuildID, AcceptedProtocolVersion: agentv1.ProtocolMajor, PreparedUnixMs: now.UnixMilli(), ExpiresUnixMs: expires}
	p.until = time.Now().Add(time.Duration(expires-now.UnixMilli()) * time.Millisecond)
	p.mu.Lock()
	p.timer = time.AfterFunc(time.Until(p.until), p.retire)
	p.mu.Unlock()
	return p, nil
}

// RetryPrepared keeps local preparation distinct from its remote receipt. An
// unknown result must be retried with this same retained handle/report.
func (p *PreparedWorkload) RetryPrepared(ctx context.Context, c *Client) (*agentv1.PreparedAck, error) {
	if c == nil || c.opts.Hello == nil || c.opts.Hello.InstanceId != p.opts.InstanceID || c.opts.Hello.BootId != p.opts.BootID {
		return nil, ErrScope
	}
	report, err := p.PreparedReport(ctx)
	if err != nil {
		return nil, err
	}
	return c.ReportPrepared(ctx, p.command, report)
}
