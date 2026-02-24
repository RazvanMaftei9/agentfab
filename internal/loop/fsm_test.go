package loop

import "testing"

func reviewLoop() LoopDefinition {
	return LoopDefinition{
		ID:           "review-loop",
		Participants: []string{"developer", "architect"},
		States: []StateConfig{
			{Name: "WORKING", Agent: "developer"},
			{Name: "REVIEWING", Agent: "architect"},
			{Name: "REVISING", Agent: "developer"},
			{Name: "APPROVED", Agent: ""},
			{Name: "ESCALATED", Agent: ""},
		},
		Transitions: []Transition{
			{From: "WORKING", To: "REVIEWING"},
			{From: "REVIEWING", To: "APPROVED", Condition: "approved"},
			{From: "REVIEWING", To: "REVISING", Condition: "revise"},
			{From: "REVISING", To: "REVIEWING"},
		},
		InitialState:   "WORKING",
		TerminalStates: []string{"APPROVED", "ESCALATED"},
		MaxTransitions: 5,
	}
}

func TestFSMHappyPath(t *testing.T) {
	fsm, err := NewFSM(reviewLoop())
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	if fsm.State().CurrentState != "WORKING" {
		t.Fatalf("initial state: got %q", fsm.State().CurrentState)
	}

	result, err := fsm.Transition("REVIEWING")
	if err != nil {
		t.Fatalf("to REVIEWING: %v", err)
	}
	if result != Transitioned {
		t.Fatalf("to REVIEWING: got result %d, want Transitioned", result)
	}

	result, err = fsm.Transition("APPROVED")
	if err != nil {
		t.Fatalf("to APPROVED: %v", err)
	}
	if result != Transitioned {
		t.Fatalf("to APPROVED: got result %d, want Transitioned", result)
	}

	if !fsm.IsTerminal() {
		t.Fatal("should be terminal")
	}
	if fsm.State().TransitionCount != 2 {
		t.Errorf("transitions: got %d, want 2", fsm.State().TransitionCount)
	}
}

func TestFSMMaxTransitions(t *testing.T) {
	def := reviewLoop()
	def.MaxTransitions = 3

	fsm, _ := NewFSM(def)
	fsm.Transition("REVIEWING")
	fsm.Transition("REVISING")
	fsm.Transition("REVIEWING")

	// 4th transition should trigger escalation (not an error).
	result, err := fsm.Transition("REVISING")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != Escalated {
		t.Fatalf("result: got %d, want Escalated", result)
	}
	if fsm.State().CurrentState != "ESCALATED" {
		t.Errorf("state: got %q, want ESCALATED", fsm.State().CurrentState)
	}
	if !fsm.IsTerminal() {
		t.Fatal("ESCALATED should be terminal")
	}
}

func TestFSMInvalidTransition(t *testing.T) {
	fsm, _ := NewFSM(reviewLoop())
	_, err := fsm.Transition("APPROVED")
	if err == nil {
		t.Fatal("expected invalid transition error (WORKING -> APPROVED not defined)")
	}
}

func TestFSMTerminalBlocks(t *testing.T) {
	fsm, _ := NewFSM(reviewLoop())
	fsm.Transition("REVIEWING")
	fsm.Transition("APPROVED")

	_, err := fsm.Transition("WORKING")
	if err == nil {
		t.Fatal("expected error transitioning from terminal state")
	}
}

func TestFSMForceEscalate(t *testing.T) {
	fsm, _ := NewFSM(reviewLoop())
	fsm.ForceEscalate()
	if fsm.State().CurrentState != "ESCALATED" {
		t.Errorf("state: got %q", fsm.State().CurrentState)
	}
	if !fsm.IsTerminal() {
		t.Fatal("should be terminal")
	}
}

func TestValidateInvalidDefinition(t *testing.T) {
	tests := []struct {
		name string
		mod  func(d *LoopDefinition)
	}{
		{"empty ID", func(d *LoopDefinition) { d.ID = "" }},
		{"no states", func(d *LoopDefinition) { d.States = nil }},
		{"no initial", func(d *LoopDefinition) { d.InitialState = "" }},
		{"no terminal", func(d *LoopDefinition) { d.TerminalStates = nil }},
		{"zero max", func(d *LoopDefinition) { d.MaxTransitions = 0 }},
		{"bad initial", func(d *LoopDefinition) { d.InitialState = "NONEXISTENT" }},
		{"bad terminal", func(d *LoopDefinition) { d.TerminalStates = []string{"NONEXISTENT"} }},
		{"non-terminal without agent", func(d *LoopDefinition) {
			d.States[0] = StateConfig{Name: "WORKING", Agent: ""}
		}},
		{"participant not in any state", func(d *LoopDefinition) {
			d.Participants = append(d.Participants, "unknown-agent")
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := reviewLoop()
			tc.mod(&d)
			if err := Validate(d); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestStateNames(t *testing.T) {
	def := reviewLoop()
	names := def.StateNames()
	if len(names) != 5 {
		t.Fatalf("expected 5 state names, got %d", len(names))
	}
	if names[0] != "WORKING" || names[1] != "REVIEWING" {
		t.Errorf("unexpected names: %v", names)
	}
}

func TestAgentForState(t *testing.T) {
	def := reviewLoop()
	if got := def.AgentForState("WORKING"); got != "developer" {
		t.Errorf("WORKING agent: got %q", got)
	}
	if got := def.AgentForState("REVIEWING"); got != "architect" {
		t.Errorf("REVIEWING agent: got %q", got)
	}
	if got := def.AgentForState("APPROVED"); got != "" {
		t.Errorf("APPROVED agent: got %q, want empty", got)
	}
	if got := def.AgentForState("NONEXISTENT"); got != "" {
		t.Errorf("NONEXISTENT: got %q, want empty", got)
	}
}
