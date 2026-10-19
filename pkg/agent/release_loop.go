package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

const (
	releasePrepare = iota
	releaseActivate
	releaseRevoke
)

type releasePosition struct {
	major, minor uint64
	command      string
	digest       [32]byte
}
type releaseTask struct {
	lane            int
	event           *agentv1.Envelope
	position        releasePosition
	prepare         PreparationResult
	activation      RuntimeActivationResult
	err             error
	due             time.Time
	waitPreparation bool
}

// ReleaseLoopState reports local work and remote receipts separately. It is
// diagnostic: a receipt, connection or Prepared never proves ACTIVE or health.
// Preparation.Workload is omitted; the PreparationSet retains resource ownership.
type ReleaseLoopState struct {
	Running                                            bool
	Preparation                                        PreparationResult
	Activation                                         RuntimeActivationResult
	RevocationEpoch, RevocationArtifactRevision        uint64
	PreparationError, ActivationError, RevocationError error
}

// ReleaseLoop handles negotiated Prepare/Activate/Revoke events for one boot.
// Four preparations, one activation and one revocation can run concurrently,
// with only the latest pending command per lane. Revocation never queues behind
// application construction. The caller owns Runtime and PreparationSet shutdown.
type ReleaseLoop struct {
	mu      sync.Mutex
	running bool
	epoch   uint64
	set     *PreparationSet
	runtime *RuntimeManager
	state   ReleaseLoopState
	heads   [3]releasePosition
	pending [3]*releaseTask
}

func NewReleaseLoop(set *PreparationSet, runtime *RuntimeManager) (*ReleaseLoop, error) {
	if set == nil || runtime == nil {
		return nil, ErrProtocol
	}
	set.mu.Lock()
	o := set.opts
	set.mu.Unlock()
	r := runtime.opts
	if o.Scope != r.Scope || o.PolicyKey != r.PolicyKey || o.InstanceID != r.InstanceID || o.BootID != r.BootID || o.BuildID != r.BuildID || o.ImageDigest != r.ImageDigest {
		return nil, ErrScope
	}
	return &ReleaseLoop{set: set, runtime: runtime}, nil
}
func (l *ReleaseLoop) State() ReleaseLoopState {
	l.mu.Lock()
	defer l.mu.Unlock()
	state := l.state
	state.Running = l.running
	state.Preparation = clonePreparationResult(state.Preparation)
	state.Preparation.Workload = nil
	state.Activation = cloneRuntimeActivationResult(state.Activation)
	return state
}
func cloneRuntimeActivationResult(r RuntimeActivationResult) RuntimeActivationResult {
	if r.Applied != nil {
		r.Applied = proto.Clone(r.Applied).(*agentv1.Applied)
	}
	if r.Receipt != nil {
		r.Receipt = proto.Clone(r.Receipt).(*agentv1.ActivationReportAck)
	}
	return r
}

// RunAgent combines registered reconnect/renewal with the release event loop.
// options can refresh credentials and endpoint on each dial. Neither this helper
// nor Serve manufactures NACKs, health updates, new deployment authority or ACTIVE.
func RunAgent(ctx context.Context, options func(context.Context) (Options, error), registration *InstanceRegistration, loop *ReleaseLoop) error {
	if loop == nil || loop.runtime == nil || loop.set == nil || registration == nil || options == nil {
		return ErrProtocol
	}
	registration.mu.Lock()
	request := registration.request
	valid := request != nil && request.Instance != nil
	if valid {
		i, o := request.Instance, loop.runtime.opts
		valid = i.Scope != nil && i.Scope.WorkspaceId == o.Scope.WorkspaceID && i.Scope.EnvironmentId == o.Scope.EnvironmentID && i.Scope.ServiceKey == o.Scope.ServiceKey && i.InstanceId == o.InstanceID && i.BootId == o.BootID && i.BuildId == o.BuildID && i.ImageDigest == o.ImageDigest
	}
	registration.mu.Unlock()
	if !valid {
		return ErrScope
	}
	return RunRegistered(ctx, func(ctx context.Context) (Options, error) {
		o, err := options(ctx)
		if err != nil {
			return o, err
		}
		if o.Hello == nil {
			return o, ErrProtocol
		}
		o.Hello = proto.Clone(o.Hello).(*agentv1.Hello)
		for _, capability := range []string{"release.prepare", "release.activate", "release.revoke"} {
			if !slices.Contains(o.Hello.RequiredCapabilities, capability) {
				o.Hello.RequiredCapabilities = append(o.Hello.RequiredCapabilities, capability)
			}
		}
		return o, nil
	}, registration, loop.Serve)
}

