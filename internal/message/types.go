package message

import "time"

// MessageType identifies the purpose of a message.
type MessageType string

const (
	TypeTaskAssignment     MessageType = "task_assignment"
	TypeTaskResult         MessageType = "task_result"
	TypeEscalation         MessageType = "escalation"
	TypeEscalationResponse MessageType = "escalation_response"
	TypeReviewRequest      MessageType = "review_request"
	TypeReviewResponse     MessageType = "review_response"
	TypeStatusUpdate       MessageType = "status_update"
	TypeUserQuery          MessageType = "user_query"    // agent → conductor
	TypeUserResponse       MessageType = "user_response" // conductor → agent
)

// Message is the Go-native inter-agent message type.
type Message struct {
	ID         string            `json:"id"`
	RequestID  string            `json:"request_id"`
	From       string            `json:"from"`
	To         string            `json:"to"`
	Type       MessageType       `json:"type"`
	Parts      []Part            `json:"parts"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	TokenUsage *TokenUsage       `json:"token_usage,omitempty"`
	Timestamp  time.Time         `json:"timestamp"`
}

// TokenUsage records tokens consumed by one or more LLM calls.
type TokenUsage struct {
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	TotalTokens     int64  `json:"total_tokens"`
	TotalCalls      int64  `json:"total_calls,omitempty"`
	CacheReadTokens int64  `json:"cache_read_tokens,omitempty"`
	Model           string `json:"model,omitempty"`
}

// Part is a content unit within a message.
type Part interface {
	partType() string
}

// TextPart contains plain text.
type TextPart struct {
	Text string `json:"text"`
}

func (TextPart) partType() string { return "text" }

// FilePart references a file on the shared volume.
type FilePart struct {
	URI      string `json:"uri"`
	MimeType string `json:"mime_type"`
	Name     string `json:"name"`
}

func (FilePart) partType() string { return "file" }

// DataPart contains structured JSON data.
type DataPart struct {
	Data map[string]any `json:"data"`
}

func (DataPart) partType() string { return "data" }
