package event

import (
	"time"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

// Type identifies the kind of event.
type Type int

const (
	AgentStarting      Type = iota // Agent spawn initiated (waiting for readiness).
	AgentReady                     // Single agent goroutine is listening.
	AllAgentsReady                 // All non-conductor agents ready.
	RequestReceived                // Request received, pre-processing starting.
	RequestScreened                // Request screened as non-actionable.
	DecomposeStart                 // LLM decomposition begins.
	DecomposeEnd                   // Task graph ready.
	TaskStart                      // Task status → running.
	TaskComplete                   // Task done.
	TaskFailed                     // Task failed.
	LoopTransition                 // FSM state transition in a loop.
	KnowledgeStart                 // Knowledge generation begins.
	KnowledgeEnd                   // Knowledge generation done.
	TaskProgress                   // Streaming LLM output snippet for a running task.
	TaskAmended                    // Task description changed via user chat.
	AgentQueryReceived             // Agent asked user a question.
	AgentQueryAnswered             // User answered agent's question.
	RequestPaused                  // Execution paused by user.
	RequestResumed                 // Execution resumed by user.
	RequestCancelled               // Execution cancelled by user.
	RequestComplete                // All tasks done.
	CurationStarted                // Agent knowledge curation begins.
	CurationComplete               // Agent knowledge curation done.
	ColdStorageLookup              // Cold storage search performed.
	KnowledgeLookup                // Knowledge graph nodes read by an agent for a task.
	AgentSleep                     // Agent entering sleep/idle curation state.
	AgentWake                      // Agent waking from sleep state.
)

type KnowledgeNodeInfo struct {
	ID      string
	Agent   string
	Title   string
	Summary string
	Tags    []string
}

type KnowledgeRelInfo struct {
	KnowledgeNodeInfo
	Depth     int
	Relations []string
}

type KnowledgeEdgeInfo struct {
	From     string
	To       string
	Relation string
}

type TaskSummary struct {
	ID            string
	Agent         string
	ExecutionNode string
	Description   string
	DependsOn     []string
	Status        string // "pending", "running", "completed", "failed", "cancelled"
	LoopID        string // non-empty when the task participates in an orchestration loop
}

// Event carries a lifecycle signal from conductor/scheduler to the renderer.
type Event struct {
	Type      Type
	Timestamp time.Time

	AgentName  string
	AgentModel string

	Tasks []TaskSummary

	TaskID          string
	TaskAgent       string
	ExecutionNode   string
	TaskDescription string
	Duration        time.Duration
	ErrMsg          string
	ResultSummary   string

	LoopID    string
	FromState string
	ToState   string
	Verdict   string
	LoopCount int

	ScreenMessage string
	ProgressText  string

	AmendedTaskID string
	AmendedAgent  string

	QueryAgent string
	QueryText  string
	AnswerText string

	CancelReason string

	KnowledgeNodes int
	KnowledgeEdges int

	KnowledgeLookupAgent    string
	KnowledgeLookupTask     string
	KnowledgeLookupOwnNodes []KnowledgeNodeInfo
	KnowledgeLookupRelNodes []KnowledgeRelInfo
	KnowledgeLookupEdges    []KnowledgeEdgeInfo

	CurationAgent     string
	CurationNodesIn   int
	CurationNodesOut  int
	ColdStorageMoved  int
	ColdStoragePurged int

	TotalDuration    time.Duration
	HasFailures      bool
	FailureSummaries []string
	ModelUsages      []ModelUsage

	InputTokens       int64
	OutputTokens      int64
	CacheReadTokens   int64
	TotalCalls        int64
	AgentInputTokens  int64
	AgentOutputTokens int64
}

type ModelUsage = runtime.ModelUsage

// Bus is a buffered event channel. Nil-safe: methods on a nil Bus are no-ops.
type Bus chan Event

func NewBus() Bus {
	return make(Bus, 128)
}

// Emit sends an event non-blocking; drops if the buffer is full. Nil/closed-safe.
func (b Bus) Emit(e Event) {
	if b == nil {
		return
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	defer func() { recover() }()
	select {
	case b <- e:
	default:
	}
}

// Close closes the underlying channel. Nil-safe.
func (b Bus) Close() {
	if b == nil {
		return
	}
	close(b)
}
