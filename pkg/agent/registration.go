package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

// RegistrationState describes control-plane registration only. Neither a lease
// nor Connected proves application health, an installed view or route readiness.
type RegistrationState struct {
	Instance              *agentv1.Instance
	DiscoveryRevision     uint64
	Connected, LeaseValid bool
}

// InstanceRegistration owns one immutable deployment/boot's registration and
// exact in-flight mutation across reconnects. It is process-local; no disk ACK
// restores readiness. Create a new owner only after explicitly authorizing a new
// registration. The owner allows one RunRegistered call at a time.
type InstanceRegistration struct {
	mu                 sync.Mutex
	request            *agentv1.Register
	registerOperation  string
	instance           *agentv1.Instance
	revision, epoch    uint64
	deadline           time.Time
	renewOperation     string
	renewRequest       *agentv1.RenewLease
	running, connected bool
	agentID            string
}

func NewInstanceRegistration(request *agentv1.Register) (*InstanceRegistration, error) {
	if request == nil || request.Instance == nil || proto.Size(request) > 32<<10 {
		return nil, ErrProtocol
	}
	request = proto.Clone(request).(*agentv1.Register)
	i := request.Instance
	if i.Scope == nil || !discoveryID(i.Scope.WorkspaceId) || !discoveryID(i.Scope.EnvironmentId) || !discoveryID(i.Scope.ServiceKey) || !discoveryID(i.InstanceId) || !discoveryID(i.BootId) || !discoveryID(i.BuildId) || i.Lease != nil || i.ProtocolVersion != 1 || i.Port == 0 || i.Port > 65535 || i.Address == "" || i.TlsServerName == "" || !strings.HasPrefix(i.ImageDigest, "sha256:") || !bundleDigest(strings.TrimPrefix(i.ImageDigest, "sha256:")) || !discoveryID(string(request.DeploymentAuthorization)) {
		return nil, ErrProtocol
	}
	return &InstanceRegistration{request: request, registerOperation: uuid.New().String()}, nil
}

func (r *InstanceRegistration) validLocked() bool {
	return r.instance != nil && time.Now().Before(r.deadline) && time.Now().UnixMilli() < r.instance.Lease.ExpiresUnixMs-5000
}
func (r *InstanceRegistration) State() RegistrationState {
	r.mu.Lock()
	defer r.mu.Unlock()
	var i *agentv1.Instance
	if r.instance != nil {
		i = proto.Clone(r.instance).(*agentv1.Instance)
	}
	return RegistrationState{Instance: i, DiscoveryRevision: r.revision, Connected: r.connected, LeaseValid: r.validLocked()}
}

// RunRegistered adds exact registration replay and independent lease renewal to
// Run. serve owns consuming Events and must stop when its context is canceled.
// An unresponsive serve retains ownership and blocks return/reconnect; no
// replacement callback is spawned behind it. The caller still owns Runtime and
// PreparationSet and must drain them after RunRegistered returns.
func RunRegistered(ctx context.Context, options func(context.Context) (Options, error), registration *InstanceRegistration, serve func(context.Context, *Client) error) error {
	if registration == nil || options == nil || serve == nil {
		return ErrProtocol
	}
	r := registration
	r.mu.Lock()
	if r.request == nil || r.request.Instance == nil {
		r.mu.Unlock()
		return ErrProtocol
	}
	if r.instance != nil && !r.validLocked() {
		r.mu.Unlock()
		return ErrExpired
	}
	if r.running {
		r.mu.Unlock()
		return ErrBackpressure
	}
	r.running = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.running = false; r.connected = false; r.mu.Unlock() }()
	return Run(ctx, func(ctx context.Context) (Options, error) {
		o, err := options(ctx)
		if err != nil {
			return o, err
		}
		o, err = o.normalized()
		if err != nil {
			return o, err
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if o.Hello.InstanceId != r.request.Instance.InstanceId || o.Hello.BootId != r.request.Instance.BootId || r.agentID != "" && r.agentID != o.Hello.AgentId {
			return o, ErrScope
		}
		r.agentID = o.Hello.AgentId
		for _, capability := range []string{"instance.register", "instance.renew"} {
			if !slices.Contains(o.Hello.RequiredCapabilities, capability) {
				o.Hello.RequiredCapabilities = append(o.Hello.RequiredCapabilities, capability)
			}
		}
		return o, nil
	}, func(c *Client) error { return r.serve(ctx, c, serve) })
}

func (r *InstanceRegistration) serve(ctx context.Context, c *Client, serve func(context.Context, *Client) error) error {
	w := c.welcome.GetWelcome()
	limits := w.Limits
	r.mu.Lock()
	valid := proto.Equal(w.Scope, r.request.Instance.Scope) && (r.epoch == 0 || r.epoch == w.ControlPlaneEpoch)
	if valid {
		r.epoch = w.ControlPlaneEpoch
	}
	r.mu.Unlock()
	if !valid {
		return ErrScope
	}
	if limits.LeaseTtlMs == 0 || limits.LeaseTtlMs > 60000 || limits.LeaseRenewIntervalMs == 0 || limits.LeaseRenewIntervalMs >= limits.LeaseTtlMs || limits.LeaseRenewIntervalMs > 20000 {
		return ErrProtocol
	}
	if err := r.mutate(ctx, c); err != nil {
		return err
	}
	r.mu.Lock()
	r.connected = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.connected = false; r.mu.Unlock() }()
	session, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Go(func() { results <- r.maintain(session, c, time.Duration(limits.LeaseRenewIntervalMs)*time.Millisecond) })
	workers.Go(func() { results <- serve(session, c) })
	err := <-results
	sessionErr := c.Err()
	cancel()
	_ = c.Close()
	r.mu.Lock()
	r.connected = false
	r.mu.Unlock()
	workers.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if sessionErr != nil && errors.Is(err, sessionErr) {
		return sessionErr
	}
	return err
}

