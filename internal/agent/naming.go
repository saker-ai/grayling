package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"maps"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godeps/gonacos/pkg/naming"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

// NamingBackend mutates a directory projection, then verifies exact readback.
// Implementations must isolate lease and revision keys, support idempotent removal,
// honor transport deadlines, and never probe an advertised application address.
type NamingBackend interface {
	Upsert(context.Context, NamingIntent) error
	Remove(context.Context, NamingIntent) error
	Verify(context.Context, NamingIntent) error
}

var ErrNamingProjection = errors.New("agent: naming projection not verified")

// EmbeddedNaming calls the pinned in-process Naming service. Its API has no
// context-aware calls: cancellation is checked at boundaries; caller deadlines
// cannot interrupt substrate sync/push callbacks. Do not claim a hard shutdown
// bound for this adapter until that substrate behavior has been verified.
type EmbeddedNaming struct{ service *naming.Service }

func NewEmbeddedNaming(service *naming.Service) (*EmbeddedNaming, error) {
	if service == nil {
		return nil, ErrNamingProjection
	}
	return &EmbeddedNaming{service: service}, nil
}
func namingIdentity(intent NamingIntent) (naming.Instance, error) {
	if !validInstanceDescriptor(intent.Instance, intent.Scope) || intent.Instance.Lease == nil || !identifier(intent.Instance.Lease.Id) || intent.Instance.Lease.Epoch == 0 || intent.Revision == 0 {
		return naming.Instance{}, ErrNamingProjection
	}
	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(intent.Instance)
	if err != nil {
		return naming.Instance{}, err
	}
	digest := sha256.Sum256(body)
	encoded := []byte("grayling.agent.naming-scope.v1\x00")
	for _, value := range []string{intent.Scope.WorkspaceID, intent.Scope.EnvironmentID, intent.Scope.ServiceKey} {
		encoded = binary.BigEndian.AppendUint16(encoded, uint16(len(value)))
		encoded = append(encoded, value...)
	}
	namespace := sha256.Sum256(encoded)
	instance := intent.Instance
	projectionID := instance.Lease.Id + "-r" + strconv.FormatUint(intent.Revision, 10)
	return naming.Instance{NamespaceID: "grayling-agent-" + hex.EncodeToString(namespace[:]), GroupName: "GRAYLING_AGENT", ServiceName: intent.Scope.ServiceKey, ClusterName: "lease-" + projectionID, InstanceID: projectionID, IP: instance.Address, Port: int(instance.Port), Weight: 1, Healthy: instance.Ready && !instance.Draining, Enabled: instance.Ready && !instance.Draining, Ephemeral: true, Metadata: map[string]string{
		"graylingManaged": "v1", "instanceId": instance.InstanceId, "bootId": instance.BootId, "buildId": instance.BuildId, "imageDigest": instance.ImageDigest, "tlsServerName": instance.TlsServerName, "zone": instance.Zone, "protocolVersion": strconv.FormatUint(uint64(instance.ProtocolVersion), 10), "leaseId": instance.Lease.Id, "leaseEpoch": strconv.FormatUint(instance.Lease.Epoch, 10), "expiresUnixMs": strconv.FormatInt(instance.Lease.ExpiresUnixMs, 10), "intentRevision": strconv.FormatUint(intent.Revision, 10), "descriptorSha256": hex.EncodeToString(digest[:]),
	}}, nil
}
func (b *EmbeddedNaming) Upsert(ctx context.Context, intent NamingIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	instance, err := namingIdentity(intent)
	if err != nil {
		return err
	}
	if !intent.Present || instance.Metadata["expiresUnixMs"] == "0" || time.Now().UnixMilli() >= intent.Instance.Lease.ExpiresUnixMs {
		return ErrNamingProjection
	}
	if _, err := b.service.RegisterInstance(instance); err != nil {
		return ErrNamingProjection
	}
	return ctx.Err()
}
func (b *EmbeddedNaming) Remove(ctx context.Context, intent NamingIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	instance, err := namingIdentity(intent)
	if err != nil {
		return err
	}
	if intent.Present {
		return ErrNamingProjection
	}
	err = b.service.DeregisterInstance(instance.NamespaceID, instance.GroupName, instance.ServiceName, instance.ClusterName, instance.IP, instance.Port, instance.InstanceID)
	if err != nil && !errors.Is(err, naming.ErrInstanceNotFound) {
		return ErrNamingProjection
	}
	return ctx.Err()
}
func (b *EmbeddedNaming) Verify(ctx context.Context, intent NamingIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	expected, err := namingIdentity(intent)
	if err != nil {
		return err
	}
	instances, err := b.service.ListInstances(expected.NamespaceID, expected.GroupName, expected.ServiceName, expected.ClusterName, false)
	if errors.Is(err, naming.ErrServiceNotFound) && !intent.Present {
		return nil
	}
	if err != nil {
		return ErrNamingProjection
	}
	found := false
	for _, instance := range instances {
		if instance.InstanceID != expected.InstanceID {
			continue
		}
		if !intent.Present || found {
			return ErrNamingProjection
		}
		found = true
		if instance.NamespaceID != expected.NamespaceID || instance.GroupName != expected.GroupName || instance.ServiceName != expected.ServiceName || instance.ClusterName != expected.ClusterName || instance.IP != expected.IP || instance.Port != expected.Port || instance.Healthy != expected.Healthy || instance.Enabled != expected.Enabled || !instance.Ephemeral || instance.Weight != expected.Weight || !maps.Equal(instance.Metadata, expected.Metadata) {
			return ErrNamingProjection
		}
	}
	if intent.Present && (!found || time.Now().UnixMilli() >= intent.Instance.Lease.ExpiresUnixMs) {
		return ErrNamingProjection
	}
	return ctx.Err()
}