// Serve consumes all Events for one session. Cancellation closes the session and
// waits for every worker, retaining ownership if a host callback ignores cancel.
// A second Serve is rejected until the previous workers have fully drained.
func (l *ReleaseLoop) Serve(ctx context.Context, c *Client) error {
	if l.set == nil || l.runtime == nil || c == nil || c.opts.Hello == nil || c.welcome == nil || c.welcome.GetWelcome() == nil {
		return ErrProtocol
	}
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		return ErrBackpressure
	}
	w := c.welcome.GetWelcome()
	o := l.runtime.opts
	if c.opts.Hello.InstanceId != o.InstanceID || c.opts.Hello.BootId != o.BootID || w.Scope == nil || w.Scope.WorkspaceId != o.Scope.WorkspaceID || w.Scope.EnvironmentId != o.Scope.EnvironmentID || w.Scope.ServiceKey != o.Scope.ServiceKey || w.ControlPlaneEpoch == 0 || l.epoch != 0 && l.epoch != w.ControlPlaneEpoch {
		l.mu.Unlock()
		return ErrScope
	}
	l.epoch = w.ControlPlaneEpoch
	l.running = true
	l.mu.Unlock()
	defer func() { l.mu.Lock(); l.running = false; l.mu.Unlock() }()
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan releaseTask, 6)
	var workers sync.WaitGroup
	active := map[releasePosition]bool{}
	var activationCancel context.CancelFunc
	var revocationActive bool
	// On reconnect only an already-created report is replayed without a new wire
	// event. New downloads/constructions require the server's current delivery.
	for lane, task := range l.pending {
		if task == nil {
			continue
		}
		keep := lane == releasePrepare && task.prepare.Prepared != nil && task.prepare.Receipt == nil || lane == releaseActivate && task.activation.Applied != nil && task.activation.Receipt == nil
		if !keep {
			l.pending[lane] = nil
		} else {
			task.due = time.Time{}
			task.waitPreparation = false
		}
	}
	defer func() {
		cancel()
		_ = c.Close()
		workers.Wait()
		for len(results) > 0 {
			l.finish(<-results)
		}
	}()
	timer := time.NewTicker(200 * time.Millisecond)
	defer timer.Stop()
	start := func(task releaseTask) {
		jobCtx := run
		var done context.CancelFunc
		if task.lane == releaseActivate {
			jobCtx, done = context.WithCancel(run)
			activationCancel = done
		}
		if task.lane == releasePrepare {
			active[task.position] = true
		}
		if task.lane == releaseRevoke {
			revocationActive = true
		}
		l.pending[task.lane] = nil
		workers.Go(func() {
			if done != nil {
				defer done()
			}
			task = l.process(jobCtx, c, task)
			results <- task
		})
	}
	for {
		now := time.Now()
		for lane, task := range l.pending {
			if task == nil || task.due.After(now) || task.waitPreparation {
				continue
			}
			if task.expires() <= now.UnixMilli() {
				task.err = ErrExpired
				l.pending[lane] = nil
				l.record(*task)
				continue
			}
			switch lane {
			case releasePrepare:
				if len(active) < 4 && !active[task.position] {
					start(*task)
				}
			case releaseActivate:
				if activationCancel == nil {
					start(*task)
				}
			case releaseRevoke:
				if !revocationActive {
					start(*task)
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.Done():
			return c.Err()
		case <-timer.C:
		case result := <-results:
			switch result.lane {
			case releasePrepare:
				delete(active, result.position)
			case releaseActivate:
				activationCancel = nil
			case releaseRevoke:
				revocationActive = false
			}
			l.finish(result)
			if result.position == l.heads[result.lane] && releaseSessionRejected(result.err) {
				return result.err
			}
		case event, ok := <-c.Events():
			if !ok {
				return c.Err()
			}
			if event.GetGoAway() != nil || event.GetError() != nil {
				select {
				case <-c.Done():
					return c.Err()
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			task, err := l.command(c, event)
			if err != nil {
				if errors.Is(err, ErrRollback) {
					continue
				}
				return err
			}
			head := l.heads[task.lane]
			if head != (releasePosition{}) && (task.position.major < head.major || task.position.major == head.major && task.position.minor < head.minor) {
				continue
			}
			if task.position.major == head.major && task.position.minor == head.minor && task.position != head {
				return ErrRollback
			}
			if task.position != head {
				if task.lane == releaseActivate && activationCancel != nil {
					activationCancel()
				}
				l.heads[task.lane] = task.position
			}
			if task.lane == releaseActivate && task.position == head {
				l.mu.Lock()
				previous := cloneRuntimeActivationResult(l.state.Activation)
				l.mu.Unlock()
				if previous.CommandID == task.event.OperationId && previous.Applied != nil {
					task.activation = previous
				}
			}
			l.pending[task.lane] = &task
		}
	}
}
func (t releaseTask) expires() int64 {
	switch t.lane {
	case releasePrepare:
		if t.prepare.Prepared != nil {
			return t.prepare.Prepared.ExpiresUnixMs
		}
		p := t.event.GetPrepare()
		return min(t.event.DeadlineUnixMs, p.ExpiresUnixMs, p.Bundle.ExpiresUnixMs)
	case releaseActivate:
		a := t.event.GetActivate()
		return min(t.event.DeadlineUnixMs, a.ExpiresUnixMs, a.View.ExpiresUnixMs)
	default:
		return min(t.event.DeadlineUnixMs, t.event.GetRevoke().View.ExpiresUnixMs)
	}
}
func (l *ReleaseLoop) process(ctx context.Context, c *Client, t releaseTask) releaseTask {
	switch t.lane {
	case releasePrepare:
		t.prepare, t.err = l.set.Prepare(ctx, c, t.event)
	case releaseActivate:
		if t.activation.Applied == nil {
			t.activation, t.err = c.InstallActivation(ctx, t.event, l.runtime)
		} else {
			current, err := l.runtime.Applied()
			if err != nil {
				t.err = err
			} else if !proto.Equal(current, t.activation.Applied) {
				t.err = ErrRollback
			} else {
				t.activation.Receipt, t.err = c.ReportApplied(ctx, t.event.OperationId, t.activation.Applied)
			}
		}
	case releaseRevoke:
		t.err = c.ApplyRevocation(ctx, t.event, l.runtime)
	}
	return t
}
func (l *ReleaseLoop) record(t releaseTask) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch t.lane {
	case releasePrepare:
		l.state.Preparation = clonePreparationResult(t.prepare)
		l.state.Preparation.Workload = nil
		l.state.Preparation.CommandID = t.event.OperationId
		l.state.PreparationError = t.err
	case releaseActivate:
		l.state.Activation = cloneRuntimeActivationResult(t.activation)
		l.state.Activation.CommandID = t.event.OperationId
		l.state.ActivationError = t.err
	case releaseRevoke:
		l.state.RevocationError = t.err
		if t.err == nil {
			l.state.RevocationEpoch = t.event.GetRevoke().Epoch
			l.state.RevocationArtifactRevision = t.event.GetRevoke().View.Revision
		}
	}
}
func (l *ReleaseLoop) finish(t releaseTask) {
	if t.position != l.heads[t.lane] {
		return
	}
	l.record(t)
	if t.lane == releasePrepare && t.prepare.Prepared != nil {
		if waiting := l.pending[releaseActivate]; waiting != nil {
			waiting.waitPreparation = false
		}
	}
	if t.err == nil {
		if queued := l.pending[t.lane]; queued != nil && queued.position == t.position {
			l.pending[t.lane] = nil
		}
		return
	}
	fixedReport := t.lane == releasePrepare && t.prepare.Prepared != nil && t.prepare.Receipt == nil || t.lane == releaseActivate && t.activation.Applied != nil && t.activation.Receipt == nil
	retry := releaseRetry(t.err) || fixedReport && releaseUnknownReport(t.err) || t.lane == releaseActivate && errors.Is(t.err, ErrRuntimeNotReady)
	if !retry || t.expires() <= time.Now().UnixMilli() {
		return
	}
	delay := time.Second
	if remote, ok := errors.AsType[*RemoteError](t.err); ok && remote.Detail != nil {
		delay = max(delay, time.Duration(remote.Detail.RetryAfterMs)*time.Millisecond)
	}
	if httpError, ok := errors.AsType[*HandshakeError](t.err); ok {
		delay = max(delay, httpError.RetryAfter)
	}
	t.due = time.Now().Add(delay)
	t.waitPreparation = t.lane == releaseActivate && errors.Is(t.err, ErrRuntimeNotReady)
	if t.waitPreparation {
		l.set.mu.Lock()
		t.waitPreparation = l.set.current == nil
		l.set.mu.Unlock()
	}
	if l.pending[t.lane] == nil {
		l.pending[t.lane] = &t
	}
}
func releaseUnknownReport(err error) bool {
	if _, ok := errors.AsType[*RemoteError](err); ok {
		return false
	}
	for _, known := range []error{ErrProtocol, ErrResource, ErrScope, ErrExpired, ErrRollback, ErrCacheClosed, ErrRuntimeResources, ErrRuntimeNotReady, ErrRuntimeUnavailable} {
		if errors.Is(err, known) {
			return false
		}
	}
	return err != nil
}
func releaseRetry(err error) bool {
	if httpError, ok := errors.AsType[*HandshakeError](err); ok {
		return httpError.StatusCode == 408 || httpError.StatusCode == 429 || httpError.StatusCode >= 500
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrBackpressure) || errors.Is(err, ErrDownload) {
		return true
	}
	remote, ok := errors.AsType[*RemoteError](err)
	return ok && remote.Detail != nil && remote.Detail.Retryable
}
func releaseSessionRejected(err error) bool {
	if httpError, ok := errors.AsType[*HandshakeError](err); ok {
		return httpError.StatusCode == 401 || httpError.StatusCode == 403
	}
	remote, ok := errors.AsType[*RemoteError](err)
	if !ok || remote.Detail == nil {
		return false
	}
	switch remote.Detail.Code {
	case agentv1.ErrorCode_ERROR_CODE_FENCED, agentv1.ErrorCode_ERROR_CODE_UNAUTHENTICATED, agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED:
		return true
	}
	return false
}
func normalizedReleaseRef(ref *agentv1.ResourceRef) *agentv1.ResourceRef {
	return &agentv1.ResourceRef{Kind: ref.Kind, Key: ref.Key, Revision: ref.Revision, Sha256: strings.ToLower(ref.Sha256), SizeBytes: ref.SizeBytes}
}
func (l *ReleaseLoop) command(c *Client, event *agentv1.Envelope) (releaseTask, error) {
	if event == nil {
		return releaseTask{}, ErrProtocol
	}
	event = proto.Clone(event).(*agentv1.Envelope)
	task := releaseTask{event: event}
	var normalized proto.Message
	if event.GetPrepare() != nil {
		frame, err := l.set.observe(c, event)
		if err != nil {
			return task, err
		}
		position, err := preparationPositionOf(c, frame)
		if err != nil {
			return task, err
		}
		task.event = frame
		task.position = releasePosition{major: position.generation, minor: position.target, command: position.command, digest: position.digest}
		return task, nil
	}
	if !discoveryID(event.OperationId) || event.DeadlineUnixMs <= 0 {
		return task, ErrProtocol
	}
	var ref *agentv1.ResourceRef
	switch {
	case event.GetActivate() != nil:
		task.lane = releaseActivate
		a := event.GetActivate()
		ref = a.View
		if a.Release == nil || a.Release.Generation == 0 || a.Release.ReleaseId != "release-"+strconv.FormatUint(a.Release.Generation, 10) || !bundleDigest(a.Release.BundleHash) || ref == nil || ref.Kind != agentv1.ResourceKind_RESOURCE_KIND_ACTIVATION || ref.Key != ActivationKey(a.Release.Generation) || a.PolicyRevision == 0 || a.PreparedRevision == 0 || a.DiscoveryRevision == 0 || a.ExpiresUnixMs <= 0 {
			return task, ErrProtocol
		}
		task.position = releasePosition{major: a.Release.Generation, minor: ref.Revision, command: event.OperationId}
		normalized = &agentv1.Activate{Release: &agentv1.ReleaseIdentity{ReleaseId: a.Release.ReleaseId, Generation: a.Release.Generation, BundleHash: a.Release.BundleHash}, PolicyRevision: a.PolicyRevision, PreparedRevision: a.PreparedRevision, View: normalizedReleaseRef(ref), DiscoveryRevision: a.DiscoveryRevision, RevocationEpoch: a.RevocationEpoch, ExpiresUnixMs: a.ExpiresUnixMs}
	case event.GetRevoke() != nil:
		task.lane = releaseRevoke
		r := event.GetRevoke()
		ref = r.View
		if r.Epoch == 0 || ref == nil || ref.Kind != agentv1.ResourceKind_RESOURCE_KIND_REVOCATION || ref.Key != RevocationKey || event.OperationId != "revoke-"+strconv.FormatUint(r.Epoch, 10)+"-"+strconv.FormatUint(ref.Revision, 10) {
			return task, ErrProtocol
		}
		task.position = releasePosition{major: r.Epoch, minor: ref.Revision, command: event.OperationId}
		normalized = &agentv1.Revoke{Epoch: r.Epoch, BundleHashes: r.BundleHashes, CancelActiveStreams: r.CancelActiveStreams, View: normalizedReleaseRef(ref)}
	default:
		return task, ErrProtocol
	}
	if ref.Revision == 0 || !bundleDigest(strings.ToLower(ref.Sha256)) || ref.SizeBytes == 0 || ref.ExpiresUnixMs <= 0 || !proto.Equal(ref.Scope, c.welcome.GetWelcome().Scope) {
		return task, ErrResource
	}
	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(normalized)
	if err != nil {
		return task, err
	}
	task.position.digest = sha256.Sum256(body)
	return task, nil
}
