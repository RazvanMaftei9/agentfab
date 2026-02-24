package local

import (
	"context"
	"fmt"
	"sync"

	"github.com/razvanmaftei/agentfab/internal/message"
)

const channelBufferSize = 64

var (
	_ message.MessageCommunicator = (*Communicator)(nil)
	_ message.CommunicatorFactory = (*Hub)(nil)
)

// Communicator implements message passing using Go channels.
type Communicator struct {
	hub       *Hub
	agentName string
}

type Hub struct {
	mu       sync.RWMutex
	channels map[string]chan *message.Message
}

func NewHub() *Hub {
	return &Hub{channels: make(map[string]chan *message.Message)}
}

func (h *Hub) Register(agentName string) message.MessageCommunicator {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan *message.Message, channelBufferSize)
	h.channels[agentName] = ch
	return &Communicator{hub: h, agentName: agentName}
}

func (h *Hub) Deregister(agentName string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.channels[agentName]; ok {
		close(ch)
		delete(h.channels, agentName)
	}
}

func (c *Communicator) Send(_ context.Context, msg *message.Message) error {
	c.hub.mu.RLock()
	defer c.hub.mu.RUnlock()

	ch, ok := c.hub.channels[msg.To]
	if !ok {
		return fmt.Errorf("agent %q not registered", msg.To)
	}

	select {
	case ch <- msg:
		return nil
	default:
		return fmt.Errorf("agent %q message buffer full", msg.To)
	}
}

func (c *Communicator) Receive(_ context.Context) <-chan *message.Message {
	c.hub.mu.RLock()
	defer c.hub.mu.RUnlock()
	return c.hub.channels[c.agentName]
}