func (r *InstanceRegistration) maintain(ctx context.Context, c *Client, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		r.mu.Lock()
		left := time.Until(r.deadline)
		live := r.validLocked()
		r.mu.Unlock()
		if !live {
			return ErrExpired
		}
		timer.Reset(min(interval, max(100*time.Millisecond, left/3)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.Done():
			return c.Err()
		case <-timer.C:
		}
		if err := r.mutate(ctx, c); err != nil {
			return err
		}
	}
}

// mutate serializes one pending operation under the single runner. A reconnect
// never invents a second logical mutation after a lost response.
func (r *InstanceRegistration) mutate(ctx context.Context, c *Client) error {
	r.mu.Lock()
	initial := r.instance == nil
	operation := r.registerOperation
	var renewal *agentv1.RenewLease
	if !initial {
		if !r.validLocked() {
			r.mu.Unlock()
			return ErrExpired
		}
		if r.renewRequest == nil {
			r.renewOperation = uuid.New().String()
			r.renewRequest = &agentv1.RenewLease{InstanceId: r.instance.InstanceId, BootId: r.instance.BootId, Lease: proto.Clone(r.instance.Lease).(*agentv1.Lease)}
		}
		operation = r.renewOperation
		renewal = proto.Clone(r.renewRequest).(*agentv1.RenewLease)
	}
	r.mu.Unlock()
	retry := time.NewTimer(time.Hour)
	defer retry.Stop()
	delay := time.Second
	for {
		var instance *agentv1.Instance
		var revision uint64
		var lease *agentv1.Lease
		var err error
		if initial {
			var response *agentv1.Registered
			response, err = c.Register(ctx, operation, r.request)
			if err == nil {
				instance = response.Instance
				revision = response.DiscoveryRevision
				if instance != nil {
					lease = instance.Lease
				}
			}
		} else {
			var response *agentv1.LeaseRenewed
			response, err = c.RenewLease(ctx, operation, renewal)
			if err == nil {
				lease = response.Lease
				revision = response.DiscoveryRevision
			}
		}
		if err == nil {
			return r.accept(c, instance, lease, revision, initial)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if c.Err() != nil {
			return c.Err()
		}
		remote, ok := errors.AsType[*RemoteError](err)
		if !errors.Is(err, context.DeadlineExceeded) && !(ok && remote.Detail != nil && remote.Detail.Retryable) {
			return err
		}
		wait := delay
		if ok {
			wait = max(wait, time.Duration(remote.Detail.RetryAfterMs)*time.Millisecond)
		}
		if !initial {
			r.mu.Lock()
			left := time.Until(r.deadline)
			r.mu.Unlock()
			if left <= wait {
				return ErrExpired
			}
		}
		retry.Reset(wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.Done():
			return c.Err()
		case <-retry.C:
		}
		delay = min(5*time.Second, delay*2)
	}
}

func (r *InstanceRegistration) accept(c *Client, instance *agentv1.Instance, lease *agentv1.Lease, revision uint64, initial bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if lease == nil || !discoveryID(lease.Id) || lease.Epoch == 0 || revision == 0 || lease.ExpiresUnixMs > now.UnixMilli()+int64(c.welcome.GetWelcome().Limits.LeaseTtlMs)+5000 {
		return ErrProtocol
	}
	if lease.ExpiresUnixMs <= now.UnixMilli()+5000 {
		return ErrExpired
	}
	if initial {
		if instance == nil {
			return ErrProtocol
		}
		identity := proto.Clone(instance).(*agentv1.Instance)
		identity.Lease = nil
		if !proto.Equal(identity, r.request.Instance) {
			return ErrScope
		}
	} else {
		if lease.Id != r.instance.Lease.Id || lease.Epoch != r.instance.Lease.Epoch || lease.ExpiresUnixMs < r.instance.Lease.ExpiresUnixMs || revision < r.revision {
			return ErrRollback
		}
		instance = proto.Clone(r.instance).(*agentv1.Instance)
		instance.Lease = proto.Clone(lease).(*agentv1.Lease)
	}
	deadline := now.Add(time.Duration(lease.ExpiresUnixMs-now.UnixMilli()-5000) * time.Millisecond)
	if r.instance != nil && lease.ExpiresUnixMs == r.instance.Lease.ExpiresUnixMs && r.deadline.Before(deadline) {
		deadline = r.deadline
	}
	r.instance = proto.Clone(instance).(*agentv1.Instance)
	r.revision = revision
	// Conservative clock allowance and a monotonic deadline prevent wall-clock
	// rollback or repeated reads from stretching local lease validity.
	r.deadline = deadline
	r.renewRequest = nil
	r.renewOperation = ""
	return nil
}
