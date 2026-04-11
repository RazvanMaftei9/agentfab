package message

import "context"

type annotatedCommunicator struct {
	base       MessageCommunicator
	senderNode string
	instanceID string
}

func AnnotateSender(base MessageCommunicator, senderNode, instanceID string) MessageCommunicator {
	return &annotatedCommunicator{
		base:       base,
		senderNode: senderNode,
		instanceID: instanceID,
	}
}

func (c *annotatedCommunicator) Send(ctx context.Context, msg *Message) error {
	if msg != nil {
		if msg.Metadata == nil {
			msg.Metadata = make(map[string]string, 2)
		}
		msg.Metadata["sender_node"] = c.senderNode
		msg.Metadata["sender_instance"] = c.instanceID
	}
	return c.base.Send(ctx, msg)
}

func (c *annotatedCommunicator) Receive(ctx context.Context) <-chan *Message {
	return c.base.Receive(ctx)
}
