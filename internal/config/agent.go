package config

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
var validModel = regexp.MustCompile(`^[a-z0-9-]+/[a-z0-9._-]+$`)

// ValidProjectName returns true if name is lowercase alphanumeric with hyphens, starting with a letter.
func ValidProjectName(name string) bool {
	return validName.MatchString(name)
}

// ValidateAgentDefinition checks that an agent definition is well-formed.
func ValidateAgentDefinition(def runtime.AgentDefinition) error {
	if def.Name == "" {
		return fmt.Errorf("agent name is required")
	}
	if !validName.MatchString(def.Name) {
		return fmt.Errorf("agent name %q must be lowercase alphanumeric with hyphens, starting with a letter", def.Name)
	}
	if def.Purpose == "" {
		return fmt.Errorf("agent %q: purpose is required", def.Name)
	}
	if len(def.Capabilities) == 0 {
		return fmt.Errorf("agent %q: at least one capability is required", def.Name)
	}
	if def.Model == "" {
		return fmt.Errorf("agent %q: model is required", def.Name)
	}
	if !validModel.MatchString(def.Model) {
		return fmt.Errorf("agent %q: model %q must use provider/model-id format (e.g., anthropic/claude-opus-4)", def.Name, def.Model)
	}
	if def.EscalationTarget != "" && !validName.MatchString(def.EscalationTarget) {
		return fmt.Errorf("agent %q: escalation_target %q is not a valid agent name", def.Name, def.EscalationTarget)
	}
	for key, value := range def.RequiredNodeLabels {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("agent %q: required_node_labels contains an empty key", def.Name)
		}
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("agent %q: required_node_labels[%q] has an empty value", def.Name, key)
		}
	}
	for i, tc := range def.Tools {
		if tc.Name == "" {
			return fmt.Errorf("agent %q: tool at index %d has empty name", def.Name, i)
		}
		if tc.Instructions == "" {
			return fmt.Errorf("agent %q: tool %q has empty instructions", def.Name, tc.Name)
		}
	}
	return nil
}

// ValidateAgentSet checks a set of agent definitions for consistency.
func ValidateAgentSet(defs []runtime.AgentDefinition) error {
	names := make(map[string]bool, len(defs))
	for _, def := range defs {
		if err := ValidateAgentDefinition(def); err != nil {
			return err
		}
		if names[def.Name] {
			return fmt.Errorf("duplicate agent name %q", def.Name)
		}
		names[def.Name] = true
	}

	for _, def := range defs {
		if def.EscalationTarget != "" && !names[def.EscalationTarget] {
			return fmt.Errorf("agent %q: escalation_target %q does not exist", def.Name, def.EscalationTarget)
		}
	}

	return nil
}

var builtinProviderNames = map[string]bool{
	"anthropic": true, "openai": true, "google": true, "openai-compat": true,
}

// ValidateAgentProviders checks that each agent's model references a known provider.
func ValidateAgentProviders(defs []runtime.AgentDefinition, providers map[string]ProviderDef) error {
	for _, def := range defs {
		parts := strings.SplitN(def.Model, "/", 2)
		if len(parts) != 2 {
			continue // model format already validated by regex
		}
		providerName := parts[0]
		if builtinProviderNames[providerName] {
			continue
		}
		pdef, ok := providers[providerName]
		if !ok {
			return fmt.Errorf("agent %q: unknown provider %q (define it in providers: section of agents.yaml)", def.Name, providerName)
		}
		if pdef.Type == "" {
			return fmt.Errorf("agent %q: provider %q must have a type field", def.Name, providerName)
		}
		if !builtinProviderNames[pdef.Type] {
			return fmt.Errorf("agent %q: provider %q has invalid type %q", def.Name, providerName, pdef.Type)
		}
	}
	return nil
}
