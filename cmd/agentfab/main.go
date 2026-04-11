package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/razvanmaftei/agentfab/defaults"
	"github.com/razvanmaftei/agentfab/internal/conductor"
	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/razvanmaftei/agentfab/internal/event"
	"github.com/razvanmaftei/agentfab/internal/llm"
	"github.com/razvanmaftei/agentfab/internal/metrics"
	"github.com/razvanmaftei/agentfab/internal/ui"
	"github.com/razvanmaftei/agentfab/internal/version"
	"github.com/spf13/cobra"
)

const cliShutdownTimeout = 15 * time.Second

var exitProcess = os.Exit

func main() {
	root := newRootCommand()

	if args := defaultRootArgs(os.Args[1:]); len(args) > 0 {
		root.SetArgs(args)
	}

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:     "agentfab",
		Short:   "AgentFab -- distributed AI agent orchestration",
		Version: version.Version,
	}

	root.AddCommand(initCmd())
	root.AddCommand(runCmd())
	root.AddCommand(verifyCmd())
	root.AddCommand(statusCmd())
	root.AddCommand(metricsCmd())
	root.AddCommand(benchCmd())
	root.AddCommand(agentCmd())
	root.AddCommand(nodeCmd())
	root.AddCommand(controlPlaneCmd())
	root.AddCommand(setupCmd())

	return root
}

func defaultRootArgs(args []string) []string {
	if len(args) == 0 {
		return []string{"run"}
	}
	return nil
}

func initCmd() *cobra.Command {
	var (
		name         string
		agentsDir    string
		custom       bool
		descriptDir  string
		defaultModel string
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a new fabric with default or custom agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			w := os.Stdout
			tty := ui.IsTTY(os.Stdin)

			if name == "" {
				name = filepath.Base(mustCwd())
			}

			path := "agents.yaml"
			if _, err := os.Stat(path); err == nil {
				fmt.Fprintf(w, "  agents.yaml already exists in this directory.\n")
				return nil
			}

			var ti *ui.TermInput
			if tty {
				ti = ui.NewTermInput()
				defer ti.Close()
			}

			useCustom := custom
			if !useCustom && !cmd.Flags().Changed("custom") {
				useCustom = initPickAgentMode(w, ti, tty)
			}

			if useCustom {
				return initCustom(w, ti, tty, name, descriptDir, defaultModel)
			}

			return initDefault(w, name, agentsDir, path)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Fabric name (defaults to directory name)")
	cmd.Flags().StringVar(&agentsDir, "agents-dir", "defaults/agents", "Directory for agent definition files")
	cmd.Flags().BoolVar(&custom, "custom", false, "Use custom agents from .md descriptions")
	cmd.Flags().StringVar(&descriptDir, "descriptions", "./agents", "Directory containing agent .md description files")
	cmd.Flags().StringVar(&defaultModel, "model", "anthropic/claude-sonnet-4-5-20250929", "Default model for compiled agents")
	return cmd
}

func initPickAgentMode(w *os.File, ti *ui.TermInput, tty bool) bool {
	fmt.Fprintf(w, "\n  %s%sAgent Setup%s\n\n", ui.Bold, ui.Teal, ui.Reset)

	defaultAgents := config.DefaultProfiles()

	fmt.Fprintf(w, "  %sDefault agents:%s\n", ui.Dim, ui.Reset)
	for _, a := range defaultAgents {
		if a.Name == "conductor" {
			continue
		}
		fmt.Fprintf(w, "    %s%s%s  %s%s%s\n",
			ui.Bold, a.Name, ui.Reset,
			ui.Dim, a.Purpose, ui.Reset)
	}
	fmt.Fprintf(w, "    %s%s%s  %s%s%s\n",
		ui.Bold, "conductor", ui.Reset,
		ui.Dim, "Request decomposition, orchestration, and user I/O (always present)", ui.Reset)
	fmt.Fprintln(w)

	options := []string{
		"Use default agents",
		"Use custom agents (provide .md descriptions)",
	}

	if tty && ti != nil {
		choice := initArrowSelect(w, ti, options)
		return choice == 1
	}

	for i, opt := range options {
		fmt.Fprintf(w, "  [%d] %s\n", i+1, opt)
	}
	fmt.Fprint(w, "  Choice [1]: ")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line) == "2"
}

