package local

import (
	"context"
	"fmt"
	"sync"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

type Lifecycle struct {
	mu     sync.Mutex
	agents map[string]*agentHandle
}

type agentHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func NewLifecycle() *Lifecycle {
	return &Lifecycle{agents: make(map[string]*agentHandle)}
}

func (l *Lifecycle) Spawn(ctx context.Context, def runtime.AgentDefinition, run func(ctx context.Context) error) error {
	if run == nil {
		return fmt.Errorf("agent %q: run function is nil (local lifecycle requires a run function)", def.Name)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if _, exists := l.agents[def.Name]; exists {
		return fmt.Errorf("agent %q already running", def.Name)
	}

	agentCtx, cancel := context.WithCancel(ctx)
	handle := &agentHandle{
		cancel: cancel,
		done:   make(chan struct{}),
	}

	go func() {
		defer close(handle.done)
		handle.err = run(agentCtx)
	}()

	l.agents[def.Name] = handle
	return nil
}

func (l *Lifecycle) Teardown(ctx context.Context, name string) error {
	l.mu.Lock()
	handle, ok := l.agents[name]
	l.mu.Unlock()

	if !ok {
		return fmt.Errorf("agent %q not found", name)
	}

	handle.cancel()
	select {
	case <-handle.done:
	case <-ctx.Done():
		return ctx.Err()
	}

	l.mu.Lock()
	delete(l.agents, name)
	l.mu.Unlock()

	return handle.err
}

func (l *Lifecycle) TeardownAll(ctx context.Context) error {
	l.mu.Lock()
	handles := make(map[string]*agentHandle, len(l.agents))
	for k, v := range l.agents {
		handles[k] = v
	}
	l.mu.Unlock()

	for _, handle := range handles {
		handle.cancel()
	}

	var firstErr error
	for name, handle := range handles {
		select {
		case <-handle.done:
		case <-ctx.Done():
			if firstErr == nil {
				firstErr = ctx.Err()
			}
			return firstErr
		}
		if handle.err != nil && firstErr == nil {
			firstErr = handle.err
		}
		l.mu.Lock()
		delete(l.agents, name)
		l.mu.Unlock()
	}

	return firstErr
}

func (l *Lifecycle) Wait(_ context.Context, name string) error {
	l.mu.Lock()
	handle, ok := l.agents[name]
	l.mu.Unlock()

	if !ok {
		return fmt.Errorf("agent %q not found", name)
	}

	<-handle.done
	return handle.err
}
