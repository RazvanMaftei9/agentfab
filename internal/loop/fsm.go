package loop

import "fmt"

// TransitionResult indicates the outcome of a successful FSM transition.
type TransitionResult int

const (
	// Transitioned indicates a normal state transition.
	Transitioned TransitionResult = iota
	// Escalated indicates the FSM auto-escalated due to reaching max transitions.
	Escalated
)

// FSM manages state transitions for an orchestration loop.
type FSM struct {
	def   LoopDefinition
	state LoopState
}

// NewFSM creates a new FSM from a loop definition.
func NewFSM(def LoopDefinition) (*FSM, error) {
	if err := Validate(def); err != nil {
		return nil, err
	}
	return &FSM{
		def: def,
		state: LoopState{
			LoopID:       def.ID,
			CurrentState: def.InitialState,
		},
	}, nil
}

// RestoreFSM creates an FSM from a saved state.
func RestoreFSM(def LoopDefinition, state LoopState) *FSM {
	return &FSM{def: def, state: state}
}

// State returns the current loop state.
func (f *FSM) State() LoopState {
	return f.state
}

// IsTerminal returns true if the current state is terminal.
func (f *FSM) IsTerminal() bool {
	for _, ts := range f.def.TerminalStates {
		if f.state.CurrentState == ts {
			return true
		}
	}
	return false
}

// Transition attempts to move to the given state.
// It returns (Transitioned, nil) on success, (Escalated, nil) when max
// transitions are reached (graceful budget exhaustion), or (0, err) for
// programming errors such as transitioning from a terminal state or
// attempting an invalid transition.
func (f *FSM) Transition(to string) (TransitionResult, error) {
	if f.IsTerminal() {
		return 0, fmt.Errorf("loop %q is in terminal state %q", f.def.ID, f.state.CurrentState)
	}

	if f.state.TransitionCount >= f.def.MaxTransitions {
		f.state.CurrentState = "ESCALATED"
		f.state.TransitionCount++
		return Escalated, nil
	}

	// Validate transition is allowed.
	valid := false
	for _, t := range f.def.Transitions {
		if t.From == f.state.CurrentState && t.To == to {
			valid = true
			break
		}
	}
	if !valid {
		return 0, fmt.Errorf("loop %q: invalid transition from %q to %q", f.def.ID, f.state.CurrentState, to)
	}

	f.state.CurrentState = to
	f.state.TransitionCount++
	return Transitioned, nil
}

// ForceEscalate moves the FSM to the ESCALATED terminal state.
func (f *FSM) ForceEscalate() {
	f.state.CurrentState = "ESCALATED"
	f.state.TransitionCount++
}

// Validate checks that a loop definition is well-formed.
func Validate(def LoopDefinition) error {
	if def.ID == "" {
		return fmt.Errorf("loop ID is required")
	}
	if len(def.States) == 0 {
		return fmt.Errorf("loop %q: at least one state is required", def.ID)
	}
	if def.InitialState == "" {
		return fmt.Errorf("loop %q: initial state is required", def.ID)
	}
	if len(def.TerminalStates) == 0 {
		return fmt.Errorf("loop %q: at least one terminal state is required", def.ID)
	}
	if def.MaxTransitions <= 0 {
		return fmt.Errorf("loop %q: max_transitions must be positive", def.ID)
	}

	names := def.StateNames()
	stateSet := make(map[string]bool, len(names))
	for _, s := range names {
		stateSet[s] = true
	}

	terminalSet := make(map[string]bool, len(def.TerminalStates))
	for _, ts := range def.TerminalStates {
		terminalSet[ts] = true
	}

	if !stateSet[def.InitialState] {
		return fmt.Errorf("loop %q: initial state %q not in states", def.ID, def.InitialState)
	}

	for _, ts := range def.TerminalStates {
		if !stateSet[ts] {
			return fmt.Errorf("loop %q: terminal state %q not in states", def.ID, ts)
		}
	}

	// Non-terminal states must have an agent assigned.
	for _, sc := range def.States {
		if !terminalSet[sc.Name] && sc.Agent == "" {
			return fmt.Errorf("loop %q: non-terminal state %q must have an agent", def.ID, sc.Name)
		}
	}

	// All participants must appear in at least one state.
	agentInState := make(map[string]bool)
	for _, sc := range def.States {
		if sc.Agent != "" {
			agentInState[sc.Agent] = true
		}
	}
	for _, p := range def.Participants {
		if !agentInState[p] {
			return fmt.Errorf("loop %q: participant %q not assigned to any state", def.ID, p)
		}
	}

	for _, t := range def.Transitions {
		if !stateSet[t.From] {
			return fmt.Errorf("loop %q: transition from unknown state %q", def.ID, t.From)
		}
		if !stateSet[t.To] {
			return fmt.Errorf("loop %q: transition to unknown state %q", def.ID, t.To)
		}
	}

	// ESCALATED must be a defined terminal state since the FSM falls back
	// to it when max transitions are reached.
	if !stateSet["ESCALATED"] {
		return fmt.Errorf("loop %q: \"ESCALATED\" must be in states (used as max-transition fallback)", def.ID)
	}
	if !terminalSet["ESCALATED"] {
		return fmt.Errorf("loop %q: \"ESCALATED\" must be in terminal_states (used as max-transition fallback)", def.ID)
	}

	// Every non-terminal state must have at least one outgoing transition.
	hasOutgoing := make(map[string]bool, len(def.States))
	for _, t := range def.Transitions {
		hasOutgoing[t.From] = true
	}
	for _, sc := range def.States {
		if !terminalSet[sc.Name] && !hasOutgoing[sc.Name] {
			return fmt.Errorf("loop %q: non-terminal state %q has no outgoing transitions", def.ID, sc.Name)
		}
	}

	return nil
}
