package agent

import (
	"context"
	"encoding/json/v2"
	"errors"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
	"strconv"
)

func (c *Channel) revocationUpdate(ctx context.Context, se Session, host string, delivered uint64) (*agentv1.Envelope, error) {
	resource, err := c.sessions.RevocationResource(ctx, se, 0, c.releaseVerifier)
	if detail, ok := errors.AsType[*agentv1.ProtocolError](err); ok && detail.Code == agentv1.ErrorCode_ERROR_CODE_NOT_READY {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if resource.Revision == delivered {
		return nil, nil
	}
	var signed agentv1.SignedResource
	if proto.Unmarshal(resource.Body, &signed) != nil {
		return nil, client.ErrResource
	}
	var d client.RevocationDocument
	// RevocationResource has already verified the exact signed catalog.
	if err := json.Unmarshal(signed.Payload, &d); err != nil {
		return nil, err
	}
	body := &agentv1.Revoke{Epoch: d.Epoch, BundleHashes: d.BundleHashes, CancelActiveStreams: len(d.CancelledHashes) > 0, View: resourceReference(host, se, agentv1.ResourceKind_RESOURCE_KIND_REVOCATION, client.RevocationKey, resource)}
	e := response(se, "", body)
	e.OperationId = "revoke-" + strconv.FormatUint(d.Epoch, 10) + "-" + strconv.FormatUint(resource.Revision, 10)
	e.DeadlineUnixMs = resource.ExpiresAt.UnixMilli()
	return e, nil
}
