package agentv1

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const (
	ProtocolMajor = 1
	ProtocolMinor = 0
	Subprotocol   = "grayling.agent.v1"
	ConnectPath   = "/api/v1/agent/connect"
	MaxFrameBytes = 256 << 10
)

// ProtocolError is safe to return to a peer: it never includes payload data.
type ProtocolError struct {
	Code         ErrorCode
	Reason       string
	Retryable    bool
	RetryAfterMs uint32
}

func (e *ProtocolError) Error() string { return fmt.Sprintf("agent: %s: %s", e.Code, e.Reason) }
func invalid(reason string) error {
	return &ProtocolError{Code: ErrorCode_ERROR_CODE_INVALID_ARGUMENT, Reason: reason}
}

// Decode validates a single binary frame before dispatch. A transport must also
// apply its read limit before buffering a frame. Domain authorization, deadlines,
// fencing and payload semantics must be checked by the application service.
func Decode(frame []byte) (*Envelope, error) {
	if len(frame) == 0 || len(frame) > MaxFrameBytes {
		return nil, invalid("invalid frame size")
	}
	// Reject duplicate oneof fields rather than allowing protobuf's last-one-wins
	// behavior to hide commands from intermediaries. Fields >=20 are reserved for
	// payload variants, including future variants this receiver cannot execute.
	payloads := 0
	for rest := frame; len(rest) > 0; {
		num, typ, n := protowire.ConsumeTag(rest)
		if n < 0 || typ == protowire.StartGroupType || typ == protowire.EndGroupType {
			return nil, invalid("malformed frame")
		}
		rest = rest[n:]
		n = protowire.ConsumeFieldValue(num, typ, rest)
		if n < 0 {
			return nil, invalid("malformed frame")
		}
		if num >= 20 {
			payloads++
			if typ != protowire.BytesType {
				return nil, invalid("invalid payload wire type")
			}
		}
		rest = rest[n:]
	}
	if payloads != 1 {
		return nil, invalid("expected exactly one payload")
	}
	e := new(Envelope)
	if err := (proto.UnmarshalOptions{RecursionLimit: 32}).Unmarshal(frame, e); err != nil {
		return nil, invalid("malformed frame")
	}
	if err := Validate(e); err != nil {
		return nil, err
	}
	return e, nil
}

// Encode enforces the same envelope invariants as Decode. Neither function
// treats protobuf's deterministic serialization as a signature encoding.
func Encode(e *Envelope) ([]byte, error) {
	if err := Validate(e); err != nil {
		return nil, err
	}
	if proto.Size(e) > MaxFrameBytes {
		return nil, invalid("invalid frame size")
	}
	b, err := proto.Marshal(e)
	if err != nil {
		return nil, invalid("invalid envelope")
	}
	// Unknown fields may contain a future payload supplied by a relay.
	if _, err := Decode(b); err != nil {
		return nil, err
	}
	return b, nil
}

// Validate checks transport-independent envelope invariants only.
func Validate(e *Envelope) error {
	if e == nil {
		return invalid("missing envelope")
	}
	if e.ProtocolMajor != ProtocolMajor || e.ProtocolMinor > ProtocolMinor {
		return &ProtocolError{Code: ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE, Reason: "unsupported protocol version"}
	}
	if e.Payload == nil || e.ProtoReflect().WhichOneof(e.ProtoReflect().Descriptor().Oneofs().ByName("payload")) == nil {
		return &ProtocolError{Code: ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE, Reason: "unsupported payload"}
	}
	payloadField := e.ProtoReflect().WhichOneof(e.ProtoReflect().Descriptor().Oneofs().ByName("payload"))
	if !e.ProtoReflect().Get(payloadField).Message().IsValid() {
		return invalid("missing payload body")
	}
	if e.MessageId == "" || len(e.MessageId) > 128 || len(e.ReplyTo) > 128 || len(e.OperationId) > 128 || len(e.SessionId) > 128 || len(e.SubscriptionId) > 128 || len(e.ResourceKey) > 1024 {
		return invalid("invalid envelope identifier")
	}
	if e.DeadlineUnixMs < 0 {
		return invalid("invalid deadline")
	}
	if _, ok := ResourceKind_name[int32(e.ResourceKind)]; !ok {
		return invalid("unknown resource kind")
	}
	if h, ok := e.Payload.(*Envelope_Hello); ok {
		if h == nil || h.Hello == nil || h.Hello.MinMinor > h.Hello.MaxMinor || h.Hello.MinMinor > ProtocolMinor {
			return &ProtocolError{Code: ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE, Reason: "unsupported minor range"}
		}
		if e.SessionId != "" || e.ConnectionEpoch != 0 {
			return invalid("hello cannot select session")
		}
		if h.Hello.AgentId == "" || h.Hello.InstanceId == "" || h.Hello.BootId == "" || len(h.Hello.AgentId) > 128 || len(h.Hello.InstanceId) > 128 || len(h.Hello.BootId) > 128 {
			return invalid("invalid hello identity")
		}
	} else if (e.SessionId == "" || e.ConnectionEpoch == 0) && !(e.GetError() != nil && e.SessionId == "" && e.ConnectionEpoch == 0) {
		return invalid("missing server session")
	}
	switch e.Payload.(type) {
	case *Envelope_Register, *Envelope_RenewLease, *Envelope_UpdateInstance, *Envelope_Deregister,
		*Envelope_ResourceApplied, *Envelope_ResourceRejected, *Envelope_Prepared, *Envelope_Applied,
		*Envelope_Rejected, *Envelope_ReadinessReport, *Envelope_DrainReport,
		*Envelope_Prepare, *Envelope_Activate, *Envelope_Abort, *Envelope_Revoke, *Envelope_DrainRequested:
		if e.OperationId == "" {
			return invalid("missing operation id")
		}
	}
	return nil
}
