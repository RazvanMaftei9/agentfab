package runtime

import "context"

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxTaskID
	ctxLoopID
	ctxLoopState
	ctxAgentName
)

// WithRequestID returns a context carrying the given request ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxRequestID, id)
}

// RequestIDFrom extracts the request ID from the context, or "".
func RequestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxRequestID).(string)
	return v
}

// WithTaskID returns a context carrying the given task ID.
func WithTaskID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxTaskID, id)
}

// TaskIDFrom extracts the task ID from the context, or "".
func TaskIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxTaskID).(string)
	return v
}

// WithLoopID returns a context carrying the given loop ID.
func WithLoopID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxLoopID, id)
}

// LoopIDFrom extracts the loop ID from the context, or "".
func LoopIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxLoopID).(string)
	return v
}

// WithLoopState returns a context carrying the given loop state name.
func WithLoopState(ctx context.Context, state string) context.Context {
	return context.WithValue(ctx, ctxLoopState, state)
}

// LoopStateFrom extracts the loop state from the context, or "".
func LoopStateFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxLoopState).(string)
	return v
}

// WithAgentName returns a context carrying the given agent name.
func WithAgentName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, ctxAgentName, name)
}

// AgentNameFrom extracts the agent name from the context, or "".
func AgentNameFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxAgentName).(string)
	return v
}
