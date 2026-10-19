package agentv1_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func hello() *agentv1.Envelope {
	return &agentv1.Envelope{ProtocolMajor: 1, MessageId: "hello-1", Payload: &agentv1.Envelope_Hello{Hello: &agentv1.Hello{AgentId: "a", InstanceId: "i", BootId: "b"}}}
}
func TestFrameValidation(t *testing.T) {
	t.Parallel()
	valid, err := agentv1.Encode(hello())
	if err != nil {
		t.Fatal(err)
	}
	unknownField := protowire.AppendTag(bytes.Clone(valid), 19, protowire.VarintType)
	unknownField = protowire.AppendVarint(unknownField, 99)
	future := protowire.AppendTag(nil, 99, protowire.BytesType)
	future = protowire.AppendBytes(future, nil)
	unknownPayload := proto.Clone(hello()).(*agentv1.Envelope)
	unknownPayload.Payload = nil
	base, _ := proto.Marshal(unknownPayload)
	tests := []struct {
		name  string
		frame []byte
		code  agentv1.ErrorCode
	}{
		{"valid", valid, 0},
		{"unknown optional field", unknownField, 0},
		{"empty", nil, agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT},
		{"oversize", make([]byte, agentv1.MaxFrameBytes+1), agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT},
		{"truncated", valid[:len(valid)-1], agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT},
		{"duplicate command", append(bytes.Clone(valid), valid...), agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT},
		{"future command", append(base, future...), agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE},
		{"ambiguous future command", append(bytes.Clone(valid), future...), agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := agentv1.Decode(tt.frame)
			if tt.code == 0 {
				if err != nil || got.GetHello().GetBootId() != "b" {
					t.Fatalf("decode: %v %v", got, err)
				}
				return
			}
			pe, ok := errors.AsType[*agentv1.ProtocolError](err)
			if !ok || pe.Code != tt.code {
				t.Fatalf("got %v; want %v", err, tt.code)
			}
		})
	}
}
func TestEnvelopeInvariants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*agentv1.Envelope)
	}{
		{"typed nil hello", func(e *agentv1.Envelope) { e.Payload = (*agentv1.Envelope_Hello)(nil) }},
		{"typed nil register", func(e *agentv1.Envelope) { e.Payload = (*agentv1.Envelope_Register)(nil) }},
		{"nil heartbeat", func(e *agentv1.Envelope) {
			e.SessionId = "s"
			e.ConnectionEpoch = 1
			e.Payload = &agentv1.Envelope_Heartbeat{}
		}},
		{"major", func(e *agentv1.Envelope) { e.ProtocolMajor = 2 }},
		{"minor", func(e *agentv1.Envelope) { e.ProtocolMinor = 1 }},
		{"missing identity", func(e *agentv1.Envelope) { e.GetHello().BootId = "" }},
		{"client chosen session", func(e *agentv1.Envelope) { e.SessionId = "chosen" }},
		{"client chosen epoch", func(e *agentv1.Envelope) { e.ConnectionEpoch = 4 }},
		{"missing message id", func(e *agentv1.Envelope) { e.MessageId = "" }},
		{"missing session", func(e *agentv1.Envelope) { e.Payload = &agentv1.Envelope_Heartbeat{Heartbeat: &agentv1.Heartbeat{}} }},
		{"missing operation", func(e *agentv1.Envelope) {
			e.SessionId = "s"
			e.ConnectionEpoch = 1
			e.Payload = &agentv1.Envelope_Register{Register: &agentv1.Register{}}
		}},
		{"negative deadline", func(e *agentv1.Envelope) { e.DeadlineUnixMs = -1 }},
		{"unknown resource", func(e *agentv1.Envelope) { e.ResourceKind = 999 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := hello()
			tt.change(e)
			if _, err := agentv1.Encode(e); err == nil {
				t.Fatal("accepted invalid envelope")
			}
		})
	}
}
func TestRevisionJSONPrecision(t *testing.T) {
	t.Parallel()
	e := hello()
	e.ResourceRevision = 18446744073709551615
	e.StreamSeq = 9007199254740993
	b, err := protojson.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"18446744073709551615"`) || !strings.Contains(string(b), `"9007199254740993"`) {
		t.Fatalf("64-bit integer not encoded as decimal string: %s", b)
	}
	var round agentv1.Envelope
	if err := protojson.Unmarshal(b, &round); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(e, &round) {
		t.Fatal("json lost precision")
	}
}
func FuzzDecode(f *testing.F) {
	b, _ := agentv1.Encode(hello())
	f.Add(b)
	f.Add([]byte{0xff})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		e, err := agentv1.Decode(b)
		if err != nil {
			return
		}
		encoded, err := agentv1.Encode(e)
		if err != nil {
			t.Fatal(err)
		}
		round, err := agentv1.Decode(encoded)
		if err != nil || !proto.Equal(e, round) {
			t.Fatalf("roundtrip: %v", err)
		}
	})
}

func TestPreWelcomeError(t *testing.T) {
	e := &agentv1.Envelope{ProtocolMajor: 1, MessageId: "error-1", Payload: &agentv1.Envelope_Error{Error: &agentv1.Error{Code: agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE, Reason: "hello rejected"}}}
	b, err := agentv1.Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentv1.Decode(b); err != nil {
		t.Fatal(err)
	}
	e.ConnectionEpoch = 1
	if _, err := agentv1.Encode(e); err == nil {
		t.Fatal("partially assigned error session accepted")
	}
}

func TestPrepareTargetRevisionFullUint64(t *testing.T) {
	frame := hello()
	frame.SessionId = "session"
	frame.ConnectionEpoch = 1
	frame.OperationId = "prepare"
	frame.Payload = &agentv1.Envelope_Prepare{Prepare: &agentv1.Prepare{TargetRevision: ^uint64(0)}}
	body, err := agentv1.Encode(frame)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := agentv1.Decode(body)
	if err != nil || decoded.GetPrepare().TargetRevision != ^uint64(0) {
		t.Fatal("target revision lost precision", decoded, err)
	}
}
