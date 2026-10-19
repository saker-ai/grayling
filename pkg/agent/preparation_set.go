package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"sync"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

type preparationPosition struct {
	epoch, generation, target uint64
	command                   string
	digest                    [32]byte
}
type preparationJob struct {
	position preparationPosition
	trust    uint64
	cancel   context.CancelFunc
	done     chan struct{}
	result   PreparationResult
	err      error
}

// PreparationSet selects one current candidate by authenticated control-plane
// epoch, release generation and target revision. It owns all returned Workloads.
// Four constructions and eight not-yet-closed candidates bound resource retention;
// cancellation never replaces an uninterruptible callback with unbounded work.
type PreparationSet struct {
	trust                   uint64
	mu                      sync.Mutex
	opts                    PreparationOptions
	position                preparationPosition
	current                 *PreparedWorkload
	job                     *preparationJob
	operations              map[*preparationJob]bool
	owned                   map[*PreparedWorkload]bool
	operationsWG, cleanupWG sync.WaitGroup
	callsWG                 sync.WaitGroup
	lifetime                context.Context
	cancel                  context.CancelFunc
	closed                  bool
	done                    chan struct{}
	closeErr                error
	failed                  error
}

func NewPreparationSet(o PreparationOptions) (*PreparationSet, error) {
	if !preparationOptionsValid(o) {
		return nil, ErrResource
	}
	life, cancel := context.WithCancel(context.Background())
	return &PreparationSet{lifetime: life, cancel: cancel, opts: o, trust: 1, operations: map[*preparationJob]bool{}, owned: map[*PreparedWorkload]bool{}, done: make(chan struct{})}, nil
}
func preparationPositionOf(c *Client, event *agentv1.Envelope) (preparationPosition, error) {
	p := event.GetPrepare()
	if p.TargetRevision == 0 {
		return preparationPosition{}, ErrProtocol
	}
	// Origin/session, a download reference's lifetime and optional wire fields can
	// change on reconnect. Only immutable command/resource identity is fingerprinted.
	normalized := &agentv1.Prepare{Release: &agentv1.ReleaseIdentity{ReleaseId: p.Release.ReleaseId, Generation: p.Release.Generation, BundleHash: p.Release.BundleHash}, Bundle: &agentv1.ResourceRef{Kind: p.Bundle.Kind, Key: p.Bundle.Key, Revision: p.Bundle.Revision, Sha256: strings.ToLower(p.Bundle.Sha256), SizeBytes: p.Bundle.SizeBytes}, ExpiresUnixMs: p.ExpiresUnixMs, TargetRevision: p.TargetRevision}
	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(normalized)
	if err != nil {
		return preparationPosition{}, err
	}
	epoch := c.welcome.GetWelcome().ControlPlaneEpoch
	body = append(binary.BigEndian.AppendUint64([]byte("grayling.agent.v1.prepare-selection\x00"), epoch), body...)
	return preparationPosition{epoch: epoch, generation: p.Release.Generation, target: p.TargetRevision, command: event.OperationId, digest: sha256.Sum256(body)}, nil
}
func (p preparationPosition) newerThan(old preparationPosition) bool {
	return old.epoch == 0 || p.epoch == old.epoch && (p.generation > old.generation || p.generation == old.generation && p.target > old.target)
}
func clonePreparationResult(r PreparationResult) PreparationResult {
	if r.Prepared != nil {
		r.Prepared = proto.Clone(r.Prepared).(*agentv1.Prepared)
	}
	if r.Receipt != nil {
		r.Receipt = proto.Clone(r.Receipt).(*agentv1.PreparedAck)
	}
	return r
}
func (s *PreparationSet) ownLocked(p *PreparedWorkload) {
	if p == nil || s.owned[p] {
		return
	}
	s.owned[p] = true
	s.cleanupWG.Go(func() {
		<-p.done
		p.mu.Lock()
		err := p.closeErr
		p.mu.Unlock()
		s.mu.Lock()
		delete(s.owned, p)
		if err != nil {
			if s.closeErr == nil {
				s.closeErr = err
			}
			s.cancel()
			for job := range s.operations {
				job.cancel()
			}
			if s.current != nil {
				s.current.Retire()
				s.current = nil
			}
		}
		s.mu.Unlock()
	})
}

func (s *PreparationSet) begin(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, ErrCacheClosed
	}
	if s.closeErr != nil {
		return nil, nil, ErrRuntimeResources
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.lifetime, cancel)
	s.callsWG.Add(1)
	return ctx, func() { stop(); cancel(); s.callsWG.Done() }, nil
}

