package runtime

import "context"

// Communicator sends and receives messages between agents.
// Message is defined as any to avoid import cycles with the message package.
// Concrete implementations use *message.Message.
type Communicator interface {
	// Send delivers a message to another agent.
	Send(ctx context.Context, msg any) error
	// Receive returns a channel that yields incoming messages for this agent.
	Receive(ctx context.Context) <-chan any
}

// StorageTier identifies which storage tier to use.
type StorageTier int

const (
	TierShared  StorageTier = iota // Shared volume (all agents)
	TierAgent                      // Per-agent persistent volume
	TierScratch                    // Ephemeral scratch space
)

// Storage reads and writes files across the three storage tiers.
type Storage interface {
	// Read returns the contents of a file.
	Read(ctx context.Context, tier StorageTier, path string) ([]byte, error)
	// Write creates or overwrites a file.
	Write(ctx context.Context, tier StorageTier, path string, data []byte) error
	// Append appends data to a file, creating it if needed.
	Append(ctx context.Context, tier StorageTier, path string, data []byte) error
	// List returns file paths matching a glob pattern.
	List(ctx context.Context, tier StorageTier, pattern string) ([]string, error)
	// Exists checks whether a file exists.
	Exists(ctx context.Context, tier StorageTier, path string) (bool, error)
	// Delete removes a file or directory. Empty path removes the entire tier root.
	Delete(ctx context.Context, tier StorageTier, path string) error
	// TierDir returns the absolute path to the given storage tier's root directory.
	TierDir(tier StorageTier) string
	// SharedDir returns the absolute path to the shared storage directory.
	SharedDir() string
}

// Discovery resolves agent names to endpoints.
type Discovery interface {
	// Register adds or updates an agent endpoint.
	Register(ctx context.Context, name string, endpoint Endpoint) error
	// Resolve returns the endpoint for an agent.
	Resolve(ctx context.Context, name string) (Endpoint, error)
	// List returns all registered agent names.
	List(ctx context.Context) ([]string, error)
	// Deregister removes an agent from the registry.
	Deregister(ctx context.Context, name string) error
}

// Lifecycle manages agent spawn, checkpoint, and teardown.
type Lifecycle interface {
	// Spawn starts an agent. The run function is called in a new goroutine/container/pod.
	Spawn(ctx context.Context, def AgentDefinition, run func(ctx context.Context) error) error
	// Teardown signals an agent to stop and waits for completion.
	Teardown(ctx context.Context, name string) error
	// TeardownAll signals all agents to stop.
	TeardownAll(ctx context.Context) error
	// Wait blocks until the named agent exits.
	Wait(ctx context.Context, name string) error
}

// Meter records token usage and enforces budgets.
type Meter interface {
	// Record logs an LLM call's token usage.
	Record(ctx context.Context, record LLMCallRecord) error
	// Usage returns aggregated usage for an agent.
	Usage(ctx context.Context, agentName string) (UsageSummary, error)
	// AggregateUsage returns aggregated usage across all agents.
	AggregateUsage(ctx context.Context) (UsageSummary, error)
	// CheckBudget returns an error if the agent has exceeded its budget.
	CheckBudget(ctx context.Context, agentName string) error
	// SetBudget sets or updates the budget for an agent.
	SetBudget(ctx context.Context, agentName string, budget Budget) error
}

// ExtendedMeter extends Meter with query methods used by the conductor and UI.
type ExtendedMeter interface {
	Meter
	// ModelUsage returns per-model token usage aggregated across all agents.
	ModelUsage(ctx context.Context) []ModelUsage
	// AllAgentUsage returns per-agent summaries for all agents.
	AllAgentUsage(ctx context.Context) map[string]UsageSummary
	// AllRecords returns a copy of all LLM call records.
	AllRecords(ctx context.Context) []LLMCallRecord
	// RequestUsage returns usage for a specific request ID.
	RequestUsage(ctx context.Context, requestID string) UsageSummary
}

// ModelUsage tracks token usage for a specific LLM model.
type ModelUsage struct {
	Model           string
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	TotalTokens     int64
	TotalCalls      int64
}
