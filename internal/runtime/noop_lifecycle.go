package runtime

import "context"

type NoopLifecycle struct{}

func NewNoopLifecycle() *NoopLifecycle {
	return &NoopLifecycle{}
}

func (l *NoopLifecycle) Spawn(_ context.Context, _ AgentDefinition, _ func(context.Context) error) error {
	return nil
}

func (l *NoopLifecycle) Teardown(_ context.Context, _ string) error {
	return nil
}

func (l *NoopLifecycle) TeardownAll(_ context.Context) error {
	return nil
}

func (l *NoopLifecycle) Wait(_ context.Context, _ string) error {
	return nil
}
