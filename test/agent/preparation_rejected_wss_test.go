//go:build integration

package agent_test

import (
	"errors"
	"strings"
	"testing"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

func TestPreparationWSSNegativeOutcomeCapabilityAndGate(t *testing.T) {
	f, _, v, r := preparedFixture(t)
	intent := queueGateTarget(t, f, v, "wire-negative", 0)
	srv, _ := channelServerWithOptions(t, f.b, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(srv.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: srv.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.prepare"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if command := clientEvent(t, c); command.OperationId != intent.CommandID {
		t.Fatal("wrong command")
	}
	report := rejectionReport(r)
	_, err = c.ReportPrepareRejected(t.Context(), intent.CommandID, report)
	if remote, ok := errors.AsType[*client.RemoteError](err); !ok || remote.Detail.Code != agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE {
		t.Fatal("unnegotiated negative ACK", err)
	}
	// New client opts in to the negative receipt extension; older preparation
	// clients never receive a field-74 payload they cannot decode.
	opts.Hello.RequiredCapabilities = append(opts.Hello.RequiredCapabilities, "release.prepare.reject")
	successor, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer successor.Close()
	clientEvent(t, successor)
	ack, err := successor.ReportPrepareRejected(t.Context(), intent.CommandID, report)
	if err != nil || ack.EvidenceRevision != 1 || ack.CommandId != intent.CommandID {
		t.Fatal("negative receipt missing", err)
	}
	replay, err := successor.ReportPrepareRejected(t.Context(), intent.CommandID, report)
	if err != nil || !proto.Equal(ack, replay) {
		t.Fatal("negative receipt renewed", err)
	}
	_, err = successor.ReportPrepared(t.Context(), intent.CommandID, preparedReport(f, r))
	if remote, ok := errors.AsType[*client.RemoteError](err); !ok || remote.Detail.Code != agentv1.ErrorCode_ERROR_CODE_CONFLICT {
		t.Fatal("positive overwrote negative", err)
	}
	current, err := f.a.EvaluatePreparation(t.Context(), gateRequest(f, "wire-gate", 1), v)
	if err != nil || current.Eligible || current.Failed != 1 || current.Release.State != agentv1.ReleaseState_RELEASE_STATE_PREPARING {
		t.Fatal("wire NACK bypassed capacity", current, err)
	}
}
