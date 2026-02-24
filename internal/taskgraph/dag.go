package taskgraph

import (
	"fmt"

	"github.com/razvanmaftei/agentfab/internal/loop"
)

// Validate checks that the task graph is a valid DAG.
func (g *TaskGraph) Validate() error {
	ids := make(map[string]bool, len(g.Tasks))
	for _, t := range g.Tasks {
		if t.ID == "" {
			return fmt.Errorf("task has empty ID")
		}
		if ids[t.ID] {
			return fmt.Errorf("duplicate task ID %q", t.ID)
		}
		ids[t.ID] = true
	}

	for _, t := range g.Tasks {
		for _, dep := range t.DependsOn {
			if !ids[dep] {
				return fmt.Errorf("task %q depends on unknown task %q", t.ID, dep)
			}
			if dep == t.ID {
				return fmt.Errorf("task %q depends on itself", t.ID)
			}
		}
	}

	if _, err := g.TopologicalSort(); err != nil {
		return err
	}

	loopIDs := make(map[string]bool, len(g.Loops))
	for _, l := range g.Loops {
		if err := loop.Validate(l); err != nil {
			return fmt.Errorf("loop validation: %w", err)
		}
		loopIDs[l.ID] = true
	}

	for _, t := range g.Tasks {
		if t.LoopID != "" && !loopIDs[t.LoopID] {
			return fmt.Errorf("task %q references unknown loop %q", t.ID, t.LoopID)
		}
	}

	return nil
}

// ValidateWithRoster validates the graph and checks that every task agent is in the roster.
func (g *TaskGraph) ValidateWithRoster(roster []string) error {
	if err := g.Validate(); err != nil {
		return err
	}

	known := make(map[string]bool, len(roster))
	for _, name := range roster {
		known[name] = true
	}

	for _, t := range g.Tasks {
		if t.Agent != "" && !known[t.Agent] {
			return fmt.Errorf("task %q assigned to unknown agent %q (known: %v)", t.ID, t.Agent, roster)
		}
	}

	for _, l := range g.Loops {
		for _, sc := range l.States {
			if sc.Agent != "" && !known[sc.Agent] {
				return fmt.Errorf("loop %q state %q assigned to unknown agent %q", l.ID, sc.Name, sc.Agent)
			}
		}
	}

	return nil
}

// TopologicalSort returns task IDs in a valid execution order.
func (g *TaskGraph) TopologicalSort() ([]string, error) {
	inDegree := make(map[string]int, len(g.Tasks))
	dependents := make(map[string][]string, len(g.Tasks))

	for _, t := range g.Tasks {
		inDegree[t.ID] = len(t.DependsOn)
		for _, dep := range t.DependsOn {
			dependents[dep] = append(dependents[dep], t.ID)
		}
	}

	var queue []string
	for _, t := range g.Tasks {
		if inDegree[t.ID] == 0 {
			queue = append(queue, t.ID)
		}
	}

	var sorted []string
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		sorted = append(sorted, id)

		for _, dep := range dependents[id] {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}

	if len(sorted) != len(g.Tasks) {
		return nil, fmt.Errorf("task graph contains a cycle")
	}

	return sorted, nil
}

// ReadyTasks returns pending tasks whose dependencies are all completed or escalated.
func (g *TaskGraph) ReadyTasks() []*TaskNode {
	completed := make(map[string]bool)
	for _, t := range g.Tasks {
		if t.Status == StatusCompleted || t.Status == StatusEscalated {
			completed[t.ID] = true
		}
	}

	var ready []*TaskNode
	for _, t := range g.Tasks {
		if t.Status != StatusPending {
			continue
		}
		allDone := true
		for _, dep := range t.DependsOn {
			if !completed[dep] {
				allDone = false
				break
			}
		}
		if allDone {
			ready = append(ready, t)
		}
	}
	return ready
}

func (g *TaskGraph) AllDone() bool {
	for _, t := range g.Tasks {
		if t.Status == StatusPending || t.Status == StatusRunning {
			return false
		}
	}
	return true
}

// FailDependents cascades failure to all transitive dependents of the given task.
func (g *TaskGraph) FailDependents(failedID string) []string {
	dependents := make(map[string][]string, len(g.Tasks))
	for _, t := range g.Tasks {
		for _, dep := range t.DependsOn {
			dependents[dep] = append(dependents[dep], t.ID)
		}
	}

	var cascaded []string
	queue := []string{failedID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, childID := range dependents[id] {
			child := g.Get(childID)
			if child == nil || child.Status == StatusFailed || child.Status == StatusCompleted {
				continue
			}
			child.Status = StatusFailed
			child.Result = fmt.Sprintf("dependency %q failed", failedID)
			cascaded = append(cascaded, childID)
			queue = append(queue, childID)
		}
	}
	return cascaded
}

// HasIncompleteDependents returns true if any transitive dependent is non-completed.
func (g *TaskGraph) HasIncompleteDependents(taskID string) bool {
	dependents := make(map[string][]string, len(g.Tasks))
	for _, t := range g.Tasks {
		for _, dep := range t.DependsOn {
			dependents[dep] = append(dependents[dep], t.ID)
		}
	}

	visited := make(map[string]bool)
	queue := []string{taskID}
	visited[taskID] = true
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, childID := range dependents[id] {
			if visited[childID] {
				continue
			}
			visited[childID] = true
			child := g.Get(childID)
			if child != nil && child.Status != StatusCompleted {
				return true
			}
			queue = append(queue, childID)
		}
	}
	return false
}

// HasRunningDependents returns true if any transitive dependent is currently running.
func (g *TaskGraph) HasRunningDependents(taskID string) bool {
	dependents := make(map[string][]string, len(g.Tasks))
	for _, t := range g.Tasks {
		for _, dep := range t.DependsOn {
			dependents[dep] = append(dependents[dep], t.ID)
		}
	}

	visited := make(map[string]bool)
	queue := []string{taskID}
	visited[taskID] = true
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, childID := range dependents[id] {
			if visited[childID] {
				continue
			}
			visited[childID] = true
			child := g.Get(childID)
			if child != nil && child.Status == StatusRunning {
				return true
			}
			queue = append(queue, childID)
		}
	}
	return false
}

// FailureSummaries returns a description for each failed/escalated/cancelled task.
func (g *TaskGraph) FailureSummaries() []string {
	var summaries []string
	for _, t := range g.Tasks {
		switch t.Status {
		case StatusFailed:
			if t.Result != "" {
				summaries = append(summaries, fmt.Sprintf("Task %s (%s): %s", t.ID, t.Agent, t.Result))
			} else {
				summaries = append(summaries, fmt.Sprintf("Task %s (%s): failed", t.ID, t.Agent))
			}
		case StatusEscalated:
			if len(t.ArtifactFiles) == 0 && t.ArtifactURI == "" {
				summaries = append(summaries, fmt.Sprintf("Task %s (%s): escalated (review loop exhausted, no artifacts)", t.ID, t.Agent))
			}
		case StatusCancelled:
			summaries = append(summaries, fmt.Sprintf("Task %s (%s): cancelled", t.ID, t.Agent))
		}
	}
	return summaries
}

// HasFailures returns true if any task failed, was cancelled, or escalated without artifacts.
func (g *TaskGraph) HasFailures() bool {
	for _, t := range g.Tasks {
		switch t.Status {
		case StatusFailed, StatusCancelled:
			return true
		case StatusEscalated:
			if len(t.ArtifactFiles) == 0 && t.ArtifactURI == "" {
				return true
			}
		}
	}
	return false
}
