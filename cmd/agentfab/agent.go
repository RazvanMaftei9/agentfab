package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/agent"
	"github.com/razvanmaftei/agentfab/internal/cluster"
	"github.com/razvanmaftei/agentfab/internal/config"
	agentgrpc "github.com/razvanmaftei/agentfab/internal/grpc"
	"github.com/razvanmaftei/agentfab/internal/llm"
	"github.com/razvanmaftei/agentfab/internal/local"
	"github.com/razvanmaftei/agentfab/internal/message"
	"github.com/razvanmaftei/agentfab/internal/runtime"
	"github.com/spf13/cobra"
)

func agentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Agent management commands",
	}
	cmd.AddCommand(agentServeCmd())
	cmd.AddCommand(agentCompileCmd())
	return cmd
}

func agentCompileCmd() *cobra.Command {
	var (
		inputDir     string
		outputDir    string
		defaultModel string
		fabricName   string
		dryRun       bool
	)

	cmd := &cobra.Command{
		Use:   "compile",
		Short: "Compile agent .md descriptions into YAML definitions and agents.yaml",
		Long: `Reads .md files from the input directory (one per agent), generates
structured YAML agent definitions and copies the .md files as special
knowledge, then writes agents.yaml.

Each .md filename becomes the agent name (e.g., backend-dev.md → agent "backend-dev").
A conductor agent is auto-added if not present in the input.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if fabricName == "" {
				cwd, _ := os.Getwd()
				fabricName = filepath.Base(cwd)
			}

			fmt.Printf("Reading agent descriptions from %s/\n", inputDir)
			descs, err := config.ReadAgentDescriptions(inputDir)
			if err != nil {
				return err
			}
			fmt.Printf("Found %d agent descriptions: ", len(descs))
			for i, d := range descs {
				if i > 0 {
					fmt.Print(", ")
				}
				fmt.Print(d.Name)
			}
			fmt.Println()

			results, err := config.CompileAgents(descs, defaultModel)
			if err != nil {
				return err
			}

			if dryRun {
				fmt.Println("\n--- Dry run: generated definitions ---")
				for _, r := range results {
					fmt.Printf("\n## %s\n", r.Definition.Name)
					data, _ := config.MarshalAgentDef(r.Definition)
					fmt.Println(string(data))
				}
				return nil
			}

			if err := config.WriteCompiledAgents(outputDir, results); err != nil {
				return err
			}
			fmt.Printf("Wrote %d agent definitions to %s/\n", len(results), outputDir)

			td := &config.FabricDef{
				Fabric: config.FabricMeta{
					Name:    fabricName,
					Version: 1,
				},
				AgentsDir: outputDir,
			}
			if err := td.ResolveAgents(); err != nil {
				return fmt.Errorf("resolve agents: %w", err)
			}

			configPath := "agents.yaml"
			if err := config.WriteFabricDef(configPath, td); err != nil {
				return err
			}

			manifest, err := config.GenerateManifest(outputDir)
			if err != nil {
				return fmt.Errorf("generate manifest: %w", err)
			}
			if err := config.WriteManifest(config.ManifestPath(outputDir), manifest); err != nil {
				return fmt.Errorf("write manifest: %w", err)
			}

			fmt.Printf("Created %s with %d agents:\n", configPath, len(td.Agents))
			for _, a := range td.Agents {
				fmt.Printf("  - %s (%s)\n", a.Name, a.Model)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&inputDir, "input", "i", "./agents", "Directory of agent .md description files")
	cmd.Flags().StringVarP(&outputDir, "output", "o", "./agents", "Output directory for generated YAML + .md files")
	cmd.Flags().StringVar(&defaultModel, "model", "anthropic/claude-sonnet-4-5-20250929", "Default model for all agents")
	cmd.Flags().StringVar(&fabricName, "name", "", "Fabric name (defaults to directory name)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print generated YAML to stdout without writing files")

	return cmd
}

func agentServeCmd() *cobra.Command {
	var (
		name          string
		configFile    string
		dataDir       string
		listen        string
		conductorAddr string
		debug         bool
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run an agent as a standalone gRPC server",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if configFile == "" {
				return fmt.Errorf("--config is required")
			}

			systemDef, err := config.LoadFabricDef(configFile)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			var agentDef runtime.AgentDefinition
			for _, a := range systemDef.Agents {
				if a.Name == name {
					agentDef = a
					break
				}
			}
			if agentDef.Name == "" {
				return fmt.Errorf("agent %q not found in config", name)
			}

			if dataDir == "" {
				dataDir = config.DefaultDataDir()
			}

			storage := local.NewStorage(dataDir, name)

			// Load TLS credentials from agent's data directory (written by conductor).
			tlsDir := agentgrpc.AgentTLSDir(dataDir, name)
			serverTLS, clientTLS, tlsErr := agentgrpc.LoadTLSCredentials(tlsDir)
			requireTLS := os.Getenv("AGENTFAB_REQUIRE_TLS") == "1"
			if tlsErr != nil {
				if requireTLS {
					return fmt.Errorf("load required TLS credentials from %s: %w", tlsDir, tlsErr)
				}
				slog.Info("no TLS credentials found, using insecure", "dir", tlsDir, "error", tlsErr)
			}

			var srv *agentgrpc.Server
			if serverTLS != nil {
				srv, err = agentgrpc.NewServer(name, listen, 64, serverTLS)
			} else {
				srv, err = agentgrpc.NewServer(name, listen, 64)
			}
			if err != nil {
				return fmt.Errorf("create gRPC server: %w", err)
			}

			discovery := agentgrpc.NewStaticDiscovery()
			ctx := context.Background()

			if conductorAddr != "" {
				discovery.Register(ctx, "conductor", runtime.Endpoint{Address: conductorAddr})
			}

			for _, a := range systemDef.Agents {
				if a.Address != "" {
					discovery.Register(ctx, a.Name, runtime.Endpoint{Address: a.Address})
				}
			}

			discovery.Register(ctx, name, runtime.Endpoint{Address: srv.Addr()})

			// Enable peers file fallback for discovering dynamically-spawned
			// agents. The conductor writes this file after all agents are up.
			discovery.SetPeersFile(agentgrpc.PeersFilePath(dataDir))

			var comm *agentgrpc.Communicator
			if clientTLS != nil {
				comm = agentgrpc.NewCommunicator(name, srv, discovery, clientTLS)
			} else {
				comm = agentgrpc.NewCommunicator(name, srv, discovery)
			}

			meter := local.NewMeter()

			specialKnowledge := ""
			if agentDef.SpecialKnowledgeFile != "" {
				skPath := fmt.Sprintf("%s/agents/%s/special_knowledge.md", dataDir, name)
				if data, readErr := os.ReadFile(skPath); readErr == nil {
					specialKnowledge = string(data)
				}
			}

			systemPrompt := agent.BuildSystemPrompt(agentDef, specialKnowledge, systemDef.Agents)

			// sendProgress sends a StatusUpdate message to the conductor with
			// streaming progress text. This makes the conductor's UI show what
			// the agent is thinking/doing, same as in local mode.
			sendProgress := func(text string) {
				progressMsg := &message.Message{
					From: name,
					To:   "conductor",
					Type: message.TypeStatusUpdate,
					Parts: []message.Part{
						message.TextPart{Text: text},
					},
				}
				// Best-effort: don't block if conductor is temporarily unreachable.
				_ = comm.Send(context.Background(), progressMsg)
			}

			var debugLog *llm.DebugStore
			if debug {
				debugDir := filepath.Join(dataDir, "debug")
				debugLog, err = llm.NewDebugStore(debugDir)
				if err != nil {
					return fmt.Errorf("create debug store: %w", err)
				}
				defer debugLog.Close()
			}

			// Build tool infos for model binding (must match the live tools
			// given to ToolExecutor so the model generates structured calls).
			toolInfos := agent.BuildToolInfos(agentDef.Tools)

			generateFn := func(callCtx context.Context, input []*schema.Message) (*schema.Message, error) {
				m, err := llm.NewChatModel(callCtx, agentDef.Model, nil, systemDef.Providers)
				if err != nil {
					return nil, err
				}

				var baseModel model.BaseChatModel = m
				if len(toolInfos) > 0 {
					if tcm, ok := m.(model.ToolCallingChatModel); ok {
						bound, bindErr := tcm.WithTools(toolInfos)
						if bindErr != nil {
							return nil, fmt.Errorf("bind tools for %q: %w", name, bindErr)
						}
						baseModel = bound
					} else {
						slog.Warn("model does not support ToolCallingChatModel; tools will not be bound",
							"agent", name, "model", agentDef.Model)
					}
				}

				metered := &llm.MeteredModel{
					Model:     baseModel,
					AgentName: name,
					ModelID:   agentDef.Model,
					Meter:     meter,
					DebugLog:  debugLog,
					Options:   llm.ProviderOptions(agentDef.Model, systemDef.Providers),
					OnChunk: func(textSoFar string) {
						snippet := textSoFar
						if len(snippet) > 80 {
							snippet = snippet[len(snippet)-80:]
						}
						sendProgress(snippet)
					},
				}
				return metered.Generate(callCtx, input)
			}

			var toolExec *agent.ToolExecutor
			liveTools := agent.LiveTools(agentDef.Tools)
			if len(liveTools) > 0 {
				toolExec = &agent.ToolExecutor{
					Tools: liveTools,
					TierPaths: []string{
						storage.TierDir(runtime.TierScratch),
						storage.TierDir(runtime.TierAgent),
						storage.TierDir(runtime.TierShared),
					},
					AgentName:       name,
					MaxOutputTokens: llm.MaxOutputTokens(agentDef.Model),
					ContextLimit:    llm.ContextLimit(agentDef.Model),
				}
			}

			ag := &agent.Agent{
				Def:                agentDef,
				Comm:               comm,
				Storage:            storage,
				Meter:              meter,
				Logger:             message.NewLogger(local.NewSharedAppender(storage)),
				Generate:           generateFn,
				SystemPrompt:       systemPrompt,
				ToolExecutor:       toolExec,
				PromptCacheEnabled: llm.HasPromptCaching(agentDef.Model, systemDef.Providers),
				OnProgress:         sendProgress,
			}

			if agentDef.Budget != nil {
				meter.SetBudget(ctx, name, *agentDef.Budget)
			}

			go func() {
				if err := srv.Serve(); err != nil {
					slog.Error("gRPC server error", "error", err)
				}
			}()

			slog.Info("agent serving", "name", name, "listen", srv.Addr())

			monitorCtx, monitorCancel := context.WithCancel(ctx)
			defer monitorCancel()

			binary, _ := os.Executable()
			clusterMon := &cluster.Monitor{
				Self: cluster.MemberInfo{
					Name:    name,
					Role:    "agent",
					Address: srv.Addr(),
					PID:     os.Getpid(),
				},
				StatePath: cluster.StatePath(dataDir),
				OnMemberDead: cluster.ConductorDeadCallback(
					cluster.StatePath(dataDir),
					cluster.MemberInfo{Name: name, Role: "agent"},
					binary,
					[]string{"run", "--distributed", "--config", configFile, "--data", dataDir},
				),
			}
			go clusterMon.Run(monitorCtx)

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

			agentCtx, agentCancel := context.WithCancel(ctx)
			defer agentCancel()

			errCh := make(chan error, 1)
			go func() {
				errCh <- ag.Run(agentCtx)
			}()

			select {
			case sig := <-sigCh:
				slog.Info("received signal, shutting down", "signal", sig)
				monitorCancel()
				agentCancel()
				srv.Stop()
				comm.Close()
			case err := <-errCh:
				monitorCancel()
				srv.Stop()
				comm.Close()
				if err != nil {
					return err
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Agent name (must match a name in agents.yaml)")
	cmd.Flags().StringVar(&configFile, "config", "", "Path to agents.yaml")
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "Data directory (default: system default)")
	cmd.Flags().StringVar(&listen, "listen", ":50051", "gRPC listen address")
	cmd.Flags().StringVar(&conductorAddr, "conductor", "", "Conductor gRPC address (e.g., localhost:50050)")
	cmd.Flags().BoolVar(&debug, "debug", false, "Log full LLM requests/responses as JSONL")

	return cmd
}