type NamingReconcileResult struct{ Claimed, Confirmed, Failed, Stale int }

// ReconcileNaming advances authority expiry first, claims a bounded fair batch,
// performs directory calls outside DB transactions, and confirms with revision
// fencing. A failed/readback-mismatched projection never becomes confirmed.
func (s *Sessions) ReconcileNaming(ctx context.Context, scope client.Scope, backend NamingBackend, parallelism int) (NamingReconcileResult, error) {
	if backend == nil || parallelism < 1 || parallelism > 32 {
		return NamingReconcileResult{}, ErrNamingProjection
	}
	if _, err := s.ExpireInstances(ctx, scope); err != nil {
		return NamingReconcileResult{}, err
	}
	intents, err := s.PendingNamingIntents(ctx, scope)
	if err != nil {
		return NamingReconcileResult{}, err
	}
	jobs := make(chan NamingIntent, len(intents))
	for _, intent := range intents {
		jobs <- intent
	}
	close(jobs)
	var confirmed, failed, stale atomic.Int64
	var workers sync.WaitGroup
	for range min(parallelism, len(intents)) {
		workers.Go(func() {
			for intent := range jobs {
				attempt, done := context.WithTimeout(ctx, 2*time.Second)
				var err error
				if intent.Present {
					err = backend.Upsert(attempt, intent)
				} else {
					err = backend.Remove(attempt, intent)
				}
				if err == nil {
					err = backend.Verify(attempt, intent)
				}
				if err == nil {
					err = s.ConfirmNamingIntent(attempt, intent)
				}
				done()
				if err == nil {
					confirmed.Add(1)
				} else if isFenced(err) {
					stale.Add(1)
				} else {
					failed.Add(1)
				}
			}
		})
	}
	workers.Wait()
	result := NamingReconcileResult{Claimed: len(intents), Confirmed: int(confirmed.Load()), Failed: int(failed.Load()), Stale: int(stale.Load())}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if result.Failed > 0 {
		return result, ErrNamingProjection
	}
	return result, nil
}

func isFenced(err error) bool {
	detail, ok := errors.AsType[*agentv1.ProtocolError](err)
	return ok && detail.Code == agentv1.ErrorCode_ERROR_CODE_FENCED
}