func initDefault(w *os.File, name, agentsDir, path string) error {
	if err := config.ExtractDefaultAgents(defaults.AgentFS, agentsDir); err != nil {
		return fmt.Errorf("extract default agents: %w", err)
	}

	td := &config.FabricDef{
		Fabric: config.FabricMeta{
			Name:    name,
			Version: 1,
		},
		AgentsDir: agentsDir,
		Defaults:  config.FabricDefaults{},
	}
	if err := td.ResolveAgents(); err != nil {
		return fmt.Errorf("resolve agents: %w", err)
	}

	if err := config.WriteFabricDef(path, td); err != nil {
		return err
	}

	manifest, err := config.GenerateManifest(agentsDir)
	if err != nil {
		return fmt.Errorf("generate manifest: %w", err)
	}
	manifestPath := config.ManifestPath(agentsDir)
	if err := config.WriteManifest(manifestPath, manifest); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	bundle, err := config.GenerateSignedBundle(td)
	if err != nil {
		return fmt.Errorf("generate bundle: %w", err)
	}
	if err := config.WriteSignedBundle(config.BundlePath(agentsDir), bundle); err != nil {
		return fmt.Errorf("write bundle: %w", err)
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s%s Created %s%s with %d agents\n", ui.Green, checkMark, path, ui.Reset, len(td.Agents))
	for _, a := range td.Agents {
		fmt.Fprintf(w, "    %s%s%s  %s%s%s\n",
			ui.Bold, a.Name, ui.Reset,
			ui.Dim, a.Model, ui.Reset)
	}
	fmt.Fprintf(w, "\n  Agent definitions: %s%s/%s\n", ui.Dim, agentsDir, ui.Reset)
	fmt.Fprintf(w, "  Data directory:    %s%s%s\n\n", ui.Dim, config.DefaultDataDir(), ui.Reset)

	return nil
}

func initCustom(w *os.File, ti *ui.TermInput, tty bool, name, descriptDir, defaultModel string) error {
	if _, err := os.Stat(descriptDir); os.IsNotExist(err) {
		fmt.Fprintf(w, "\n  Directory %s%s%s not found.\n\n", ui.Bold, descriptDir, ui.Reset)

		prompt := fmt.Sprintf("  %sPath to agent .md descriptions%s: ", ui.Bold, ui.Reset)
		if ti != nil {
			line, ok := ti.ReadLine(w, prompt)
			if !ok {
				return fmt.Errorf("interrupted")
			}
			line = strings.TrimSpace(line)
			if line != "" {
				descriptDir = line
			}
		} else {
			fmt.Fprint(w, prompt)
			reader := bufio.NewReader(os.Stdin)
			line, _ := reader.ReadString('\n')
			line = strings.TrimSpace(line)
			if line != "" {
				descriptDir = line
			}
		}
	}

	fmt.Fprintf(w, "\n  Compiling agents from %s%s/%s\n", ui.Dim, descriptDir, ui.Reset)

	configPath, err := config.InitProjectCustom(name, ".", descriptDir, defaultModel)
	if err != nil {
		return err
	}

	td, err := config.LoadFabricDef(configPath)
	if err != nil {
		return fmt.Errorf("load generated config: %w", err)
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s%s Created %s%s with %d agents\n", ui.Green, checkMark, configPath, ui.Reset, len(td.Agents))
	for _, a := range td.Agents {
		fmt.Fprintf(w, "    %s%s%s  %s%s%s\n",
			ui.Bold, a.Name, ui.Reset,
			ui.Dim, a.Model, ui.Reset)
	}
	fmt.Fprintln(w)

	return nil
}

func initArrowSelect(w *os.File, ti *ui.TermInput, options []string) int {
	selected := 0

	if err := ti.EnterRaw(); err != nil {
		return 0
	}
	keyCh := ti.StartKeyEvents()

	lines := drawInitArrowSelect(w, options, selected)

	for {
		key, ok := <-keyCh
		if !ok {
			break
		}
		switch {
		case key.Key == "up":
			if selected > 0 {
				selected--
				initEraseLines(w, lines)
				lines = drawInitArrowSelect(w, options, selected)
			}
		case key.Key == "down":
			if selected < len(options)-1 {
				selected++
				initEraseLines(w, lines)
				lines = drawInitArrowSelect(w, options, selected)
			}
		case key.Key == "enter":
			ti.StopKeyEvents()
			ti.Drain()
			ti.ExitRaw()
			initEraseLines(w, lines)
			fmt.Fprintf(w, "  %s%s %s%s\n", ui.Cyan, ui.ConnArrow, options[selected], ui.Reset)
			return selected
		case key.Rune >= '1' && key.Rune <= '9':
			idx := int(key.Rune - '1')
			if idx < len(options) {
				ti.StopKeyEvents()
				ti.Drain()
				ti.ExitRaw()
				initEraseLines(w, lines)
				fmt.Fprintf(w, "  %s%s %s%s\n", ui.Cyan, ui.ConnArrow, options[idx], ui.Reset)
				return idx
			}
		}
	}

	ti.ExitRaw()
	return selected
}

func drawInitArrowSelect(w *os.File, options []string, selected int) int {
	lines := 0
	for i, opt := range options {
		if i == selected {
			fmt.Fprintf(w, "  %s▸ %s%s%s\n", ui.Cyan, ui.Bold, opt, ui.Reset)
		} else {
			fmt.Fprintf(w, "    %s%s%s\n", ui.Dim, opt, ui.Reset)
		}
		lines++
	}
	fmt.Fprintf(w, "  %s↑↓%s%s navigate  %sEnter%s%s select%s\n",
		ui.Bold, ui.Reset, ui.Dim, ui.Bold, ui.Reset, ui.Dim, ui.Reset)
	lines++
	return lines
}

func initEraseLines(w *os.File, n int) {
	if n > 0 {
		fmt.Fprint(w, strings.Repeat(ui.MoveUp+ui.ClearLn, n))
	}
}

const checkMark = "✓"

func runCmd() *cobra.Command {
	var (
		configFile          string
		agentsDir           string
		dataDir             string
		debug               bool
		skipVerify          bool
		listenAddr          string
		advertiseAddr       string
		controlPlaneAddress string
		externalNodes       bool
		bootstrapNodes      int
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Start the fabric and enter interactive mode",
		RunE: func(cmd *cobra.Command, args []string) error {
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
				Level: slog.LevelWarn,
			})))

			tty := ui.IsTTY(os.Stdout)
			projectDir := ""

			// A single TermInput avoids leaking readLoopTTY goroutines on
			// macOS where closing /dev/tty doesn't unblock a concurrent read().
			var termInput *ui.TermInput
			if tty {
				termInput = ui.NewTermInput()
				defer termInput.Close()
			}

			var stdinReader *bufio.Reader
			if !tty {
				stdinReader = bufio.NewReader(os.Stdin)
			}
			pickerReadLine := func() (string, bool) {
				if termInput != nil {
					return termInput.ReadLine(os.Stdout, "")
				}
				line, err := stdinReader.ReadString('\n')
				if err != nil {
					return "", false
				}
				return strings.TrimRight(line, "\n\r"), true
			}

			if !cmd.Flags().Changed("config") {
				entries, err := config.LoadProjectRegistry()
				if err != nil {
					return fmt.Errorf("load project registry: %w", err)
				}

				for {
					projects := make([]ui.ProjectInfo, len(entries))
					for i, e := range entries {
						projects[i] = ui.ProjectInfo{
							Name:       e.Name,
							Dir:        e.Dir,
							LastUsedAt: e.LastUsedAt,
						}
						if empty, err := projectWorkspaceEmpty(e); err == nil && empty {
							projects[i].Error = "empty workspace"
							continue
						}
						if _, err := projectConfigPath(e); err != nil {
							projects[i].Error = shortProjectError(err)
						}
					}

					selection := ui.PickProject(os.Stdout, projects, pickerReadLine, tty, termInput)

					switch selection {
					case "":
						return nil // cancelled
					case "new":
						name, dir, ok := promptNewProject(os.Stdout, pickerReadLine, tty)
						if !ok {
							return nil
						}
						// If the directory already has an agents.yaml, just
						// register it without overwriting.
						existingCfg := filepath.Join(dir, "agents.yaml")
						if _, statErr := os.Stat(existingCfg); statErr == nil {
							entries, err = config.AddProject(name, dir)
							if err != nil {
								return fmt.Errorf("register project: %w", err)
							}
							configFile = existingCfg
							projectDir = dir
							if !cmd.Flags().Changed("data-dir") {
								dataDir = dir
							}
							fmt.Printf("Registered existing project %q at %s\n", name, dir)
						} else {
							cfgPath, err := config.InitProject(name, dir)
							if err != nil {
								return fmt.Errorf("create project: %w", err)
							}
							entries, err = config.AddProject(name, dir)
							if err != nil {
								return fmt.Errorf("register project: %w", err)
							}
							configFile = cfgPath
							projectDir = dir
							if !cmd.Flags().Changed("data-dir") {
								dataDir = dir
							}
							fmt.Printf("Created project %q at %s\n", name, dir)
						}
					default:
						found, err := findProjectEntry(entries, selection)
						if err != nil {
							return err
						}
						projectConfig, err := projectConfigPath(*found)
						if err != nil {
							if empty, emptyErr := projectWorkspaceEmpty(*found); emptyErr == nil && empty {
								recreate, promptErr := promptRecreateProject(os.Stdout, pickerReadLine, tty, *found)
								if promptErr != nil {
									return promptErr
								}
								if recreate {
									cfgPath, initErr := config.InitProject(found.Name, found.Dir)
									if initErr != nil {
										return fmt.Errorf("recreate project %q: %w", found.Name, initErr)
									}
									fmt.Printf("Recreated project %q at %s\n", found.Name, found.Dir)
									configFile = cfgPath
									projectDir = found.Dir
									if !cmd.Flags().Changed("data-dir") {
										dataDir = found.Dir
									}
									if entries, err = config.TouchProject(entries, selection); err != nil {
										fmt.Fprintf(os.Stderr, "Warning: could not update project timestamp: %v\n", err)
									}
									break
								}
							}
							fmt.Fprintf(os.Stderr, "Project %q is not runnable: %v\n", selection, err)
							continue
						}
						configFile = projectConfig
						projectDir = found.Dir
						if !cmd.Flags().Changed("data-dir") {
							dataDir = found.Dir
						}
						if entries, err = config.TouchProject(entries, selection); err != nil {
							fmt.Fprintf(os.Stderr, "Warning: could not update project timestamp: %v\n", err)
						}
					}
					break
				}
			}

			td, err := config.LoadFabricDef(configFile)
			if err != nil {
				return fmt.Errorf("load fabric definition: %w\nRun 'agentfab init' first", err)
			}

			if cmd.Flags().Changed("agents-dir") {
				td.AgentsDir = agentsDir
				td.Agents = nil
				if err := td.ResolveAgents(); err != nil {
					return fmt.Errorf("resolve agents from %q: %w", agentsDir, err)
				}
			} else if projectDir != "" {
				if err := config.ResolvePathsRelativeToConfig(td, configFile); err != nil {
					return err
				}
			}

			var verifiedBundle config.BundleVerificationResult
			if !skipVerify {
				verifiedBundle, err = config.VerifySignedBundle(td)
				if err != nil {
					if tty {
						fmt.Fprintf(os.Stderr, "Warning: agent bundle verification failed: %v\n", err)
						fmt.Fprint(os.Stderr, "Continue anyway? [y/N] ")
						answer, ok := pickerReadLine()
						if !ok {
							return fmt.Errorf("aborted: agent files have been modified")
						}
						answer = strings.TrimSpace(strings.ToLower(answer))
						if answer != "y" && answer != "yes" {
							return fmt.Errorf("aborted: agent files have been modified")
						}
					} else {
						return fmt.Errorf("agent bundle verification failed: %w (use --skip-verify to bypass)", err)
					}
				}
			}

			baseDir := dataDir
			fmt.Printf("Data directory: %s\n", baseDir)
			if err := os.MkdirAll(baseDir, 0755); err != nil {
				return err
			}

			startupBus := event.NewBus()
			renderer := ui.NewRenderer(os.Stdout, tty)

			factory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
				return llm.NewChatModel(ctx, modelID, nil, td.Providers)
			}

			bootstrapNodeCount, err := resolveBootstrapNodeCount(td, externalNodes, controlPlaneAddress, bootstrapNodes, cmd.Flags().Changed("bootstrap-nodes"))
			if err != nil {
				return err
			}
			listenAddr = localBootstrapListenAddr(bootstrapNodeCount > 0, listenAddr, cmd.Flags().Changed("listen"))

			var cluster *localCluster
			if bootstrapNodeCount > 0 {
				cluster, err = startLocalCluster(context.Background(), td, baseDir, verifiedBundle, bootstrapNodeCount, debug, factory)
				if err != nil {
					return fmt.Errorf("start local distributed cluster: %w", err)
				}
				controlPlaneAddress = cluster.controlPlaneAddress
				fmt.Printf("Control plane: %s\n", controlPlaneAddress)
				fmt.Printf("Bootstrap nodes: %d\n", len(bootstrapNodeSummary(cluster)))
			}

			startupSummary := ui.StartupSummary{
				RuntimeMode:         runtimeModeLabel(externalNodes),
				ControlPlaneAddress: startupControlPlaneAddress(externalNodes, controlPlaneAddress),
			}
			if cluster != nil {
				startupSummary.BootstrapNodeIDs = bootstrapNodeSummary(cluster)
			}

			var conductorOpts []conductor.Option
			if externalNodes {
				conductorOpts = append(conductorOpts, conductor.WithExternalAgents())
				if listenAddr != "" {
					conductorOpts = append(conductorOpts, conductor.WithConductorListenAddr(listenAddr))
				}
				if advertiseAddr != "" {
					conductorOpts = append(conductorOpts, conductor.WithConductorAdvertiseAddr(advertiseAddr))
				}
				if controlPlaneAddress != "" {
					conductorOpts = append(conductorOpts, conductor.WithControlPlaneAddress(controlPlaneAddress))
				}
			}
			if verifiedBundle.BundleDigest != "" {
				conductorOpts = append(conductorOpts, conductor.WithBundleDigests(verifiedBundle.BundleDigest, verifiedBundle.ProfileDigests))
			}

			var debugStore *llm.DebugStore
			if debug {
				debugDir := filepath.Join(baseDir, "debug")
				var debugErr error
				debugStore, debugErr = llm.NewDebugStore(debugDir)
				if debugErr != nil {
					return fmt.Errorf("create debug store: %w", debugErr)
				}
				defer debugStore.Close()
				conductorOpts = append(conductorOpts, conductor.WithDebugLog(debugStore))
				fmt.Printf("Debug logs: %s/<agent>/{input,output}.jsonl\n", debugDir)
			}

			c, err := conductor.New(td, baseDir, factory, startupBus, conductorOpts...)
			if err != nil {
				if cluster != nil {
					_ = cluster.Shutdown(context.Background())
				}
				fmt.Fprintf(os.Stderr, "\nError: %v\n", err)
				return fmt.Errorf("create conductor: %w", err)
			}

			if templates, err := conductor.LoadTemplates(defaults.TemplateFS); err == nil && len(templates) > 0 {
				c.Templates = templates
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var stdinCh <-chan string
			if !tty {
				stdinCh = readStdin(bufio.NewScanner(os.Stdin))
			}

			readLine := func() (string, bool) {
				if termInput != nil {
					return termInput.ReadLine(os.Stdout, "")
				}
				s, ok := <-stdinCh
				return s, ok
			}

			var shutdownOnce sync.Once
			shutdownNow := func() {
				shutdownOnce.Do(func() {
					fmt.Fprintln(os.Stdout, "Shutting down...")
					reportShutdownResult(os.Stderr, shutdownFabric(cancel, termInput, func(shutdownCtx context.Context) error {
						if err := c.Shutdown(shutdownCtx); err != nil {
							return err
						}
						if cluster != nil {
							return cluster.Shutdown(shutdownCtx)
						}
						return nil
					}))
				})
			}
			defer shutdownNow()

			if termInput != nil {
				termInput.OnQuit = func() {
					shutdownNow()
					exitProcess(0)
				}
			}

			// SIGTERM only; Ctrl+C is handled as raw 0x03 via byteCh.
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGTERM)
			go func() {
				<-sigCh
				fmt.Fprintln(os.Stdout)
				shutdownNow()
				exitProcess(0)
			}()

			startErr := make(chan error, 1)
			go func() {
				err := c.Start(ctx)
				if err != nil {
					// Close bus so RenderStartup unblocks on failure.
					startupBus.Close()
				}
				startErr <- err
			}()

			renderer.RenderStartup(startupBus, td.Fabric.Name, len(td.Agents), startupSummary)

			select {
			case err := <-startErr:
				if err != nil {
					return fmt.Errorf("start fabric: %w", err)
				}
			default:
			}

			prompt := "  Enter your request: "
			if !tty {
				prompt = "Enter your request: "
			}

			type requestResult struct {
				result string
				err    error
			}

			var resultCh chan requestResult
			paused := false

		mainLoop:
			for {
				if resultCh == nil {
					mode, ok := promptInteractionMode(os.Stdout, readLine, tty, termInput)
					if !ok {
						break
					}
					switch mode {
					case "quit":
						break mainLoop
					case "chat":
						if escalation := handleAgentChat(ctx, c, renderer, readLine, tty, termInput); escalation != "" {
							reqBus := event.NewBus()
							c.SetEvents(reqBus)
							go func() { renderer.RenderRequest(reqBus) }()
							resultCh = make(chan requestResult, 1)
							go func() {
								result, err := c.HandleRequest(ctx, escalation)
								reqBus.Close()
								resultCh <- requestResult{result, err}
							}()
							if termInput != nil {
								termInput.EnterRaw()
							}
						}
						continue
					case "status":
						printStatus(ctx, c)
						continue
					}

					var input string
					if termInput != nil {
						if err := termInput.EnterRaw(); err == nil {
							input, ok = termInput.ReadMultiLine(os.Stdout, prompt, "e.g. Build a new login page with Next.js")
							termInput.ExitRaw()
						} else {
							input, ok = termInput.ReadLinePlaceholder(os.Stdout, prompt, "e.g. Build a new login page with Next.js")
						}
					} else {
						fmt.Print(prompt)
						input, ok = <-stdinCh
					}
					if !ok {
						break
					}
					input = strings.TrimSpace(input)
					if input == "" {
						continue
					}
					if input == "exit" || input == "quit" {
						break mainLoop
					}
					if input == "status" {
						printStatus(ctx, c)
						continue
					}
					if handled := handleDirectAgentCommand(ctx, c, renderer, input, tty); handled {
						continue
					}
					if strings.HasPrefix(input, "!") {
						if escalation := handleAgentChat(ctx, c, renderer, readLine, tty, termInput); escalation != "" {
							input = escalation
						} else {
							continue
						}
					}

					reqBus := event.NewBus()
					c.SetEvents(reqBus)

					go func() {
						renderer.RenderRequest(reqBus)
					}()

					resultCh = make(chan requestResult, 1)
					go func() {
						result, err := c.HandleRequest(ctx, input)
						reqBus.Close()
						resultCh <- requestResult{result, err}
					}()

					if termInput != nil {
						termInput.EnterRaw()
					}
				}

				if resultCh != nil {
					if termInput != nil {
						keyCh := termInput.StartKeyEvents()
						userQueryCh := c.GetUserQueryCh()

						select {
						case key := <-keyCh:
							termInput.StopKeyEvents()
							handleKey(key, c, renderer, termInput, readLine, ctx, tty, &paused)

						case res := <-resultCh:
							termInput.StopKeyEvents()
							termInput.Drain()
							termInput.ExitRaw()
							resultCh = nil
							paused = false
							if res.err == conductor.ErrRequestCancelled {
								renderer.RenderSeparator()
								continue
							}
							if res.err != nil {
								fmt.Fprintf(os.Stderr, "Error: %v\n", res.err)
								continue
							}
							if res.result != "" {
								renderer.RenderResults(res.result)
							}
							renderer.RenderSummary()
							renderer.RenderSeparator()

						case query, ok := <-userQueryCh:
							termInput.StopKeyEvents()
							if ok && query != nil {
								termInput.Drain()
								termInput.ExitRaw()
								renderer.Pause()
								handleUserQuery(query, readLine, renderer, tty, termInput)
								renderer.Resume()
								termInput.EnterRaw()
							}
						}
					} else {
						userQueryCh := c.GetUserQueryCh()
						select {
						case input, ok := <-stdinCh:
							if !ok {
								return nil
							}
							input = strings.TrimSpace(input)
							switch {
							case (input == "stop" || input == "pause") && !paused:
								if c.PauseExecution() {
									paused = true
									renderer.Pause()
									fmt.Fprintln(os.Stdout, "Paused. Type 'resume' or 'cancel'.")
								} else {
									fmt.Fprintln(os.Stdout, "Cannot pause during decomposition. Use 'cancel' to abort.")
								}
							case input == "resume" && paused:
								c.ResumeExecution()
								paused = false
								renderer.Resume()
								fmt.Fprintln(os.Stdout, "Resumed.")
							case input == "cancel":
								c.CancelExecution()
								if paused {
									paused = false
									renderer.Resume()
								}
								fmt.Fprintln(os.Stdout, "Cancelled.")
							case strings.HasPrefix(input, "!"):
								if !paused {
									renderer.Pause()
								}
								_ = handleAgentChat(ctx, c, renderer, readLine, tty, termInput)
								if !paused {
									renderer.Resume()
								}
							case isDirectAgentCommandInput(input):
								if !paused {
									renderer.Pause()
								}
								handleDirectAgentCommand(ctx, c, renderer, input, tty)
								if !paused {
									renderer.Resume()
								}
							case paused:
								fmt.Fprintln(os.Stdout, "Type 'resume' or 'cancel'.")
							}
						case res := <-resultCh:
							resultCh = nil
							paused = false
							if res.err == conductor.ErrRequestCancelled {
								renderer.RenderSeparator()
								continue
							}
							if res.err != nil {
								fmt.Fprintf(os.Stderr, "Error: %v\n", res.err)
								continue
							}
							if res.result != "" {
								renderer.RenderResults(res.result)
							}
							renderer.RenderSummary()
							renderer.RenderSeparator()
						case query, ok := <-userQueryCh:
							if ok && query != nil {
								renderer.Pause()
								handleUserQuery(query, readLine, renderer, tty, termInput)
								renderer.Resume()
							}
						}
					}
				}
			}

			return nil
		},
	}
	cmd.Flags().StringVar(&configFile, "config", "agents.yaml", "Path to agents.yaml")
	cmd.Flags().StringVar(&agentsDir, "agents-dir", "", "Override agents directory")
	cmd.Flags().StringVar(&dataDir, "data-dir", config.DefaultDataDir(), "Data directory")
	cmd.Flags().BoolVar(&debug, "debug", false, "Log full LLM requests/responses as JSONL")
	cmd.Flags().BoolVar(&skipVerify, "skip-verify", false, "Skip agent manifest integrity verification")
	cmd.Flags().StringVar(&listenAddr, "listen", ":50050", "Conductor gRPC listen address for external-node mode")
	cmd.Flags().StringVar(&advertiseAddr, "advertise", "", "Conductor gRPC advertise address for external-node mode")
	cmd.Flags().StringVar(&controlPlaneAddress, "control-plane-address", "", "Reachable address of an external control-plane API")
	cmd.Flags().BoolVar(&externalNodes, "external-nodes", false, "Run in external-node mode: the conductor hosts a gRPC server and agent instances run in external node hosts discovered through the control plane")
	cmd.Flags().IntVar(&bootstrapNodes, "bootstrap-nodes", 0, "Automatically start N local external nodes when running in external-node mode without a configured control-plane API (default: 1)")
	return cmd
}

