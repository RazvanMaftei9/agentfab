package conductor

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/defaults"
	"github.com/razvanmaftei/agentfab/internal/agent"
	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/razvanmaftei/agentfab/internal/controlplane"
	"github.com/razvanmaftei/agentfab/internal/event"
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

	configFilePath := filepath.Join(c.StorageLayout.SharedRoot, "agents.yaml")
	if err := os.MkdirAll(filepath.Dir(configFilePath), 0755); err != nil {
		return fmt.Errorf("create shared dir: %w", err)
	}
	if err := config.WriteFabricDef(configFilePath, c.FabricDef); err != nil {
		return fmt.Errorf("write agents.yaml: %w", err)
	}

	for _, def := range c.FabricDef.Agents {
		if def.SpecialKnowledgeFile != "" {
			if err := writeSpecialKnowledge(ctx, c.StorageFactory(def.Name), c.FabricDef.AgentsDir, def); err != nil {
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
		if c.ExternalAgents {
			if err := waitForExternalAgentInstance(ctx, c, def.Name, 30*time.Second); err != nil {
				return fmt.Errorf("wait for external agent %q: %w", def.Name, err)
			}
			c.Events.Emit(event.Event{
				Type:       event.AgentReady,
				AgentName:  def.Name,
				AgentModel: def.Model,
			})
			continue
		}
		if err := spawnLocalAgent(ctx, c, def, c.FabricDef.Agents); err != nil {
			return fmt.Errorf("spawn agent %q: %w", def.Name, err)
		}
		c.Events.Emit(event.Event{
			Type:       event.AgentReady,
			AgentName:  def.Name,
			AgentModel: def.Model,
		})
	}

	c.Events.Emit(event.Event{Type: event.AllAgentsReady})
	slog.Info("fabric setup complete", "agents", len(c.FabricDef.Agents))
	return nil
}

func waitForExternalAgentInstance(ctx context.Context, c *Conductor, profile string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		instances, err := c.ControlPlane.ListInstances(ctx, controlplane.InstanceFilter{Profile: profile})
		if err != nil {
			return err
		}
		for _, instance := range instances {
			switch instance.State {
			case controlplane.InstanceStateReady, controlplane.InstanceStateBusy:
				return nil
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for ready instance")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func spawnLocalAgent(ctx context.Context, c *Conductor, def runtime.AgentDefinition, peers []runtime.AgentDefinition) error {
	c.Discovery.Register(ctx, def.Name, runtime.Endpoint{Address: "local", Local: true})
	comm := c.CommFactory.Register(def.Name)
	storage := c.StorageFactory(def.Name)
	workspace, err := runtime.OpenWorkspace(ctx, storage)
	if err != nil {
		return fmt.Errorf("materialize workspace for %q: %w", def.Name, err)
	}

	specialKnowledge := ""
	if def.SpecialKnowledgeFile != "" {
		if data, err := storage.Read(ctx, runtime.TierAgent, "special_knowledge.md"); err == nil {
			specialKnowledge = string(data)
		}
	}

	systemPrompt := agent.BuildSystemPrompt(def, specialKnowledge, peers)
	generateFn := c.createGenerateFn(ctx, def)

	if len(def.Tools) > 0 {
		if err := writeToolsYAML(ctx, storage, def); err != nil {
			_ = workspace.Close()
			return err
		}
		if err := workspace.Agent.Refresh(ctx); err != nil {
			_ = workspace.Close()
			return err
		}
	}

	var toolExec *agent.ToolExecutor
	liveTools := agent.LiveTools(def.Tools)
	if len(liveTools) > 0 {
		toolExec = &agent.ToolExecutor{
			Tools:           liveTools,
			TierPaths:       workspace.TierPaths(),
			AgentName:       def.Name,
			MaxOutputTokens: llm.MaxOutputTokens(def.Model),
			ContextLimit:    llm.ContextLimit(def.Model),
			SyncWorkspace:   workspace.Sync,
		}
	}

	ag := &agent.Agent{
		Def:                def,
		Comm:               comm,
		Storage:            storage,
		Workspace:          workspace,
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

func writeToolsYAML(ctx context.Context, storage runtime.Storage, def runtime.AgentDefinition) error {
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
	if err := storage.Write(ctx, runtime.TierAgent, "tools.yaml", data); err != nil {
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
				DebugLog: c.DebugLog,
				Options:  llm.ProviderOptions(def.Model, c.FabricDef.Providers),
			}
			return metered.Generate(callCtx, input)
		}
	}
	return func(_ context.Context, _ []*schema.Message) (*schema.Message, error) {
		return nil, fmt.Errorf("no model factory configured for agent %q", def.Name)
	}
}

func writeSpecialKnowledge(ctx context.Context, storage runtime.Storage, agentsDir string, def runtime.AgentDefinition) error {
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

	return storage.Write(ctx, runtime.TierAgent, "special_knowledge.md", data)
}
