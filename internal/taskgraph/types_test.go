package taskgraph

import "testing"

func TestTaskNodeTargets(t *testing.T) {
	task := &TaskNode{
		ID:               "t1",
		Agent:            "developer",
		Profile:          "developer",
		AssignedInstance: "developer-i-01",
	}

	if got := task.TargetProfile(); got != "developer" {
		t.Fatalf("TargetProfile() = %q, want developer", got)
	}
	if got := task.ExecutionTarget(); got != "developer-i-01" {
		t.Fatalf("ExecutionTarget() = %q, want developer-i-01", got)
	}
}

func TestTaskNodeTargetsFallbackToAgent(t *testing.T) {
	task := &TaskNode{ID: "t1", Agent: "architect"}

	if got := task.TargetProfile(); got != "architect" {
		t.Fatalf("TargetProfile() = %q, want architect", got)
	}
	if got := task.ExecutionTarget(); got != "architect" {
		t.Fatalf("ExecutionTarget() = %q, want architect", got)
	}
}