// promptNewProject asks the user for a project name and directory.
// Returns name, absolute dir path, and ok.
func promptNewProject(w io.Writer, readLine func() (string, bool), tty bool) (string, string, bool) {
	if tty {
		fmt.Fprintf(w, "  %sProject name:%s ", ui.Bold, ui.Reset)
	} else {
		fmt.Fprint(w, "Project name: ")
	}
	name, ok := readLine()
	if !ok {
		return "", "", false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", false
	}
	if !config.ValidProjectName(name) {
		fmt.Fprintln(w, "Invalid name: must be lowercase alphanumeric with hyphens, starting with a letter.")
		return "", "", false
	}

	defaultDir := filepath.Join(config.DefaultProjectsBase(), name)
	if tty {
		fmt.Fprintf(w, "  %sDirectory%s [%s]: ", ui.Bold, ui.Reset, defaultDir)
	} else {
		fmt.Fprintf(w, "Directory [%s]: ", defaultDir)
	}
	dirInput, ok := readLine()
	if !ok {
		return "", "", false
	}
	dirInput = strings.TrimSpace(dirInput)
	dir := defaultDir
	if dirInput != "" {
		var err error
		dir, err = filepath.Abs(dirInput)
		if err != nil {
			fmt.Fprintf(w, "Invalid path: %v\n", err)
			return "", "", false
		}
	}

	return name, dir, true
}

