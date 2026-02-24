package conductor

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/defaults"
	"github.com/razvanmaftei/agentfab/internal/agent"
	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/razvanmaftei/agentfab/internal/event"
	agentgrpc "github.com/razvanmaftei/agentfab/internal/grpc"
	"github.com/razvanmaftei/agentfab/internal/llm"
	"github.com/razvanmaftei/agentfab/internal/local"
	"github.com/razvanmaftei/agentfab/internal/message"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

// Setup validates definitions, writes agents.yaml, spawns agents.
func Setup(ctx context.Context, c *Conductor) error {
	if err := config.ValidateAgentSet(c.FabricDef.Agents); err != nil {
		return fmt.Errorf("validate agents: %w", err)
	}

	configFilePath := filepath.Join(c.BaseDir, "shared", "agents.yaml")
	if err := os.MkdirAll(filepath.Dir(configFilePath), 0755); err != nil {
		return fmt.Errorf("create shared dir: %w", err)
	}
	if err := config.WriteFabricDef(configFilePath, c.FabricDef); err != nil {
		return fmt.Errorf("write agents.yaml: %w", err)
	}

	for _, def := range c.FabricDef.Agents {
		if def.SpecialKnowledgeFile != "" {
			if err := writeSpecialKnowledge(c.BaseDir, c.FabricDef.AgentsDir, def); err != nil {
				slog.Warn("could not write special knowledge", "agent", def.Name, "error", err)
			}
		}
	}

	for _, def := range c.FabricDef.Agents {
		if def.Name == "conductor" {
			continue
		}
		c.Events.Emit(event.Event{
			Type:       event.AgentStarting,
			AgentName:  def.Name,
			AgentModel: def.Model,
		})
		if err := spawnAgent(ctx, c, def, c.FabricDef.Agents); err != nil {
			return fmt.Errorf("spawn agent %q: %w", def.Name, err)
		}
		c.Events.Emit(event.Event{
			Type:       event.AgentReady,
			AgentName:  def.Name,
			AgentModel: def.Model,
		})
	}

	// In distributed mode, write peer addresses so agents can discover each
	// other for direct communication (e.g., loop task forwarding).
	if c.Distributed {
		if pl, ok := c.Lifecycle.(*agentgrpc.ProcessLifecycle); ok {
			if err := pl.WritePeers(); err != nil {
				slog.Warn("failed to write peers file", "error", err)
			}
		}
	}

	c.Events.Emit(event.Event{Type: event.AllAgentsReady})
	slog.Info("fabric setup complete", "agents", len(c.FabricDef.Agents))
	return nil
}

func spawnAgent(ctx context.Context, c *Conductor, def runtime.AgentDefinition, peers []runtime.AgentDefinition) error {
	// In distributed mode, the agent runs as a separate OS process.
	// The ProcessLifecycle handles spawning, heartbeat readiness, and
	// discovery registration. We only need to write config files to disk.
	if c.Distributed {
		return spawnDistributedAgent(ctx, c, def)
	}

	return spawnLocalAgent(ctx, c, def, peers)
}

func spawnDistributedAgent(ctx context.Context, c *Conductor, def runtime.AgentDefinition) error {
	if len(def.Tools) > 0 {
		if err := writeToolsYAML(c.BaseDir, def); err != nil {
			return err
		}
	}

	if def.Budget != nil {
		c.Meter.SetBudget(ctx, def.Name, *def.Budget)
	}

	return c.Lifecycle.Spawn(ctx, def, nil)
}

func spawnLocalAgent(ctx context.Context, c *Conductor, def runtime.AgentDefinition, peers []runtime.AgentDefinition) error {
	c.Discovery.Register(ctx, def.Name, runtime.Endpoint{Address: "local", Local: true})
	comm := c.CommFactory.Register(def.Name)
	storage := c.StorageFactory(def.Name)

	specialKnowledge := ""
	if def.SpecialKnowledgeFile != "" {
		skPath := filepath.Join(c.BaseDir, "agents", def.Name, "special_knowledge.md")
		if data, err := os.ReadFile(skPath); err == nil {
			specialKnowledge = string(data)
		}
	}

	systemPrompt := agent.BuildSystemPrompt(def, specialKnowledge, peers)
	generateFn := c.createGenerateFn(ctx, def)

	if len(def.Tools) > 0 {
		if err := writeToolsYAML(c.BaseDir, def); err != nil {
			return err
		}
	}

	var toolExec *agent.ToolExecutor
	liveTools := agent.LiveTools(def.Tools)
	if len(liveTools) > 0 {
		toolExec = &agent.ToolExecutor{
			Tools: liveTools,
			TierPaths: []string{
				storage.TierDir(runtime.TierScratch),
				storage.TierDir(runtime.TierAgent),
				storage.TierDir(runtime.TierShared),
			},
			AgentName:       def.Name,
			MaxOutputTokens: llm.MaxOutputTokens(def.Model),
			ContextLimit:    llm.ContextLimit(def.Model),
		}
	}

	ag := &agent.Agent{
		Def:                def,
		Comm:               comm,
		Storage:            storage,
		Meter:              c.Meter,
		Logger:             message.NewLogger(local.NewSharedAppender(c.StorageFactory(def.Name))),
		Generate:           generateFn,
		SystemPrompt:       systemPrompt,
		Events:             c.Events,
		ToolExecutor:       toolExec,
		PromptCacheEnabled: llm.HasPromptCaching(def.Model, c.FabricDef.Providers),
	}

	if def.Budget != nil {
		c.Meter.SetBudget(ctx, def.Name, *def.Budget)
	}

	return c.Lifecycle.Spawn(ctx, def, func(agentCtx context.Context) error {
		return ag.Run(agentCtx)
	})
}

