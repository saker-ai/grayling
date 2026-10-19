package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/pkg/model"
)

const releasePrepareTimeout = 120 * time.Second
const releasePreparedTTL = 300 * time.Second

// ReleaseCapacity is immutable release intent. A demo must explicitly select a
// separate profile; the production minimum cannot silently fall below two.
// Zones apply to every build carried by the Bundle, including its default build.
type ReleaseCapacity struct {
	Profile                   string   `json:"profile"`
	RequiredZones             []string `json:"requiredZones"`
	MinimumPerBuildZone       uint32   `json:"minimumPerBuildZone"`
	MaximumFailureBasisPoints uint32   `json:"maximumFailureBasisPoints"`
}

func (c ReleaseCapacity) valid() bool {
	minimum := uint32(2)
	switch c.Profile {
	case "production-v1":
	case "demo-v1":
		minimum = 1
	default:
		return false
	}
	if c.MinimumPerBuildZone < minimum || c.MinimumPerBuildZone > 10000 || c.MaximumFailureBasisPoints > 10000 || len(c.RequiredZones) == 0 || len(c.RequiredZones) > 64 || !slices.IsSorted(c.RequiredZones) {
		return false
	}
	for i, zone := range c.RequiredZones {
		if !model.KeyPattern.MatchString(zone) || (i > 0 && c.RequiredZones[i-1] == zone) {
			return false
		}
	}
	return true
}

type ReleaseDefinition struct {
	Scope                  client.Scope          `json:"scope"`
	BundleHash             string                `json:"bundleHash"`
	BundleArtifactRevision uint64                `json:"bundleArtifactRevision,string"`
	Document               client.BundleDocument `json:"document"`
	Capacity               ReleaseCapacity       `json:"capacity"`
	PrepareTimeoutMs       int64                 `json:"prepareTimeoutMs"`
	PreparedTTLms          int64                 `json:"preparedTTLms"`
}

// ReleaseRecord is diagnostic state. Neither a creation receipt nor PREPARING
// grants membership, route activation, Prepared or Applied evidence.
type ReleaseRecord struct {
	Definition         ReleaseDefinition    `json:"definition"`
	ReleaseID          string               `json:"releaseId"`
	Generation         uint64               `json:"generation,string"`
	ControlPlaneEpoch  uint64               `json:"controlPlaneEpoch,string"`
	State              agentv1.ReleaseState `json:"state"`
	Revision           uint64               `json:"revision,string"`
	CreatedAt          time.Time            `json:"createdAt"`
	PrepareDeadline    time.Time            `json:"prepareDeadline"`
	ResourcesExpiresAt time.Time            `json:"resourcesExpiresAt"`
}

type StartReleaseRequest struct {
	Scope                    client.Scope
	PrincipalID, OperationID string
	ExpectedGeneration       uint64
	BundleHash               [32]byte
	BundleArtifactRevision   uint64
	Capacity                 ReleaseCapacity
	AuthorizationExpiresAt   time.Time
	AuthorizeNewRelease      func(context.Context, pgx.Tx) error
}

func releaseRecordValid(r ReleaseRecord) bool {
	d := r.Definition
	hash, err := hex.DecodeString(d.BundleHash)
	if err != nil || len(hash) != 32 || d.BundleHash != hex.EncodeToString(hash) || !scopeValid(d.Scope) || d.Scope != d.Document.Scope || d.BundleArtifactRevision == 0 || !d.Capacity.valid() || d.PrepareTimeoutMs != releasePrepareTimeout.Milliseconds() || d.PreparedTTLms != releasePreparedTTL.Milliseconds() || r.Generation == 0 || r.ReleaseID != releaseID(r.Generation) || r.ControlPlaneEpoch == 0 || r.ControlPlaneEpoch > math.MaxInt64 || r.ControlPlaneEpoch != d.Document.Policy.ControlPlaneEpoch || r.Revision == 0 || r.State < agentv1.ReleaseState_RELEASE_STATE_DRAFT || r.State > agentv1.ReleaseState_RELEASE_STATE_ROLLED_BACK {
		return false
	}
	body, err := client.MarshalBundle(d.Document)
	if err != nil {
		return false
	}
	digest := sha256.Sum256(body)
	return bytes.Equal(digest[:], hash) && !r.CreatedAt.IsZero() && r.PrepareDeadline.After(r.CreatedAt) && !r.PrepareDeadline.After(r.ResourcesExpiresAt) && !r.PrepareDeadline.After(r.CreatedAt.Add(releasePrepareTimeout)) && r.ResourcesExpiresAt.UnixMilli() <= d.Document.ValidUntilUnixMs
}
func encodeRelease(r ReleaseRecord) ([]byte, error) {
	if !releaseRecordValid(r) {
		return nil, client.ErrResource
	}
	body, err := json.Marshal(r, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	if len(body) > client.MaxResourceBytes {
		return nil, client.ErrResource
	}
	return body, nil
}
func decodeRelease(body []byte) (ReleaseRecord, error) {
	var r ReleaseRecord
	if len(body) > client.MaxResourceBytes || json.Unmarshal(body, &r, json.RejectUnknownMembers(true)) != nil {
		return r, client.ErrResource
	}
	canonical, err := encodeRelease(r)
	if err != nil || !bytes.Equal(body, canonical) {
		return ReleaseRecord{}, client.ErrResource
	}
	return r, nil
}

func releaseCapacityEqual(a, b ReleaseCapacity) bool {
	return a.Profile == b.Profile && a.MinimumPerBuildZone == b.MinimumPerBuildZone && a.MaximumFailureBasisPoints == b.MaximumFailureBasisPoints && slices.Equal(a.RequiredZones, b.RequiredZones)
}
