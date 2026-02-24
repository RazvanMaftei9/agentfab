package taskgraph

import (
	"time"

	"github.com/razvanmaftei/agentfab/internal/loop"
)

type TaskStatus string

const (
	StatusPending   TaskStatus = "pending"
	StatusRunning   TaskStatus = "running"
	StatusCompleted TaskStatus = "completed"
	StatusFailed    TaskStatus = "failed"
	StatusEscalated TaskStatus = "escalated"
	StatusCancelled TaskStatus = "cancelled"
)

type TaskScope string

const (
	ScopeSmall    TaskScope = "small"
	ScopeStandard TaskScope = "standard"
	ScopeLarge    TaskScope = "large"
)

func (s TaskScope) EffectiveScope() TaskScope {
	switch s {
	case ScopeSmall, ScopeStandard, ScopeLarge:
		return s
	default:
		return ScopeStandard
	}
}

type ArtifactFile struct {
	URI      string `json:"uri"`
	MimeType string `json:"mime_type"`
	Name     string `json:"name"`
}

type TaskNode struct {
	ID            string         `json:"id" yaml:"id"`
	Agent         string         `json:"agent" yaml:"agent"`
	Description   string         `json:"description" yaml:"description"`
	DependsOn     []string       `json:"depends_on,omitempty" yaml:"depends_on,omitempty"`
	LoopID        string         `json:"loop_id,omitempty" yaml:"loop_id,omitempty"`
	Scope         TaskScope      `json:"scope,omitempty" yaml:"scope,omitempty"`
	Timeout       time.Duration  `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Budget        int64          `json:"budget,omitempty" yaml:"budget,omitempty"`
	Status        TaskStatus     `json:"status" yaml:"status"`
	Result        string         `json:"result,omitempty" yaml:"result,omitempty"`
	ArtifactURI   string         `json:"artifact_uri,omitempty" yaml:"artifact_uri,omitempty"`
	ArtifactFiles []ArtifactFile `json:"artifact_files,omitempty" yaml:"artifact_files,omitempty"`
	ChatContext   string         `json:"-" yaml:"-"` // transient: chat conversation for re-dispatch after amendment
}

type TaskGraph struct {
	RequestID string                `json:"request_id"`
	Name      string                `json:"name,omitempty"` // short descriptive name from decompose
	Tasks     []*TaskNode           `json:"tasks"`
	Loops     []loop.LoopDefinition `json:"loops,omitempty"`
}

func (g *TaskGraph) Get(id string) *TaskNode {
	for _, t := range g.Tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (g *TaskGraph) GetLoop(id string) *loop.LoopDefinition {
	for i := range g.Loops {
		if g.Loops[i].ID == id {
			return &g.Loops[i]
		}
	}
	return nil
}
