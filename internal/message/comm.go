package message

import "context"

// MessageCommunicator sends and receives typed messages between agents.
// Unlike runtime.Communicator (which uses any), this interface is strongly typed
// and used by both local (channel-based) and distributed (gRPC-based) transports.
type MessageCommunicator interface {
	Send(ctx context.Context, msg *Message) error
	Receive(ctx context.Context) <-chan *Message
}

// CommunicatorFactory creates and manages per-agent communicators.
type CommunicatorFactory interface {
	Register(agentName string) MessageCommunicator
	Deregister(agentName string)
}
