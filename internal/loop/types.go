package loop

// StateConfig maps a state name to the agent that handles it.
// Terminal states have an empty Agent.
type StateConfig struct {
	Name             string `json:"name" yaml:"name"`
	Agent            string `json:"agent,omitempty" yaml:"agent,omitempty"`
	ReviewGuidelines string `json:"review_guidelines,omitempty" yaml:"review_guidelines,omitempty"`
}

// LoopDefinition defines an orchestration loop as an FSM.
type LoopDefinition struct {
	ID             string        `json:"id" yaml:"id"`
	Participants   []string      `json:"participants" yaml:"participants"`
	States         []StateConfig `json:"states" yaml:"states"`
	Transitions    []Transition  `json:"transitions" yaml:"transitions"`
	InitialState   string        `json:"initial_state" yaml:"initial_state"`
	TerminalStates []string      `json:"terminal_states" yaml:"terminal_states"`
	MaxTransitions int           `json:"max_transitions" yaml:"max_transitions"`
}

// StateNames returns the state names for backward compatibility.
func (d *LoopDefinition) StateNames() []string {
	names := make([]string, len(d.States))
	for i, s := range d.States {
		names[i] = s.Name
	}
	return names
}

// ReviewGuidelinesForState returns the review guidelines for the given state, or empty string.
func (d *LoopDefinition) ReviewGuidelinesForState(state string) string {
	for _, s := range d.States {
		if s.Name == state {
			return s.ReviewGuidelines
		}
	}
	return ""
}

// AgentForState returns the agent assigned to the given state, or empty string.
func (d *LoopDefinition) AgentForState(state string) string {
	for _, s := range d.States {
		if s.Name == state {
			return s.Agent
		}
	}
	return ""
}

// Transition defines a valid state change.
type Transition struct {
	From      string `json:"from" yaml:"from"`
	To        string `json:"to" yaml:"to"`
	Condition string `json:"condition,omitempty" yaml:"condition,omitempty"`
}

// LoopState tracks the current state of an active loop.
type LoopState struct {
	LoopID          string `json:"loop_id"`
	CurrentState    string `json:"current_state"`
	TransitionCount int    `json:"transition_count"`
}