func verifyCmd() *cobra.Command {
	var (
		agentsDir    string
		regenerate   bool
		signingKey   string
		generateKey  string
		publicKeyOut string
	)
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify agent bundle integrity, signatures, and manifest state",
		RunE: func(cmd *cobra.Command, args []string) error {
			configPath := "agents.yaml"
			if agentsDir == "" {
				td, err := config.LoadFabricDef(configPath)
				if err == nil && td.AgentsDir != "" {
					if err := config.ResolvePathsRelativeToConfig(td, configPath); err != nil {
						return err
					}
					agentsDir = td.AgentsDir
				} else {
					agentsDir = "defaults/agents"
				}
			}

			if generateKey != "" {
				publicKey, privateKey, err := config.GenerateBundleKeyPair()
				if err != nil {
					return fmt.Errorf("generate bundle signing key: %w", err)
				}
				if err := config.WriteBundlePrivateKey(generateKey, privateKey); err != nil {
					return fmt.Errorf("write private key: %w", err)
				}
				if publicKeyOut == "" {
					publicKeyOut = generateKey + ".pub"
				}
				if err := config.WriteBundlePublicKey(publicKeyOut, publicKey); err != nil {
					return fmt.Errorf("write public key: %w", err)
				}
				fmt.Printf("Bundle signing key written to %s\n", generateKey)
				fmt.Printf("Bundle public key written to %s\n", publicKeyOut)
				if !regenerate {
					return nil
				}
				if signingKey == "" {
					signingKey = generateKey
				}
			}

			if regenerate {
				td, err := config.LoadFabricDef(configPath)
				if err != nil {
					return fmt.Errorf("load fabric definition: %w", err)
				}
				if err := config.ResolvePathsRelativeToConfig(td, configPath); err != nil {
					return err
				}
				if agentsDir != "" {
					td.AgentsDir = agentsDir
					td.Agents = nil
					if err := td.ResolveAgents(); err != nil {
						return fmt.Errorf("resolve agents from %q: %w", agentsDir, err)
					}
				}

				manifest, err := config.GenerateManifest(td.AgentsDir)
				if err != nil {
					return fmt.Errorf("generate manifest: %w", err)
				}
				manifestPath := config.ManifestPath(td.AgentsDir)
				if err := config.WriteManifest(manifestPath, manifest); err != nil {
					return fmt.Errorf("write manifest: %w", err)
				}

				bundle, err := config.GenerateSignedBundle(td)
				if err != nil {
					return fmt.Errorf("generate bundle: %w", err)
				}
				if signingKey != "" {
					privateKey, err := config.LoadBundlePrivateKey(signingKey)
					if err != nil {
						return fmt.Errorf("load signing key: %w", err)
					}
					if err := config.SignBundle(bundle, privateKey); err != nil {
						return fmt.Errorf("sign bundle: %w", err)
					}
				}
				bundlePath := config.BundlePath(td.AgentsDir)
				if err := config.WriteSignedBundle(bundlePath, bundle); err != nil {
					return fmt.Errorf("write bundle: %w", err)
				}

				fmt.Printf("Manifest regenerated: %s (%d files)\n", manifestPath, len(manifest.Checksums))
				fmt.Printf("Bundle written: %s\n", bundlePath)
				if signingKey != "" {
					fmt.Printf("Bundle signature: %s\n", "present")
				} else {
					fmt.Printf("Bundle signature: %s\n", "absent")
				}
				return nil
			}

			td, err := config.LoadFabricDef(configPath)
			if err == nil {
				if err := config.ResolvePathsRelativeToConfig(td, configPath); err != nil {
					return err
				}
				if agentsDir != "" {
					td.AgentsDir = agentsDir
					td.Agents = nil
					if err := td.ResolveAgents(); err != nil {
						return fmt.Errorf("resolve agents from %q: %w", agentsDir, err)
					}
				}
			}
			if td == nil {
				td = &config.FabricDef{
					Fabric:    config.FabricMeta{Name: "local", Version: 1},
					AgentsDir: agentsDir,
				}
				if err := td.ResolveAgents(); err != nil {
					return fmt.Errorf("resolve agents: %w", err)
				}
			}

			result, err := config.VerifySignedBundle(td)
			if err != nil {
				return fmt.Errorf("verify: %w", err)
			}

			bundlePath := config.BundlePath(td.AgentsDir)
			if result.SignedBundleUsed {
				fmt.Printf("PASS: signed bundle verified: %s\n", bundlePath)
				if result.SignatureVerified {
					fmt.Println("Signature verification: passed")
				}
			} else if result.ManifestVerified {
				fmt.Printf("PASS: agent manifest verified: %s\n", config.ManifestPath(td.AgentsDir))
			} else {
				fmt.Println("PASS: bundle digests computed from local agent definitions")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&agentsDir, "agents-dir", "", "Agents directory (defaults to agents_dir from agents.yaml)")
	cmd.Flags().BoolVar(&regenerate, "regenerate", false, "Regenerate manifest and bundle metadata after intentional changes")
	cmd.Flags().StringVar(&signingKey, "signing-key", "", "Path to an Ed25519 private key used to sign the bundle")
	cmd.Flags().StringVar(&generateKey, "generate-key", "", "Generate a new Ed25519 private key at the given path")
	cmd.Flags().StringVar(&publicKeyOut, "public-key-out", "", "Path to write the generated Ed25519 public key")
	return cmd
}

func handleKey(key ui.KeyEvent, c *conductor.Conductor, renderer *ui.Renderer, termInput *ui.TermInput, readLine func() (string, bool), ctx context.Context, tty bool, paused *bool) {
	switch {
	case key.Rune == 'p' && !*paused:
		if c.PauseExecution() {
			*paused = true
			renderer.Pause()
			fmt.Fprintf(os.Stdout, "\r  %sPaused.%s Press %sr%s to resume or %sc%s to cancel.\r\n",
				ui.Yellow, ui.Reset, ui.Bold, ui.Reset, ui.Bold, ui.Reset)
		}
	case key.Rune == 'r' && *paused:
		c.ResumeExecution()
		*paused = false
		renderer.Resume()
	case key.Rune == 'c':
		c.CancelExecution()
		if *paused {
			*paused = false
			renderer.Resume()
		}
	case key.Key == "ctrl-c":
		termInput.Drain()
		termInput.ExitRaw()
		if termInput.ConfirmQuit(os.Stdout) {
			return // OnQuit handles shutdown
		}
		// Not quitting — cancel execution instead.
		c.CancelExecution()
		if *paused {
			*paused = false
			renderer.Resume()
		}
		termInput.EnterRaw()
	case key.Rune == '!' || key.Key == "tab":
		termInput.Drain()
		termInput.ExitRaw()
		if !*paused {
			renderer.Pause()
		}
		_ = handleAgentChat(ctx, c, renderer, readLine, tty, termInput)
		if !*paused {
			renderer.Resume()
		}
		termInput.EnterRaw()
	}
}

func statusCmd() *cobra.Command {
	var configFile string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show fabric agent status and token usage",
		RunE: func(cmd *cobra.Command, args []string) error {
			td, err := config.LoadFabricDef(configFile)
			if err != nil {
				return fmt.Errorf("load fabric definition: %w", err)
			}

			fmt.Printf("Fabric: %s (v%d)\n\n", td.Fabric.Name, td.Fabric.Version)
			fmt.Printf("%-15s %-30s %s\n", "AGENT", "MODEL", "CAPABILITIES")
			fmt.Println(strings.Repeat("-", 70))
			for _, a := range td.Agents {
				caps := strings.Join(a.Capabilities, ", ")
				fmt.Printf("%-15s %-30s %s\n", a.Name, a.Model, caps)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configFile, "config", "agents.yaml", "Path to agents.yaml")
	return cmd
}

func printStatus(ctx context.Context, c *conductor.Conductor) {
	runtimeStatus := c.RuntimeStatus(ctx)
	fmt.Printf(
		"\nRuntime: %s\nControl plane: %s\nNodes: %d total (%d ready)\nInstances: %d total (%d ready)\n",
		runtimeStatus.Mode,
		statusValueOrUnknown(runtimeStatus.ControlPlaneAddress),
		runtimeStatus.NodeCount,
		runtimeStatus.ReadyNodeCount,
		runtimeStatus.InstanceCount,
		runtimeStatus.ReadyInstanceCount,
	)
	states := c.AgentStates(ctx)
	fmt.Printf("\n%-15s %-30s %8s %8s %6s\n", "AGENT", "MODEL", "IN_TOK", "OUT_TOK", "CALLS")
	fmt.Println(strings.Repeat("-", 75))
	for _, s := range states {
		fmt.Printf("%-15s %-30s %8d %8d %6d\n",
			s.Name, s.Model, s.InputTokens, s.OutputTokens, s.TotalCalls)
	}
	fmt.Println()
}

func runtimeModeLabel(externalNodes bool) string {
	if externalNodes {
		return "External-Node Distributed"
	}
	return "Local"
}

func startupControlPlaneAddress(externalNodes bool, controlPlaneAddress string) string {
	if !externalNodes {
		return ""
	}
	return strings.TrimSpace(controlPlaneAddress)
}

func statusValueOrUnknown(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "n/a"
	}
	return value
}

func readStdin(scanner *bufio.Scanner) <-chan string {
	ch := make(chan string, 1)
	go func() {
		defer close(ch)
		for scanner.Scan() {
			ch <- scanner.Text()
		}
	}()
	return ch
}

func shutdownFabric(cancel context.CancelFunc, termInput *ui.TermInput, shutdown func(context.Context) error) error {
	if termInput != nil {
		termInput.Close()
	}
	cancel()

	shutCtx, shutCancel := context.WithTimeout(context.Background(), cliShutdownTimeout)
	defer shutCancel()
	return shutdown(shutCtx)
}

func reportShutdownResult(w io.Writer, err error) {
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		return
	case errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintln(w, "Shutdown timed out; forcing exit.")
	default:
		fmt.Fprintf(w, "Shutdown warning: %v\n", err)
	}
}

