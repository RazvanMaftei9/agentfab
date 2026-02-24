package message

import (
	"context"
	"encoding/json"
	"fmt"
)

// Appender appends data to a file. This is a subset of the Storage interface
// to avoid import cycles between message and runtime.
type Appender interface {
	Append(ctx context.Context, path string, data []byte) error
}

// Logger writes messages to the interaction log as JSONL.
type Logger struct {
	appender Appender
}

// NewLogger creates a message logger.
func NewLogger(appender Appender) *Logger {
	return &Logger{appender: appender}
}

// Log appends a message to the request's interaction log.
func (l *Logger) Log(ctx context.Context, msg *Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	data = append(data, '\n')

	path := fmt.Sprintf("logs/%s.jsonl", msg.RequestID)
	return l.appender.Append(ctx, path, data)
}
