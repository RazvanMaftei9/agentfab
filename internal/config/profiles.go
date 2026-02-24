package config

import (
	"github.com/razvanmaftei/agentfab/defaults"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

// DefaultProfiles returns the built-in agent definitions loaded from embedded YAML files.
func DefaultProfiles() []runtime.AgentDefinition {
	defs, err := LoadAgentsDirFS(defaults.AgentFS, "agents")
	if err != nil {
		panic("load embedded default agents: " + err.Error())
	}
	return defs
}

// DefaultFabricDef returns a system definition using default profiles.
func DefaultFabricDef(name string) *FabricDef {
	td := &FabricDef{
		Fabric: FabricMeta{
			Name:    name,
			Version: 1,
		},
		AgentsDir: "defaults/agents",
		Defaults: FabricDefaults{
			EscalationTarget: "",
		},
	}
	if err := td.ResolveAgents(); err != nil {
		panic("resolve default agents: " + err.Error())
	}
	return td
}
