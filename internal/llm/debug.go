package llm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"
)

// DebugStore writes per-agent JSONL debug logs.
type DebugStore struct {
	mu     sync.Mutex
	dir    string
	agents map[string]*agentWriter
}

type agentWriter struct {
	inputEnc   *json.Encoder
	outputEnc  *json.Encoder
	inputFile  *os.File
	outputFile *os.File
	callSeq    int64
}

type inputRecord struct {
	Timestamp time.Time      `json:"ts"`
	CallID    int64          `json:"call_id"`
	Model     string         `json:"model"`
	Messages  []debugMessage `json:"messages"`
}

type outputRecord struct {
	Timestamp       time.Time    `json:"ts"`
	CallID          int64        `json:"call_id"`
	Model           string       `json:"model"`
	DurationMs      int64        `json:"duration_ms"`
	InputTokens     int64        `json:"input_tokens"`
	OutputTokens    int64        `json:"output_tokens"`
	CacheReadTokens int64        `json:"cache_read_tokens,omitempty"`
	FinishReason    string       `json:"finish_reason,omitempty"`
	Message         debugMessage `json:"message"`
}

type debugToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type debugMessage struct {
	Role      string          `json:"role"`
	Content   string          `json:"content"`
	ToolCalls []debugToolCall `json:"tool_calls,omitempty"`
}

func NewDebugStore(dir string) (*DebugStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create debug dir: %w", err)
	}
	return &DebugStore{dir: dir, agents: make(map[string]*agentWriter)}, nil
}

func (s *DebugStore) Log(agent, modelID string, duration time.Duration, input []*schema.Message, output *schema.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	aw, err := s.writerFor(agent)
	if err != nil {
		return
	}

	aw.callSeq++
	callID := aw.callSeq
	now := time.Now()

	inRec := inputRecord{
		Timestamp: now.Add(-duration),
		CallID:    callID,
		Model:     modelID,
	}
	for _, m := range input {
		inRec.Messages = append(inRec.Messages, toDebugMessage(m))
	}
	_ = aw.inputEnc.Encode(inRec)

	outRec := outputRecord{
		Timestamp:  now,
		CallID:     callID,
		Model:      modelID,
		DurationMs: duration.Milliseconds(),
		Message:    toDebugMessage(output),
	}
	if output.ResponseMeta != nil {
		if output.ResponseMeta.Usage != nil {
			outRec.InputTokens = int64(output.ResponseMeta.Usage.PromptTokens)
			outRec.OutputTokens = int64(output.ResponseMeta.Usage.CompletionTokens)
			if output.ResponseMeta.Usage.PromptTokenDetails.CachedTokens > 0 {
				outRec.CacheReadTokens = int64(output.ResponseMeta.Usage.PromptTokenDetails.CachedTokens)
			}
		}
		if output.ResponseMeta.FinishReason != "" {
			outRec.FinishReason = output.ResponseMeta.FinishReason
		}
	}
	_ = aw.outputEnc.Encode(outRec)
}

func (s *DebugStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for _, aw := range s.agents {
		if err := aw.inputFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := aw.outputFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.agents = nil
	return firstErr
}

// writerFor returns (or lazily creates) the agentWriter. Caller must hold s.mu.
func (s *DebugStore) writerFor(agent string) (*agentWriter, error) {
	if aw, ok := s.agents[agent]; ok {
		return aw, nil
	}

	agentDir := filepath.Join(s.dir, agent)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return nil, err
	}

	inputFile, err := os.OpenFile(filepath.Join(agentDir, "input.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	outputFile, err := os.OpenFile(filepath.Join(agentDir, "output.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		inputFile.Close()
		return nil, err
	}

	aw := &agentWriter{
		inputFile:  inputFile,
		outputFile: outputFile,
		inputEnc:   json.NewEncoder(inputFile),
		outputEnc:  json.NewEncoder(outputFile),
	}
	s.agents[agent] = aw
	return aw, nil
}

func toDebugMessage(m *schema.Message) debugMessage {
	dm := debugMessage{
		Role:    string(m.Role),
		Content: m.Content,
	}
	for _, tc := range m.ToolCalls {
		dm.ToolCalls = append(dm.ToolCalls, debugToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return dm
}