func writeToolsYAML(baseDir string, def runtime.AgentDefinition) error {
	agentDir := filepath.Join(baseDir, "agents", def.Name)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return fmt.Errorf("create agent dir for tools.yaml: %w", err)
	}
	type toolEntry struct {
		Name         string `yaml:"name"`
		Instructions string `yaml:"instructions"`
	}
	entries := make([]toolEntry, len(def.Tools))
	for i, tc := range def.Tools {
		entries[i] = toolEntry{Name: tc.Name, Instructions: tc.Instructions}
	}
	data, err := yaml.Marshal(entries)
	if err != nil {
		return fmt.Errorf("marshal tools.yaml for %q: %w", def.Name, err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "tools.yaml"), data, 0644); err != nil {
		return fmt.Errorf("write tools.yaml for %q: %w", def.Name, err)
	}
	return nil
}

func (c *Conductor) createGenerateFn(ctx context.Context, def runtime.AgentDefinition) func(context.Context, []*schema.Message) (*schema.Message, error) {
	if c.ModelFactory != nil {
		var onChunk llm.ChunkCallback
		if c.Events != nil {
			onChunk = func(textSoFar string) {
				snippet := textSoFar
				if len(snippet) > 80 {
					snippet = snippet[len(snippet)-80:]
				}
				c.Events.Emit(event.Event{
					Type:         event.TaskProgress,
					TaskAgent:    def.Name,
					ProgressText: snippet,
				})
			}
		}

		toolInfos := agent.BuildToolInfos(def.Tools)

		return func(callCtx context.Context, input []*schema.Message) (*schema.Message, error) {
			routeCtx := runtime.WithAgentName(callCtx, def.Name)
			m, err := c.ModelFactory(routeCtx, def.Model)
			if err != nil {
				return nil, fmt.Errorf("create model for %q: %w", def.Name, err)
			}

			var baseModel model.BaseChatModel = m
			if len(toolInfos) > 0 {
				if tcm, ok := m.(model.ToolCallingChatModel); ok {
					bound, bindErr := tcm.WithTools(toolInfos)
					if bindErr != nil {
						return nil, fmt.Errorf("bind tools for %q: %w", def.Name, bindErr)
					}
					baseModel = bound
				} else {
					slog.Warn("model does not support ToolCallingChatModel; tools will not be bound",
						"agent", def.Name, "model", def.Model)
				}
			}

			metered := &llm.MeteredModel{
				Model:     baseModel,
				AgentName: def.Name,
				ModelID:   def.Model,
				Meter:     c.Meter,
				OnChunk:   onChunk,
				OnRetry: func(attempt, maxAttempts int, err error) {
					if c.Events != nil {
						c.Events.Emit(event.Event{
							Type:         event.TaskProgress,
							TaskAgent:    def.Name,
							ProgressText: fmt.Sprintf("Retrying model call (%d/%d)...", attempt, maxAttempts),
						})
					}
				},
				DebugLog:  c.DebugLog,
				Options:   llm.ProviderOptions(def.Model, c.FabricDef.Providers),
			}
			return metered.Generate(callCtx, input)
		}
	}
	return func(_ context.Context, _ []*schema.Message) (*schema.Message, error) {
		return nil, fmt.Errorf("no model factory configured for agent %q", def.Name)
	}
}

func writeSpecialKnowledge(baseDir string, agentsDir string, def runtime.AgentDefinition) error {
	var data []byte

	// 1. Try agents dir on disk.
	if agentsDir != "" {
		p := filepath.Join(agentsDir, def.SpecialKnowledgeFile)
		if d, err := os.ReadFile(p); err == nil {
			data = d
		}
	}

	// 2. Fall back to embedded defaults.
	if data == nil {
		if d, err := fs.ReadFile(defaults.AgentFS, "agents/"+def.SpecialKnowledgeFile); err == nil {
			data = d
		}
	}

	if data == nil {
		return nil // Not found anywhere — graceful.
	}

	agentDir := filepath.Join(baseDir, "agents", def.Name)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(agentDir, "special_knowledge.md"), data, 0644)
}