func parseInteractionMode(input string) string {
	v := strings.ToLower(strings.TrimSpace(input))
	switch v {
	case "", "1", "w", "work", "run", "request":
		return "work"
	case "2", "c", "chat", "agent":
		return "chat"
	case "3", "s", "status":
		return "status"
	case "q", "quit", "exit":
		return "quit"
	default:
		return ""
	}
}

func promptInteractionMode(w io.Writer, readLine func() (string, bool), tty bool, ti *ui.TermInput) (string, bool) {
	if !tty || ti == nil {
		// Fallback for non-TTY
		for {
			fmt.Fprintln(w, "Choose action:")
			fmt.Fprintln(w, "[1] Coordinated work")
			fmt.Fprintln(w, "[2] Chat with an agent")
			fmt.Fprintln(w, "[3] Status")
			fmt.Fprintln(w, "[q] Quit")
			fmt.Fprint(w, "Selection: ")

			input, ok := readLine()
			if !ok {
				return "", false
			}
			mode := parseInteractionMode(input)
			if mode != "" {
				return mode, true
			}
			fmt.Fprintln(w, "Invalid selection. Choose 1, 2, 3, or q.")
		}
	}

	// Interactive TTY mode
	ti.EnterRaw()
	defer ti.ExitRaw()

	options := []struct {
		label string
		ret   string
	}{
		{"Work on a project", "work"},
		{"Chat with an agent", "chat"},
		{"View agents", "status"},
		{"Quit", "quit"},
	}

	selected := 0
	keyCh := ti.StartKeyEvents()
	defer ti.StopKeyEvents()

	drawMenu := func() {
		fmt.Fprintf(w, "%s\r  %sChoose Action%s\n", ui.ClearLn, ui.Bold, ui.Reset)
		for i, opt := range options {
			if i == selected {
				fmt.Fprintf(w, "%s\r  %s%s> %s%s\n", ui.ClearLn, ui.Bold, ui.Blue, opt.label, ui.Reset)
			} else {
				fmt.Fprintf(w, "%s\r    %s\n", ui.ClearLn, opt.label)
			}
		}
	}

	eraseMenu := func() {
		// Move cursor up by (len(options) + 1) lines and clear each
		for i := 0; i < len(options)+1; i++ {
			fmt.Fprintf(w, "\r%s%s", ui.MoveUp, ui.ClearLn)
		}
	}

	// Initial draw
	fmt.Fprintf(w, "\r%s", ui.ClearLn)
	drawMenu()

	for {
		key, ok := <-keyCh
		if !ok {
			eraseMenu()
			return "", false
		}

		switch {
		case key.Key == "up":
			selected--
			if selected < 0 {
				selected = len(options) - 1
			}
			eraseMenu()
			drawMenu()
		case key.Key == "down":
			selected++
			if selected >= len(options) {
				selected = 0
			}
			eraseMenu()
			drawMenu()
		case key.Key == "enter":
			eraseMenu()
			return options[selected].ret, true
		case key.Key == "ctrl-c":
			ti.StopKeyEvents()
			ti.Drain()
			ti.ExitRaw()
			if ti.ConfirmQuit(w) {
				eraseMenu()
				return "", false
			}
			// Re-enter raw mode and redraw.
			ti.EnterRaw()
			keyCh = ti.StartKeyEvents()
			eraseMenu()
			drawMenu()
		case key.Key == "ctrl-d":
			eraseMenu()
			return "", false
		}
	}
}