// Prepare deduplicates one command, retries its exact report after reconnect,
// and lets a newer command cancel older work without waiting for its builder.
// A local preparation is retained after an unknown remote receipt; it becomes
// usable only when a matching signed activation is subsequently installed.
func (s *PreparationSet) Prepare(ctx context.Context, c *Client, event *agentv1.Envelope) (PreparationResult, error) {
	var empty PreparationResult
	ctx, done, err := s.begin(ctx)
	if err != nil {
		return empty, err
	}
	defer done()
	s.mu.Lock()
	o := s.opts
	s.mu.Unlock()
	event, err = preparationEvent(c, event, o)
	if err != nil {
		return empty, err
	}
	position, err := preparationPositionOf(c, event)
	if err != nil {
		return empty, err
	}
	s.mu.Lock()
	if err := s.observeLocked(position); err != nil {
		s.mu.Unlock()
		return empty, err
	}
	if s.failed != nil {
		err := s.failed
		result := empty
		if s.job != nil {
			result = clonePreparationResult(s.job.result)
			result.Receipt = nil
		}
		s.mu.Unlock()
		return result, err
	}
	if job := s.job; job != nil {
		select {
		case <-job.done:
			if job.result.Prepared != nil {
				retained := clonePreparationResult(job.result)
				trust := s.trust
				s.mu.Unlock()
				retained.Receipt, err = retained.Workload.RetryPrepared(ctx, c)
				s.mu.Lock()
				valid := !s.closed && s.closeErr == nil && s.position == position && s.current == retained.Workload && s.trust == trust
				if valid && preparationRejected(err) {
					s.failed = err
					s.current.Retire()
					s.current = nil
				}
				s.mu.Unlock()
				if !valid {
					return retained, ErrRollback
				}
				return retained, err
			}
		default:
			s.mu.Unlock()
			select {
			case <-job.done:
			case <-ctx.Done():
				return empty, ctx.Err()
			}
			if job.result.Prepared == nil {
				return clonePreparationResult(job.result), job.err
			}
			// Joining callers retry only after the first construction has finished.
			return s.Prepare(ctx, c, event)
		}
	}
	if len(s.operations) >= 4 || len(s.owned)+len(s.operations) >= 8 {
		s.mu.Unlock()
		return empty, ErrBackpressure
	}
	jobCtx, cancel := context.WithCancel(ctx)
	o = s.opts
	job := &preparationJob{trust: s.trust, position: position, cancel: cancel, done: make(chan struct{})}
	s.job = job
	s.operations[job] = true
	s.operationsWG.Add(1)
	s.mu.Unlock()
	result, err := c.PrepareRelease(jobCtx, event, o)
	cancel()
	s.mu.Lock()
	s.ownLocked(result.Workload)
	valid := !s.closed && s.closeErr == nil && s.position == position && s.trust == job.trust
	if valid && result.Prepared != nil && !preparationRejected(err) {
		s.current = result.Workload
	} else if result.Workload != nil {
		result.Workload.Retire()
	}
	if valid && preparationRejected(err) {
		s.failed = err
	}
	if !valid {
		err = ErrRollback
	}
	job.result = clonePreparationResult(result)
	job.err = err
	delete(s.operations, job)
	close(job.done)
	s.mu.Unlock()
	s.operationsWG.Done()
	return result, err
}

func preparationRejected(err error) bool {
	remote, ok := errors.AsType[*RemoteError](err)
	if !ok || remote.Detail == nil {
		return false
	}
	switch remote.Detail.Code {
	case agentv1.ErrorCode_ERROR_CODE_FENCED, agentv1.ErrorCode_ERROR_CODE_CONFLICT, agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, agentv1.ErrorCode_ERROR_CODE_UNAUTHENTICATED, agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE, agentv1.ErrorCode_ERROR_CODE_STALE_REVISION:
		return true
	}
	return false
}

// BuildRuntime can stay installed as one RuntimeOptions.Builder while Prepare
// replaces candidates. Previously installed views retain their own references.
func (s *PreparationSet) BuildRuntime(ctx context.Context, spec RuntimeSpec) (RuntimeResources, error) {
	s.mu.Lock()
	current := s.current
	position := s.position
	trust := s.trust
	closed := s.closed
	failed := s.closeErr != nil
	s.mu.Unlock()
	if closed {
		return RuntimeResources{}, ErrCacheClosed
	}
	if failed {
		return RuntimeResources{}, ErrRuntimeResources
	}
	if current == nil {
		return RuntimeResources{}, ErrRuntimeNotReady
	}
	resources, err := current.BuildRuntime(ctx, spec)
	if err != nil {
		return resources, err
	}
	s.mu.Lock()
	valid := !s.closed && s.closeErr == nil && s.current == current && s.position == position && s.trust == trust
	s.mu.Unlock()
	if !valid {
		return resources, ErrRollback
	}
	return resources, nil
}
func (s *PreparationSet) UpdateTrustRoots(v *Verifier) error {
	if v == nil {
		return ErrResource
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrCacheClosed
	}
	if s.trust == math.MaxUint64 {
		return ErrRollback
	}
	s.trust++
	s.opts.Verifier = v
	for job := range s.operations {
		job.cancel()
	}
	if s.current != nil {
		if err := s.current.UpdateTrustRoots(v); err != nil && !errors.Is(err, ErrCacheClosed) {
			return err
		}
	}
	return nil
}
func (s *PreparationSet) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.cancel()
		for job := range s.operations {
			job.cancel()
		}
		if s.current != nil {
			s.current.Retire()
			s.current = nil
		}
		go func() {
			s.callsWG.Wait()
			s.operationsWG.Wait()
			s.mu.Lock()
			for p := range s.owned {
				p.Retire()
			}
			s.mu.Unlock()
			s.cleanupWG.Wait()
			close(s.done)
		}()
	}
	s.mu.Unlock()
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// observe advances the selection barrier before a bounded event dispatcher can
// start construction. It does not create resources or report preparation.
func (s *PreparationSet) observe(c *Client, event *agentv1.Envelope) (*agentv1.Envelope, error) {
	s.mu.Lock()
	opts := s.opts
	s.mu.Unlock()
	event, err := preparationEvent(c, event, opts)
	if err != nil {
		return nil, err
	}
	position, err := preparationPositionOf(c, event)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.observeLocked(position); err != nil {
		return nil, err
	}
	return event, nil
}
func (s *PreparationSet) observeLocked(position preparationPosition) error {
	if s.closed {
		return ErrCacheClosed
	}
	if s.closeErr != nil {
		return ErrRuntimeResources
	}
	newer := position.newerThan(s.position)
	if !newer && position != s.position {
		return ErrRollback
	}
	if newer {
		s.position = position
		if s.current != nil {
			s.current.Retire()
			s.current = nil
		}
		for job := range s.operations {
			job.cancel()
		}
		s.job = nil
		s.failed = nil
	}
	return nil
}
