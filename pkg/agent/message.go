package agent

import (
	"uuid"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Message wraps a generated client payload without a second hand-maintained
// message catalog. Call/Send supply session metadata; callers set operation and
// subscription IDs explicitly. The payload is copied to isolate ownership.
func Message(body proto.Message) (*agentv1.Envelope, error) {
	if body == nil || !body.ProtoReflect().IsValid() {
		return nil, ErrProtocol
	}
	e := &agentv1.Envelope{ProtocolMajor: agentv1.ProtocolMajor, MessageId: uuid.New().String()}
	value := proto.Clone(body).ProtoReflect()
	fields := e.ProtoReflect().Descriptor().Oneofs().ByName("payload").Fields()
	for i := range fields.Len() {
		field := fields.Get(i)
		if field.Message().FullName() == value.Descriptor().FullName() {
			e.ProtoReflect().Set(field, protoreflect.ValueOfMessage(value))
			if !clientMessage(e) {
				return nil, ErrProtocol
			}
			return e, nil
		}
	}
	return nil, ErrProtocol
}