type directAgentCommand struct {
	mode    string // "ask" or "do"
	agent   string
	message string
}

func isDirectAgentCommandInput(input string) bool {
	_, matched, _ := parseDirectAgentCommand(input)
	return matched
}

func parseDirectAgentCommand(input string) (directAgentCommand, bool, error) {
	var cmd directAgentCommand
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return cmd, false, nil
	}

	lower := strings.ToLower(trimmed)
	mode := ""
	cmdPrefix := ""
	switch {
	case lower == "/ask" || strings.HasPrefix(lower, "/ask "):
		mode = "ask"
		cmdPrefix = "/ask"
	case lower == "/do" || strings.HasPrefix(lower, "/do "):
		mode = "do"
		cmdPrefix = "/do"
	default:
		return cmd, false, nil
	}

	rest := strings.TrimSpace(trimmed[len(cmdPrefix):])
	if rest == "" {
		return cmd, true, fmt.Errorf("usage: /%s <agent> <message>", mode)
	}

	parts := strings.SplitN(rest, " ", 2)
	if len(parts) < 2 {
		return cmd, true, fmt.Errorf("usage: /%s <agent> <message>", mode)
	}

	cmd = directAgentCommand{
		mode:    mode,
		agent:   strings.TrimSpace(parts[0]),
		message: strings.TrimSpace(parts[1]),
	}
	if cmd.agent == "" || cmd.message == "" {
		return cmd, true, fmt.Errorf("usage: /%s <agent> <message>", mode)
	}
	return cmd, true, nil
}

func handleDirectAgentCommand(ctx context.Context, c *conductor.Conductor, renderer *ui.Renderer, input string, tty bool) bool {
	cmd, matched, err := parseDirectAgentCommand(input)
	if !matched {
		return false
	}

	w := renderer.Writer()
	if err != nil {
		fmt.Fprintln(w, err.Error())
		return true
	}

	entries := c.AgentChatInfo()
	known := make(map[string]bool, len(entries))
	var names []string
	for _, e := range entries {
		known[e.Name] = true
		names = append(names, e.Name)
	}
	if !known[cmd.agent] {
		fmt.Fprintf(w, "Unknown agent %q. Available: %s\n", cmd.agent, strings.Join(names, ", "))
		return true
	}

	taskID := c.RunningTaskForAgent(cmd.agent)
	taskContext := c.TaskContextForAgent(cmd.agent)
	msg := cmd.message
	if cmd.mode == "do" {
		msg = "Please do this: " + cmd.message
		if taskContext == "" {
			fmt.Fprintf(w, "Note: %s has no active task; this will be treated as guidance, not execution.\n", cmd.agent)
		}
	}

	resp, chatErr := chatWithSpinner(ctx, c, conductor.ChatRequest{
		AgentName:   cmd.agent,
		Message:     msg,
		TaskContext: taskContext,
	}, w, tty)
	if chatErr != nil {
		fmt.Fprintf(w, "Chat error: %v\n", chatErr)
		return true
	}

	ui.RenderChatResponse(w, cmd.agent, resp.Response, tty, renderer.Glamour())

	if resp.Amendment == nil {
		return true
	}
	if resp.Amendment.TaskID == "" {
		resp.Amendment.TaskID = taskID
	}
	if resp.Amendment.TaskID == "" {
		fmt.Fprintln(w, "Amendment ignored: no active task to amend.")
		return true
	}

	chatCtx := fmt.Sprintf("User: %s\n\nYou: %s", msg, resp.Response)
	if resp.Amendment.Structural {
		if err := c.RestructureGraph(ctx, msg, resp.Amendment.NewDescription); err != nil {
			fmt.Fprintf(w, "Restructure failed: %v\n", err)
		} else if tty {
			fmt.Fprintf(w, "  %s→ Graph restructured — re-executing%s\n", ui.Yellow, ui.Reset)
		} else {
			fmt.Fprintln(w, "Graph restructured — re-executing")
		}
		return true
	}

	amendID := resp.Amendment.TaskID
	promoted, err := c.AmendTask(ctx, amendID, resp.Amendment.NewDescription, chatCtx)
	if err != nil {
		fmt.Fprintf(w, "Amendment failed: %v\n", err)
		return true
	}
	if promoted {
		if tty {
			fmt.Fprintf(w, "  %s→ Graph restructured — re-executing%s\n", ui.Yellow, ui.Reset)
		} else {
			fmt.Fprintln(w, "Graph restructured — re-executing")
		}
		return true
	}
	if tty {
		fmt.Fprintf(w, "  %s→ Task %s amended — re-executing with your feedback%s\n", ui.Yellow, amendID, ui.Reset)
	} else {
		fmt.Fprintf(w, "Task %s amended — re-executing with your feedback\n", amendID)
	}
	return true
}

// handleAgentChat runs a multi-turn interactive chat with a selected agent.
// The conversation loops until the user sends an empty reply.
// Returns a non-empty string when the agent escalates to coordinated work,
// containing the user's original message to be fed into HandleRequest.
func handleAgentChat(ctx context.Context, c *conductor.Conductor, renderer *ui.Renderer, readLine func() (string, bool), tty bool, ti *ui.TermInput) string {
	w := renderer.Writer()

	entries := c.AgentChatInfo()
	agents := make([]ui.AgentInfo, len(entries))
	for i, e := range entries {
		agents[i] = ui.AgentInfo{
			Name:     e.Name,
			Model:    e.Model,
			Status:   e.Status,
			TaskID:   e.TaskID,
			TaskDesc: e.TaskDesc,
		}
	}

	agentName := ui.PickAgent(w, agents, readLine, tty, ti)
	if agentName == "" {
		return ""
	}

	var msg string
	var ok bool
	if tty && ti != nil {
		if err := ti.EnterRaw(); err == nil {
			msg, ok = ti.ReadMultiLine(w, "  "+ui.Bold+">"+ui.Reset+" ", "Message for "+agentName+"...")
			ti.ExitRaw()
		} else {
			fmt.Fprintf(w, "  %sMessage for %s:%s ", ui.Bold, agentName, ui.Reset)
			msg, ok = readLine()
		}
	} else {
		if tty {
			fmt.Fprintf(w, "  %sMessage for %s:%s ", ui.Bold, agentName, ui.Reset)
		} else {
			fmt.Fprintf(w, "Message for %s: ", agentName)
		}
		msg, ok = readLine()
	}
	if !ok {
		return ""
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}

	taskID := c.RunningTaskForAgent(agentName)
	taskContext := c.TaskContextForAgent(agentName)

	var history []conductor.ChatMessage

	for {
		resp, err := chatWithSpinner(ctx, c, conductor.ChatRequest{
			AgentName:   agentName,
			Message:     msg,
			TaskContext: taskContext,
			History:     history,
		}, w, tty)
		if err != nil {
			fmt.Fprintf(w, "Chat error: %v\n", err)
			return ""
		}

		ui.RenderChatResponse(w, agentName, resp.Response, tty, renderer.Glamour())

		history = append(history,
			conductor.ChatMessage{Role: "user", Content: msg},
			conductor.ChatMessage{Role: "assistant", Content: resp.Response},
		)

		if resp.Amendment != nil {
			if resp.Amendment.TaskID == "" {
				resp.Amendment.TaskID = taskID
			}
		}
		if resp.Amendment != nil && resp.Amendment.TaskID != "" {

			var chatCtxBuilder strings.Builder
			for _, h := range history {
				if h.Role == "user" {
					chatCtxBuilder.WriteString("User: ")
				} else {
					chatCtxBuilder.WriteString("You: ")
				}
				chatCtxBuilder.WriteString(h.Content)
				chatCtxBuilder.WriteString("\n\n")
			}
			chatCtxBuilder.WriteString("User: ")
			chatCtxBuilder.WriteString(msg)
			chatCtxBuilder.WriteString("\n\nYou: ")
			chatCtxBuilder.WriteString(resp.Response)
			chatCtx := chatCtxBuilder.String()

			if resp.Amendment.Structural {
				if err := c.RestructureGraph(ctx, msg, resp.Amendment.NewDescription); err != nil {
					fmt.Fprintf(w, "  Restructure failed: %v\n", err)
				} else {
					if tty {
						fmt.Fprintf(w, "  %s→ Graph restructured — re-executing%s\n", ui.Yellow, ui.Reset)
					} else {
						fmt.Fprintf(w, "Graph restructured — re-executing\n")
					}
					return "" // exit chat so re-dispatch happens immediately
				}
			} else {
				amendID := resp.Amendment.TaskID
				promoted, err := c.AmendTask(ctx, amendID, resp.Amendment.NewDescription, chatCtx)
				if err != nil {
					fmt.Fprintf(w, "  Amendment failed: %v\n", err)
				} else if promoted {
					if tty {
						fmt.Fprintf(w, "  %s→ Graph restructured — re-executing%s\n", ui.Yellow, ui.Reset)
					} else {
						fmt.Fprintf(w, "Graph restructured — re-executing\n")
					}
					return ""
				} else {
					if tty {
						fmt.Fprintf(w, "  %s→ Task %s amended — re-executing with your feedback%s\n", ui.Yellow, amendID, ui.Reset)
					} else {
						fmt.Fprintf(w, "Task %s amended — re-executing with your feedback\n", amendID)
					}
					return "" // exit chat so re-dispatch happens immediately
				}
			}
		}

		if resp.Escalation != "" {
			if tty {
				fmt.Fprintf(w, "  %s→ Escalating to coordinated work: %s%s\n", ui.Yellow, resp.Escalation, ui.Reset)
			} else {
				fmt.Fprintf(w, "Escalating to coordinated work: %s\n", resp.Escalation)
			}
			return msg
		}

		if resp.Done {
			return ""
		}

		var reply string
		if len(resp.SuggestedReplies) > 0 {
			reply = ui.PromptReplyWithOptions(w, resp.SuggestedReplies, readLine, tty, ti)
		} else {
			reply = ui.PromptReply(w, readLine, tty, ti)
		}
		if reply == "" {
			return ""
		}
		msg = reply
	}
}

func chatWithSpinner(ctx context.Context, c *conductor.Conductor, req conductor.ChatRequest, w io.Writer, tty bool) (*conductor.ChatResponse, error) {
	spinner := ui.NewSpinner(w, tty)
	spinner.Start("Thinking...")

	type chatResult struct {
		resp *conductor.ChatResponse
		err  error
	}
	ch := make(chan chatResult, 1)
	go func() {
		r, e := c.Chat(ctx, req)
		ch <- chatResult{r, e}
	}()

	wittyIdx := 0
	wittyTicker := time.NewTicker(3 * time.Second)
	defer wittyTicker.Stop()
	for {
		select {
		case res := <-ch:
			spinner.Stop()
			return res.resp, res.err
		case <-wittyTicker.C:
			wittyIdx++
			spinner.Update(ui.WittyStatus(wittyIdx))
		}
	}
}

func handleUserQuery(query *conductor.UserQuery, readLine func() (string, bool), renderer *ui.Renderer, tty bool, ti *ui.TermInput) {
	w := renderer.Writer()
	ui.RenderAgentQuery(w, query.AgentName, query.Question, tty)

	answer := ui.PromptFreeText(w, readLine, tty, ti)
	query.ResponseCh <- answer
}

func metricsCmd() *cobra.Command {
	var (
		dataDir    string
		outputJSON bool
	)
	cmd := &cobra.Command{
		Use:   "metrics",
		Short: "Show token usage, cost estimates, and latency from debug logs",
		RunE: func(cmd *cobra.Command, args []string) error {
			debugDir := filepath.Join(dataDir, "debug")
			report, err := metrics.LoadFromDebugDir(debugDir)
			if err != nil {
				return fmt.Errorf("load debug logs from %s: %w", debugDir, err)
			}

			if outputJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}

			fmt.Printf("AgentFab Metrics Report\n")
			fmt.Printf("======================\n\n")

			if len(report.Agents) == 0 {
				fmt.Println("No debug logs found. Run with --debug to enable logging.")
				return nil
			}

			fmt.Printf("Per-Agent Usage:\n")
			fmt.Printf("  %-15s %-35s %6s %10s %10s %10s %8s %10s\n",
				"Agent", "Model", "Calls", "Input", "Output", "Total", "Avg ms", "Est $")
			for _, a := range report.Agents {
				modelShort := a.Model
				if len(modelShort) > 35 {
					modelShort = modelShort[:32] + "..."
				}
				fmt.Printf("  %-15s %-35s %6d %10d %10d %10d %8d %10.4f\n",
					a.Agent, modelShort, a.TotalCalls,
					a.InputTokens, a.OutputTokens, a.TotalTokens,
					a.AvgLatencyMs, a.EstCostUSD)
			}

			fmt.Printf("\nPer-Model Usage:\n")
			fmt.Printf("  %-40s %6s %10s %10s %10s %10s\n",
				"Model", "Calls", "Input", "Output", "Total", "Est $")
			for _, m := range report.Models {
				modelShort := m.Model
				if len(modelShort) > 40 {
					modelShort = modelShort[:37] + "..."
				}
				fmt.Printf("  %-40s %6d %10d %10d %10d %10.4f\n",
					modelShort, m.TotalCalls,
					m.InputTokens, m.OutputTokens, m.TotalTokens,
					m.EstCostUSD)
			}

			fmt.Printf("\nTotals:\n")
			fmt.Printf("  Calls: %d | Input: %d | Output: %d | Total: %d tokens\n",
				report.TotalCalls, report.InputTokens, report.OutputTokens, report.TotalTokens)
			fmt.Printf("  Time: %dms | Est. Cost: $%.4f\n",
				report.TotalTimeMs, report.EstCostUSD)

			return nil
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", config.DefaultDataDir(), "Data directory")
	cmd.Flags().BoolVar(&outputJSON, "json", false, "Output as JSON")
	return cmd
}

func mustCwd() string {
	dir, err := os.Getwd()
	if err != nil {
		return "agentfab"
	}
	return dir
}

func findProjectEntry(entries []config.ProjectEntry, name string) (*config.ProjectEntry, error) {
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i], nil
		}
	}
	return nil, fmt.Errorf("project %q not found in registry", name)
}

func projectConfigPath(entry config.ProjectEntry) (string, error) {
	if strings.TrimSpace(entry.Dir) == "" {
		return "", fmt.Errorf("project %q has no directory recorded in the registry", entry.Name)
	}

	info, err := os.Stat(entry.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("project %q directory does not exist: %s", entry.Name, entry.Dir)
		}
		return "", fmt.Errorf("stat project %q directory %s: %w", entry.Name, entry.Dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project %q path is not a directory: %s", entry.Name, entry.Dir)
	}

	configFile := filepath.Join(entry.Dir, "agents.yaml")
	if _, err := os.Stat(configFile); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("project %q is missing %s", entry.Name, configFile)
		}
		return "", fmt.Errorf("stat project %q config %s: %w", entry.Name, configFile, err)
	}

	return configFile, nil
}

func projectWorkspaceEmpty(entry config.ProjectEntry) (bool, error) {
	if strings.TrimSpace(entry.Dir) == "" {
		return false, fmt.Errorf("project %q has no directory recorded in the registry", entry.Name)
	}

	info, err := os.Stat(entry.Dir)
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("project %q path is not a directory: %s", entry.Name, entry.Dir)
	}

	entries, err := os.ReadDir(entry.Dir)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

func shortProjectError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "workspace is empty"):
		return "empty workspace"
	case strings.Contains(msg, "missing") && strings.Contains(msg, "agents.yaml"):
		return "missing agents.yaml"
	case strings.Contains(msg, "directory does not exist"):
		return "missing directory"
	case strings.Contains(msg, "not a directory"):
		return "invalid path"
	default:
		return "invalid project"
	}
}

func promptRecreateProject(w io.Writer, readLine func() (string, bool), tty bool, entry config.ProjectEntry) (bool, error) {
	if tty {
		fmt.Fprintf(w, "  %sProject %q workspace is empty. Recreate it at %s? [Y/n]:%s ", ui.Bold, entry.Name, entry.Dir, ui.Reset)
	} else {
		fmt.Fprintf(w, "Project %q workspace is empty. Recreate it at %s? [Y/n]: ", entry.Name, entry.Dir)
	}

	answer, ok := readLine()
	if !ok {
		return false, nil
	}
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "" || answer == "y" || answer == "yes", nil
}
