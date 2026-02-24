package conductor

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/razvanmaftei/agentfab/internal/cluster"
	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/razvanmaftei/agentfab/internal/event"
	agentgrpc "github.com/razvanmaftei/agentfab/internal/grpc"
	"github.com/razvanmaftei/agentfab/internal/knowledge"
	"github.com/razvanmaftei/agentfab/internal/llm"
	"github.com/razvanmaftei/agentfab/internal/local"
	"github.com/razvanmaftei/agentfab/internal/message"
	"github.com/razvanmaftei/agentfab/internal/runtime"
	"github.com/razvanmaftei/agentfab/internal/taskgraph"
)

type Option func(*Conductor)

func WithCommFactory(f message.CommunicatorFactory) Option {
	return func(c *Conductor) { c.CommFactory = f }
}

func WithDiscovery(d runtime.Discovery) Option {
	return func(c *Conductor) { c.Discovery = d }
}

func WithLifecycle(l runtime.Lifecycle) Option {
	return func(c *Conductor) { c.Lifecycle = l }
}

func WithMeter(m runtime.ExtendedMeter) Option {
	return func(c *Conductor) { c.Meter = m }
}

func WithStorageFactory(f func(string) runtime.Storage) Option {
	return func(c *Conductor) { c.StorageFactory = f }
}

func WithDistributed() Option {
	return func(c *Conductor) { c.Distributed = true }
}

func WithConductorListenAddr(addr string) Option {
	return func(c *Conductor) { c.ConductorListenAddr = addr }
}

func WithDebugLog(d *llm.DebugStore) Option {
	return func(c *Conductor) { c.DebugLog = d }
}

var ErrRequestCancelled = fmt.Errorf("request cancelled by user")

// ModelFactory creates a ChatModel from a model ID string.
type ModelFactory func(ctx context.Context, modelID string) (model.ChatModel, error)

// Conductor orchestrates a fabric: setup, decomposition, scheduling, user I/O.
type Conductor struct {
	FabricDef      *config.FabricDef
	BaseDir        string
	CommFactory    message.CommunicatorFactory
	Comm           message.MessageCommunicator
	Discovery      runtime.Discovery
	Lifecycle      runtime.Lifecycle
	Meter          runtime.ExtendedMeter
	StorageFactory func(agentName string) runtime.Storage
	Logger         *message.Logger
	ModelFactory   ModelFactory
	Events         event.Bus
	DebugLog       *llm.DebugStore     // Optional; set before Start().
	Templates      []DecomposeTemplate // Decomposition templates loaded from defaults.

	// Distributed enables distributed mode where agents run as separate OS
	// processes communicating via gRPC.
	Distributed bool

	// ConductorListenAddr is the gRPC listen address for the conductor
	// in distributed mode. Defaults to ":50050".
	ConductorListenAddr string

	// SkipDisambiguation bypasses the requirement-clarity check before decomposition.
	// Set to true for headless/benchmark runners that have no user to answer queries.
	SkipDisambiguation bool

	// SkipScratchCleanup prevents automatic scratch directory cleanup after
	// HandleRequest completes. Set to true when the caller needs to inspect
	// scratch contents (e.g., capturing git diffs from cloned repos).
	SkipScratchCleanup bool

	// conductorGenerate is the generate function for the conductor's own LLM calls.
	conductorGenerate func(context.Context, []*schema.Message) (*schema.Message, error)

	// conductorDecomposeGenerate is like conductorGenerate but with capped output
	// tokens to prevent runaway decomposition responses on long/complex requests.
	conductorDecomposeGenerate func(context.Context, []*schema.Message) (*schema.Message, error)

	backgroundCtx    context.Context
	cancelBackground context.CancelFunc
	knowledgeWg      sync.WaitGroup

	mu                sync.RWMutex
	activeScheduler   *Scheduler
	activeGraph       *taskgraph.TaskGraph
	activeReqCancel   context.CancelFunc
	activeUserRequest string
	conductorQueryCh  chan *UserQuery // Pre-decomposition user queries (disambiguation).

	// Sleep state / idle curation.
	curationMu      sync.Mutex
	curationRunning map[string]bool // agent name → curation in progress
	sleepCancel     context.CancelFunc

	shutdownOnce        sync.Once
	conductorGRPCServer interface{ Stop() } // gRPC server for distributed mode (nil in local mode)
	shutdownErr         error
}

// New creates a new Conductor. events may be nil; options override local defaults.
func New(systemDef *config.FabricDef, baseDir string, factory ModelFactory, events event.Bus, opts ...Option) (*Conductor, error) {
	hub := local.NewHub()
	meter := local.NewMeter()
	backgroundCtx, cancelBackground := context.WithCancel(context.Background())

	c := &Conductor{
		FabricDef:   systemDef,
		BaseDir:     baseDir,
		CommFactory: hub,
		Discovery:   local.NewDiscovery(),
		Lifecycle:   local.NewLifecycle(),
		Meter:       meter,
		StorageFactory: func(name string) runtime.Storage {
			return local.NewStorage(baseDir, name)
		},
		ModelFactory:     factory,
		Events:           events,
		backgroundCtx:    backgroundCtx,
		cancelBackground: cancelBackground,
	}

	for _, opt := range opts {
		opt(c)
	}

	if c.Distributed {
		if err := c.setupDistributed(); err != nil {
			return nil, fmt.Errorf("distributed setup: %w", err)
		}
	}

	if c.Comm == nil {
		c.Comm = c.CommFactory.Register("conductor")
	}

	conductorStorage := c.StorageFactory("conductor")
	c.Logger = message.NewLogger(local.NewSharedAppender(conductorStorage))

	return c, nil
}

// setupDistributed replaces local infra with gRPC-backed implementations.
// An ephemeral cluster CA is created in memory; the private key never leaves
// the conductor process.
func (c *Conductor) setupDistributed() error {
	listenAddr := c.ConductorListenAddr
	if listenAddr == "" {
		listenAddr = ":50050"
	}

	ca, err := agentgrpc.NewClusterCA()
	if err != nil {
		return fmt.Errorf("create cluster CA: %w", err)
	}

	conductorCert, err := ca.IssueCert("conductor")
	if err != nil {
		return fmt.Errorf("issue conductor certificate: %w", err)
	}
	serverTLS := agentgrpc.ServerTLSConfig(conductorCert, ca.Pool())
	clientTLS := agentgrpc.ClientTLSConfig(conductorCert, ca.Pool())

	discovery := agentgrpc.NewStaticDiscovery()
	c.Discovery = discovery
	c.CommFactory = agentgrpc.NewCommFactory(discovery, serverTLS, clientTLS)

	conductorSrv, srvErr := agentgrpc.NewServer("conductor", listenAddr, 64, serverTLS)
	if srvErr != nil {
		return fmt.Errorf("create conductor gRPC server: %w", srvErr)
	}
	go func() {
		if err := conductorSrv.Serve(); err != nil {
			slog.Debug("conductor gRPC server stopped", "error", err)
		}
	}()

	actualAddr := conductorSrv.Addr()
	_, port, _ := net.SplitHostPort(actualAddr)
	conductorAddr := "localhost:" + port

	discovery.Register(context.Background(), "conductor", runtime.Endpoint{Address: conductorAddr})
	c.Comm = agentgrpc.NewCommunicator("conductor", conductorSrv, discovery, clientTLS)

	configFile := filepath.Join(c.BaseDir, "shared", "agents.yaml")
	lifecycle := agentgrpc.NewProcessLifecycle(configFile, c.BaseDir, conductorAddr, discovery)
	lifecycle.SetCA(ca)
	if c.DebugLog != nil {
		lifecycle.SetDebug(true)
	}
	c.Lifecycle = lifecycle

	c.conductorGRPCServer = conductorSrv

	return nil
}

// Start sets up the system and prepares it for requests.
func (c *Conductor) Start(ctx context.Context) error {
	// Local mode only — distributed mode registers with the actual gRPC address.
	if !c.Distributed {
		c.Discovery.Register(ctx, "conductor", runtime.Endpoint{Address: "local", Local: true})
	}

	if c.ModelFactory != nil {
		conductorModel := ""
		for _, a := range c.FabricDef.Agents {
			if a.Name == "conductor" {
				conductorModel = a.Model
				break
			}
		}
		if conductorModel != "" {
			var onChunk llm.ChunkCallback
			if c.Events != nil {
				onChunk = func(textSoFar string) {
					snippet := textSoFar
					if len(snippet) > 80 {
						snippet = snippet[len(snippet)-80:]
					}
					c.Events.Emit(event.Event{
						Type:         event.TaskProgress,
						TaskAgent:    "conductor",
						ProgressText: snippet,
					})
				}
			}
			c.conductorGenerate = func(callCtx context.Context, input []*schema.Message) (*schema.Message, error) {
				m, err := c.ModelFactory(callCtx, conductorModel)
				if err != nil {
					return nil, err
				}
				metered := &llm.MeteredModel{
					Model:     m,
					AgentName: "conductor",
					ModelID:   conductorModel,
					Meter:     c.Meter,
					OnChunk:   onChunk,
					DebugLog:  c.DebugLog,
					Options:   llm.ProviderOptions(conductorModel, c.FabricDef.Providers),
				}
				return metered.Generate(callCtx, input)
			}
			// Capped output tokens to prevent runaway decomposition responses.
			c.conductorDecomposeGenerate = func(callCtx context.Context, input []*schema.Message) (*schema.Message, error) {
				m, err := llm.NewChatModel(callCtx, conductorModel, &llm.ProviderConfig{MaxTokens: decomposeMaxOutputTokens}, c.FabricDef.Providers)
				if err != nil {
					m, err = c.ModelFactory(callCtx, conductorModel)
					if err != nil {
						return nil, err
					}
				}
				metered := &llm.MeteredModel{
					Model:     m,
					AgentName: "conductor",
					ModelID:   conductorModel,
					Meter:     c.Meter,
					OnChunk:   onChunk,
					DebugLog:  c.DebugLog,
					Options:   llm.ProviderOptions(conductorModel, c.FabricDef.Providers),
				}
				return metered.Generate(callCtx, input)
			}
		}
	}

	if c.Distributed {
		c.startClusterMonitor(ctx)
	}

	return Setup(ctx, c)
}

func (c *Conductor) startClusterMonitor(ctx context.Context) {
	lifecycle, ok := c.Lifecycle.(*agentgrpc.ProcessLifecycle)
	if !ok {
		return
	}

	knownAgents := make(map[string]bool, len(c.FabricDef.Agents)+1)
	knownAgents["conductor"] = true
	for _, def := range c.FabricDef.Agents {
		knownAgents[def.Name] = true
	}

	mon := &cluster.Monitor{
		Self: cluster.MemberInfo{
			Name:    "conductor",
			Role:    "conductor",
			Address: c.ConductorListenAddr,
			PID:     os.Getpid(),
		},
		StatePath:    cluster.StatePath(c.BaseDir),
		KnownMembers: knownAgents,
		OnMemberDead: func(dead cluster.MemberInfo) {
			if dead.Role != "agent" {
				return
			}
			slog.Warn("conductor: dead agent detected, respawning", "name", dead.Name)
			// Find the agent definition.
			for _, def := range c.FabricDef.Agents {
				if def.Name == dead.Name {
					if err := lifecycle.Respawn(ctx, def); err != nil {
						slog.Error("conductor: respawn failed", "agent", dead.Name, "error", err)
					}
					return
				}
			}
		},
	}
	go mon.Run(ctx)
}

// HandleRequest processes a user request end-to-end.
func (c *Conductor) HandleRequest(ctx context.Context, userRequest string) (string, error) {
	c.cancelIdleCuration()

	reqCtx, reqCancel := context.WithCancel(ctx)
	defer reqCancel()

	// Fallback: create conductorQueryCh if SetEvents wasn't called.
	c.mu.Lock()
	c.activeReqCancel = reqCancel
	c.activeUserRequest = userRequest
	if c.conductorQueryCh == nil {
		c.conductorQueryCh = make(chan *UserQuery, 1)
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.activeReqCancel = nil
		c.activeUserRequest = ""
		c.conductorQueryCh = nil
		c.mu.Unlock()
	}()

	requestID := newRequestID()
	requestStart := time.Now()
	reqCtx = runtime.WithRequestID(reqCtx, requestID)
	slog.Info("handling request", "request_id", requestID)

	c.Events.Emit(event.Event{Type: event.RequestReceived})

	if c.conductorGenerate == nil {
		return "", fmt.Errorf("conductor has no LLM configured")
	}

	var agentRoster []string
	for _, a := range c.FabricDef.Agents {
		if a.Name != "conductor" {
			agentRoster = append(agentRoster, a.Name)
		}
	}

	// Skipped in headless mode (benchmarks) where no user can answer queries.
	if !c.SkipDisambiguation {
		disambResult, disambErr := Disambiguate(reqCtx, c.conductorGenerate, userRequest, agentRoster)
		if disambErr == nil && !disambResult.Clear {
			answerCh := make(chan string, 1)
			query := &UserQuery{
				AgentName:  "conductor",
				Question:   disambResult.Question,
				ResponseCh: answerCh,
			}
			c.Events.Emit(event.Event{
				Type:       event.AgentQueryReceived,
				QueryAgent: "conductor",
				QueryText:  disambResult.Question,
			})
			c.mu.RLock()
			queryCh := c.conductorQueryCh
			c.mu.RUnlock()
			select {
			case queryCh <- query:
			case <-reqCtx.Done():
				return "", ErrRequestCancelled
			}
			select {
			case answer := <-answerCh:
				userRequest = userRequest + "\n\nQ: " + disambResult.Question + "\nA: " + answer
				c.Events.Emit(event.Event{
					Type:       event.AgentQueryAnswered,
					QueryAgent: "conductor",
					AnswerText: answer,
				})
			case <-reqCtx.Done():
				return "", ErrRequestCancelled
			}
		}
	}

	conductorKnowledge := ""
	for _, a := range c.FabricDef.Agents {
		if a.Name == "conductor" && a.SpecialKnowledgeFile != "" {
			skPath := filepath.Join(c.BaseDir, "agents", "conductor", "special_knowledge.md")
			if data, err := os.ReadFile(skPath); err == nil {
				conductorKnowledge = string(data)
			}
			break
		}
	}

	// Wait for prior background knowledge generation to avoid loading stale graphs.
	c.knowledgeWg.Wait()

	conductorStorage := c.StorageFactory("conductor")
	existingGraph, _ := knowledge.Load(ctx, conductorStorage)
	if existingGraph == nil {
		existingGraph = knowledge.NewGraph()
	}

	// Dedup: reuse existing node ID if an identical user-request already exists.
	nodeID := existingGraph.FindByTagAndSummary("user-request", userRequest)
	if nodeID == "" {
		nodeID = fmt.Sprintf("conductor/req-%s", requestID)
	}
	userReqNode := knowledge.ManifestNode{
		ID:         nodeID,
		Agent:      "conductor",
		Title:      truncateTitle(userRequest, 80),
		Summary:    userRequest,
		Tags:       []string{"user-request"},
		Confidence: 1.0,
		Source:     "user_provided",
		TTLDays:    90,
	}
	existingGraph.Merge(&knowledge.Manifest{Nodes: []knowledge.ManifestNode{userReqNode}}, requestID, "")
	knowledge.Save(reqCtx, conductorStorage, existingGraph)

	agentGraphs := make(map[string]*knowledge.Graph)
	for _, def := range c.FabricDef.Agents {
		if def.Name == "conductor" {
			continue
		}
		agentStorage := c.StorageFactory(def.Name)
		ag, _ := knowledge.LoadFromTier(ctx, agentStorage, runtime.TierAgent)
		if ag == nil {
			ag = knowledge.NewGraph()
		}
		agentGraphs[def.Name] = ag
	}

	if graphSummary := decomposeKnowledge(existingGraph, userRequest); graphSummary != "" {
		if conductorKnowledge != "" {
			conductorKnowledge += "\n\n"
		}
		conductorKnowledge += graphSummary
	}

	if artifactSummary := decomposeArtifacts(conductorStorage, userRequest); artifactSummary != "" {
		if conductorKnowledge != "" {
			conductorKnowledge += "\n\n"
		}
		conductorKnowledge += artifactSummary
	}

	if decisionSummary := decomposeDecisions(existingGraph, userRequest); decisionSummary != "" {
		if conductorKnowledge != "" {
			conductorKnowledge += "\n\n"
		}
		conductorKnowledge += decisionSummary
	}

	if userReqSummary := decomposeUserRequests(existingGraph, userRequest); userReqSummary != "" {
		if conductorKnowledge != "" {
			conductorKnowledge += "\n\n"
		}
		conductorKnowledge += userReqSummary
	}

	c.Events.Emit(event.Event{Type: event.DecomposeStart})

	var graph *taskgraph.TaskGraph
	var decomposeTokenUsage *message.TokenUsage

	result, err := Decompose(reqCtx, c.conductorDecomposeGenerate, c.FabricDef, userRequest, conductorKnowledge, c.Templates...)
	if err != nil {
		if reqCtx.Err() != nil {
			return "", ErrRequestCancelled
		}
		return "", fmt.Errorf("decompose: %w", err)
	}

	if !result.Actionable {
		screenEvt := event.Event{
			Type:          event.RequestScreened,
			ScreenMessage: result.Message,
		}
		if result.TokenUsage != nil {
			screenEvt.InputTokens = result.TokenUsage.InputTokens
			screenEvt.OutputTokens = result.TokenUsage.OutputTokens
			screenEvt.TotalCalls = 1
		}
		c.Events.Emit(screenEvt)
		c.Events.Emit(event.Event{
			Type:          event.RequestComplete,
			TotalDuration: time.Since(requestStart),
		})
		return "", nil
	}

	graph = result.Graph
	graph.RequestID = requestID
	graph.Name = result.Name
	decomposeTokenUsage = result.TokenUsage

	if c.Logger != nil {
		logText := "Decomposed user request into task graph"
		decomposeMsg := &message.Message{
			ID:         uuid.New().String(),
			RequestID:  requestID,
			From:       "conductor",
			To:         "conductor",
			Type:       message.TypeStatusUpdate,
			Parts:      []message.Part{message.TextPart{Text: logText}},
			Metadata:   map[string]string{"phase": "decompose", "task_count": fmt.Sprintf("%d", len(graph.Tasks))},
			TokenUsage: decomposeTokenUsage,
			Timestamp:  time.Now(),
		}
		c.Logger.Log(ctx, decomposeMsg)
	}

	decomposeEndEvt := event.Event{
		Type:  event.DecomposeEnd,
		Tasks: toTaskSummaries(graph),
	}
	if decomposeTokenUsage != nil {
		decomposeEndEvt.InputTokens = decomposeTokenUsage.InputTokens
		decomposeEndEvt.OutputTokens = decomposeTokenUsage.OutputTokens
		decomposeEndEvt.TotalCalls = 1
	}
	c.Events.Emit(decomposeEndEvt)

	slog.Info("task graph created", "request_id", requestID, "tasks", len(graph.Tasks))

	scheduler := &Scheduler{
		Comm:              c.Comm,
		Logger:            c.Logger,
		Storage:           conductorStorage,
		Meter:             c.Meter,
		StorageFactory:    c.StorageFactory,
		RequestID:         requestID,
		Events:            c.Events,
		Agents:            c.FabricDef.Agents,
		UserRequest:       userRequest,
		KnowledgeGraph:    existingGraph,
		AgentGraphs:       agentGraphs,
		KnowledgeGenerate: c.conductorGenerate,
		UserQueryCh:       c.conductorQueryCh,
		graphReplace:      make(chan *taskgraph.TaskGraph, 1),
		reqCancel:         reqCancel,
	}

	c.mu.Lock()
	c.activeScheduler = scheduler
	c.activeGraph = graph
	c.mu.Unlock()

	execErr := scheduler.Execute(reqCtx, graph)

	c.mu.Lock()
	c.activeScheduler = nil
	c.activeGraph = nil
	c.mu.Unlock()

	allCancelled := true
	for _, t := range graph.Tasks {
		if t.Status != taskgraph.StatusCancelled {
			allCancelled = false
			break
		}
	}
	if allCancelled && len(graph.Tasks) > 0 {
		return "", ErrRequestCancelled
	}

	if execErr != nil {
		return "", fmt.Errorf("execute: %w", execErr)
	}

	c.knowledgeWg.Add(1)
	go func() {
		defer c.knowledgeWg.Done()
		bgCtx := runtime.WithRequestID(c.backgroundCtx, requestID)
		manifest, _, knErr := knowledge.Generate(bgCtx, c.conductorGenerate, graph, existingGraph, graph.Name)
		if knErr != nil {
			slog.Warn("knowledge generation failed", "error", knErr)
			return
		}

		agentMaxNodes := make(map[string]int, len(c.FabricDef.Agents))
		for _, def := range c.FabricDef.Agents {
			if def.MaxKnowledgeNodes > 0 {
				agentMaxNodes[def.Name] = def.MaxKnowledgeNodes
			}
		}
		pruneOpts := knowledge.PruneOpts{}
		for _, max := range agentMaxNodes {
			if max > 0 {
				pruneOpts.MaxNodes = max
				break
			}
		}

		storageFor := func(name string) knowledge.StorageHandle {
			return c.StorageFactory(name)
		}
		if applyErr := knowledge.Apply(bgCtx, storageFor, conductorStorage, manifest, existingGraph, requestID, graph.Name, pruneOpts); applyErr != nil {
			slog.Warn("knowledge apply failed", "error", applyErr)
		}
		slog.Info("knowledge updated in background",
			"nodes", len(manifest.Nodes), "edges", len(manifest.Edges))
	}()

	if !c.SkipScratchCleanup {
		c.cleanAllScratch(ctx)
	}

	if err := c.writeArtifacts(requestID, graph); err != nil {
		slog.Warn("failed to write artifacts", "request_id", requestID, "error", err)
	}

	completeEvt := event.Event{
		Type:             event.RequestComplete,
		TotalDuration:    time.Since(requestStart),
		HasFailures:      graph.HasFailures(),
		FailureSummaries: graph.FailureSummaries(),
	}
	if c.Meter != nil {
		usage, _ := c.Meter.AggregateUsage(context.Background())
		completeEvt.InputTokens = usage.InputTokens
		completeEvt.OutputTokens = usage.OutputTokens
		completeEvt.CacheReadTokens = usage.CacheReadTokens
		completeEvt.TotalCalls = usage.TotalCalls
		completeEvt.ModelUsages = c.Meter.ModelUsage(context.Background())
	}
	c.Events.Emit(completeEvt)

	finalResult := c.collectResults(graph)
	scheduler.persistHitCounters(context.WithoutCancel(ctx))
	go c.StartIdleCuration(context.WithoutCancel(ctx))

	return finalResult, nil
}

func toTaskSummaries(graph *taskgraph.TaskGraph) []event.TaskSummary {
	summaries := make([]event.TaskSummary, len(graph.Tasks))
	for i, t := range graph.Tasks {
		summaries[i] = event.TaskSummary{
			ID:          t.ID,
			Agent:       t.Agent,
			Description: t.Description,
			DependsOn:   t.DependsOn,
			Status:      string(t.Status),
			LoopID:      t.LoopID,
		}
	}
	return summaries
}

func (c *Conductor) writeArtifacts(requestID string, graph *taskgraph.TaskGraph) error {
	artifactDir := filepath.Join(c.BaseDir, "shared", "artifacts", "_requests", requestID)
	if err := os.MkdirAll(artifactDir, 0755); err != nil {
		return fmt.Errorf("create artifact dir: %w", err)
	}

	var combined string
	for _, task := range graph.Tasks {
		if task.Result == "" {
			continue
		}
		content := fmt.Sprintf("# %s (%s)\n\n%s\n", task.ID, task.Agent, task.Result)
		taskFile := filepath.Join(artifactDir, task.ID+".md")
		if err := os.WriteFile(taskFile, []byte(content), 0644); err != nil {
			slog.Warn("failed to write task artifact", "task", task.ID, "error", err)
		}
		if combined != "" {
			combined += "\n\n---\n\n"
		}
		combined += content
	}

	if combined != "" {
		resultsFile := filepath.Join(artifactDir, "results.md")
		if err := os.WriteFile(resultsFile, []byte(combined), 0644); err != nil {
			return fmt.Errorf("write results.md: %w", err)
		}
	}

	slog.Info("artifacts written", "request_id", requestID, "dir", artifactDir)
	return nil
}

func (c *Conductor) collectResults(graph *taskgraph.TaskGraph) string {
	result := ""
	for _, task := range graph.Tasks {
		if task.Result != "" {
			if result != "" {
				result += "\n\n---\n\n"
			}
			result += fmt.Sprintf("**%s** (%s):\n%s", task.ID, task.Agent, task.Result)
		}
	}
	return result
}

func newRequestID() string {
	return time.Now().Format("20060102-150405") + "-" + uuid.New().String()[:8]
}

func (c *Conductor) cleanAllScratch(ctx context.Context) {
	for _, def := range c.FabricDef.Agents {
		if def.Name == "conductor" {
			continue
		}
		s := c.StorageFactory(def.Name)
		if err := s.Delete(ctx, runtime.TierScratch, ""); err != nil {
			slog.Debug("scratch cleanup failed", "agent", def.Name, "error", err)
		}
	}
}

// SetEvents replaces the event bus and eagerly creates the conductor query channel.
func (c *Conductor) SetEvents(bus event.Bus) {
	c.mu.Lock()
	c.Events = bus
	c.conductorQueryCh = make(chan *UserQuery, 1)
	c.mu.Unlock()
}

// AgentChatInfo returns agent status info sorted: conductor first, running, idle.
func (c *Conductor) AgentChatInfo() []AgentChatEntry {
	c.mu.RLock()
	graph := c.activeGraph
	c.mu.RUnlock()

	runningTasks := make(map[string]*taskgraph.TaskNode)
	if graph != nil {
		for _, t := range graph.Tasks {
			if t.Status == taskgraph.StatusRunning {
				runningTasks[t.Agent] = t
			}
		}
	}

	var entries []AgentChatEntry
	for _, def := range c.FabricDef.Agents {
		e := AgentChatEntry{
			Name:  def.Name,
			Model: def.Model,
		}
		if def.Name == "conductor" {
			e.Status = "conductor"
		} else if t, ok := runningTasks[def.Name]; ok {
			e.Status = "running"
			e.TaskID = t.ID
			e.TaskDesc = t.Description
		} else {
			e.Status = "idle"
		}
		entries = append(entries, e)
	}

	sortAgentEntries(entries)
	return entries
}

type AgentChatEntry struct {
	Name     string
	Model    string
	Status   string // "conductor", "running", "idle"
	TaskID   string
	TaskDesc string
}

func sortAgentEntries(entries []AgentChatEntry) {
	statusOrder := map[string]int{"conductor": 0, "running": 1, "idle": 2}
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && statusOrder[entries[j].Status] < statusOrder[entries[j-1].Status]; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

func (c *Conductor) RunningTaskForAgent(agentName string) string {
	c.mu.RLock()
	graph := c.activeGraph
	c.mu.RUnlock()

	if graph == nil {
		return ""
	}
	for _, t := range graph.Tasks {
		if t.Agent == agentName && t.Status == taskgraph.StatusRunning {
			return t.ID
		}
	}
	return ""
}

// TaskContextForAgent returns task context for an agent (prefers running, falls
// back to completed). For "conductor", returns a summary of all tasks.
func (c *Conductor) TaskContextForAgent(agentName string) string {
	c.mu.RLock()
	graph := c.activeGraph
	c.mu.RUnlock()

	if graph == nil {
		return ""
	}

	if agentName == "conductor" {
		return c.conductorTaskContext(graph)
	}

	var task *taskgraph.TaskNode
	for _, t := range graph.Tasks {
		if t.Agent != agentName {
			continue
		}
		if t.Status == taskgraph.StatusRunning {
			task = t
			break
		}
		if t.Status == taskgraph.StatusCompleted && task == nil {
			task = t
		}
	}
	if task == nil {
		return ""
	}

	var b strings.Builder
	b.WriteString("Task: ")
	b.WriteString(task.Description)

	if task.Status == taskgraph.StatusCompleted && task.Result != "" {
		b.WriteString("\n\nYour completed output:\n")
		result := task.Result
		if len(result) > 3000 {
			result = result[:3000] + "\n[... truncated]"
		}
		b.WriteString(result)
	}

	for _, depID := range task.DependsOn {
		dep := graph.Get(depID)
		if dep == nil || dep.Status != taskgraph.StatusCompleted {
			continue
		}
		b.WriteString("\n\n--- Context from ")
		b.WriteString(dep.ID)
		b.WriteString(" (")
		b.WriteString(dep.Agent)
		b.WriteString(") ---\n")
		result := dep.Result
		if len(result) > 2000 {
			result = result[:2000] + "\n[... truncated]"
		}
		b.WriteString(result)
	}

	return b.String()
}

func (c *Conductor) conductorTaskContext(graph *taskgraph.TaskGraph) string {
	if len(graph.Tasks) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Running tasks:\n")
	for _, t := range graph.Tasks {
		b.WriteString("- ")
		b.WriteString(t.ID)
		b.WriteString(" (")
		b.WriteString(t.Agent)
		b.WriteString("): ")
		b.WriteString(t.Description)
		b.WriteString(" [")
		b.WriteString(string(t.Status))
		if len(t.DependsOn) > 0 {
			b.WriteString(", depends on ")
			b.WriteString(strings.Join(t.DependsOn, ", "))
		}
		b.WriteString("]\n")
	}
	return b.String()
}

// AmendTask updates a task description. Promotes to full RestructureGraph when
// the amended task has running downstream dependents (returned bool is true).
func (c *Conductor) AmendTask(ctx context.Context, taskID, newDesc, chatContext string) (bool, error) {
	c.mu.RLock()
	sched := c.activeScheduler
	graph := c.activeGraph
	userRequest := c.activeUserRequest
	c.mu.RUnlock()

	if sched == nil || graph == nil {
		return false, fmt.Errorf("no active execution")
	}

	if graph.HasRunningDependents(taskID) {
		amendment := fmt.Sprintf("Task %s amended: %s\n\nUser feedback:\n%s", taskID, newDesc, chatContext)
		return true, c.RestructureGraph(ctx, userRequest, amendment)
	}

	return false, sched.AmendTask(taskID, newDesc, chatContext)
}

// RestructureGraph cancels running tasks, re-decomposes remaining work, and swaps the graph.
func (c *Conductor) RestructureGraph(ctx context.Context, userRequest, amendment string) error {
	c.mu.RLock()
	sched := c.activeScheduler
	graph := c.activeGraph
	c.mu.RUnlock()

	if sched == nil || graph == nil {
		return fmt.Errorf("no active execution")
	}

	sched.CancelAllRunningForRestructure()

	var completedContext string
	for _, t := range graph.Tasks {
		if t.Status == taskgraph.StatusCompleted && t.Result != "" {
			summary := t.Result
			if len(summary) > 200 {
				summary = summary[:200] + "..."
			}
			completedContext += fmt.Sprintf("- %s (%s): %s\n", t.ID, t.Agent, summary)
		}
	}

	modifiedRequest := fmt.Sprintf("Original request: %s\n\nAmendment: %s\n", userRequest, amendment)
	if completedContext != "" {
		modifiedRequest += fmt.Sprintf("\nAlready completed work (do not redo):\n%s\nDecompose only the remaining work needed to satisfy the amended request.\n", completedContext)
	}

	conductorKnowledge := ""
	for _, a := range c.FabricDef.Agents {
		if a.Name == "conductor" && a.SpecialKnowledgeFile != "" {
			skPath := filepath.Join(c.BaseDir, "agents", "conductor", "special_knowledge.md")
			if data, err := os.ReadFile(skPath); err == nil {
				conductorKnowledge = string(data)
			}
			break
		}
	}

	conductorStorage := c.StorageFactory("conductor")
	existingGraph, _ := knowledge.Load(ctx, conductorStorage)
	if existingGraph == nil {
		existingGraph = knowledge.NewGraph()
	}
	if graphSummary := decomposeKnowledge(existingGraph, modifiedRequest); graphSummary != "" {
		if conductorKnowledge != "" {
			conductorKnowledge += "\n\n"
		}
		conductorKnowledge += graphSummary
	}

	if artifactSummary := decomposeArtifacts(conductorStorage, modifiedRequest); artifactSummary != "" {
		if conductorKnowledge != "" {
			conductorKnowledge += "\n\n"
		}
		conductorKnowledge += artifactSummary
	}

	if decisionSummary := decomposeDecisions(existingGraph, modifiedRequest); decisionSummary != "" {
		if conductorKnowledge != "" {
			conductorKnowledge += "\n\n"
		}
		conductorKnowledge += decisionSummary
	}

	if userReqSummary := decomposeUserRequests(existingGraph, modifiedRequest); userReqSummary != "" {
		if conductorKnowledge != "" {
			conductorKnowledge += "\n\n"
		}
		conductorKnowledge += userReqSummary
	}

	result, err := Decompose(ctx, c.conductorDecomposeGenerate, c.FabricDef, modifiedRequest, conductorKnowledge, c.Templates...)
	if err != nil {
		return fmt.Errorf("re-decompose: %w", err)
	}

	newGraph := &taskgraph.TaskGraph{
		RequestID: graph.RequestID,
		Name:      graph.Name,
	}

	for _, t := range graph.Tasks {
		if t.Status == taskgraph.StatusCompleted {
			newGraph.Tasks = append(newGraph.Tasks, t)
		}
	}

	existingIDs := make(map[string]bool, len(newGraph.Tasks))
	for _, t := range newGraph.Tasks {
		existingIDs[t.ID] = true
	}
	idRemap := make(map[string]string)
	for _, t := range result.Graph.Tasks {
		newID := t.ID
		for existingIDs[newID] {
			newID = newID + "r"
		}
		if newID != t.ID {
			idRemap[t.ID] = newID
		}
		existingIDs[newID] = true
		newTask := &taskgraph.TaskNode{
			ID:          newID,
			Agent:       t.Agent,
			Description: t.Description,
			DependsOn:   t.DependsOn,
			LoopID:      t.LoopID,
			Status:      taskgraph.StatusPending,
		}
		for i, dep := range newTask.DependsOn {
			if remapped, ok := idRemap[dep]; ok {
				newTask.DependsOn[i] = remapped
			}
		}
		newGraph.Tasks = append(newGraph.Tasks, newTask)
	}

	newGraph.Loops = result.Graph.Loops

	c.mu.Lock()
	c.activeGraph = newGraph
	c.mu.Unlock()

	sched.ReplaceGraph(newGraph)

	c.Events.Emit(event.Event{
		Type:         event.TaskAmended,
		AmendedAgent: "conductor",
	})

	return nil
}

// GetUserQueryCh returns the active user query channel, or nil.
func (c *Conductor) GetUserQueryCh() <-chan *UserQuery {
	c.mu.RLock()
	sched := c.activeScheduler
	queryCh := c.conductorQueryCh
	c.mu.RUnlock()

	if sched != nil {
		return sched.UserQueryCh
	}
	return queryCh
}

type CurationResult struct {
	Agent       string
	NodesBefore int
	NodesAfter  int
	ColdMoved   int
	ColdPurged  int
	CostTokens  int64
}

// ForceCuration runs curation for all non-conductor agents regardless of threshold.
func (c *Conductor) ForceCuration(ctx context.Context) ([]CurationResult, error) {
	var results []CurationResult
	for _, def := range c.FabricDef.Agents {
		if def.Name == "conductor" {
			continue
		}

		agentStorage := c.StorageFactory(def.Name)
		agentGraph, err := knowledge.LoadFromTier(ctx, agentStorage, runtime.TierAgent)
		if err != nil || agentGraph == nil || len(agentGraph.Nodes) == 0 {
			continue
		}

		nodesBefore := len(agentGraph.Nodes)

		retentionDays := 1095
		if def.ColdRetentionDays > 0 {
			retentionDays = def.ColdRetentionDays
		}
		coldOpts := knowledge.ColdStorageOpts{RetentionDays: retentionDays}

		moved, purged, coldErr := knowledge.MoveToCold(ctx, agentStorage, agentGraph, coldOpts)
		if coldErr != nil {
			continue
		}

		nodesAfter := len(agentGraph.Nodes)
		var costTokens int64

		if c.conductorGenerate != nil {
			result, curateErr := knowledge.Curate(ctx, c.conductorGenerate, def.Name, agentGraph, knowledge.CurationOpts{
				Threshold: 1,
				ColdOpts:  coldOpts,
			})
			if curateErr == nil {
				if validateErr := knowledge.ValidateCurated(agentGraph, result.CuratedGraph); validateErr == nil {
					if swapErr := knowledge.SwapCurated(ctx, agentStorage, agentGraph, result.CuratedGraph, coldOpts); swapErr == nil {
						agentGraph = result.CuratedGraph
						nodesAfter = result.NodesOut
					}
				}
			}
		}

		knowledge.SaveToTier(ctx, agentStorage, runtime.TierAgent, agentGraph)

		results = append(results, CurationResult{
			Agent:       def.Name,
			NodesBefore: nodesBefore,
			NodesAfter:  nodesAfter,
			ColdMoved:   len(moved),
			ColdPurged:  purged,
			CostTokens:  costTokens,
		})
	}
	return results, nil
}

// StartIdleCuration waits 30s then curates knowledge for agents with 50+ nodes.
// Cancels cleanly if a new request arrives (via cancelIdleCuration).
func (c *Conductor) StartIdleCuration(ctx context.Context) {
	curationCtx, cancel := context.WithCancel(ctx)

	c.mu.Lock()
	// Cancel any previous curation context before replacing.
	if c.sleepCancel != nil {
		c.sleepCancel()
	}
	c.sleepCancel = cancel
	c.curationRunning = make(map[string]bool)
	c.mu.Unlock()

	defer cancel()

	select {
	case <-time.After(30 * time.Second):
	case <-curationCtx.Done():
		return
	}

	c.knowledgeWg.Wait()

	for _, def := range c.FabricDef.Agents {
		if def.Name == "conductor" {
			continue
		}

		select {
		case <-curationCtx.Done():
			return
		default:
		}

		c.curateAgent(curationCtx, def)
	}
}

func (c *Conductor) cancelIdleCuration() {
	c.mu.Lock()
	if c.sleepCancel != nil {
		c.sleepCancel()
		c.sleepCancel = nil
	}
	c.mu.Unlock()
}

func (c *Conductor) curateAgent(ctx context.Context, def runtime.AgentDefinition) {
	c.curationMu.Lock()
	if c.curationRunning[def.Name] {
		c.curationMu.Unlock()
		return
	}
	c.curationRunning[def.Name] = true
	c.curationMu.Unlock()

	defer func() {
		c.curationMu.Lock()
		delete(c.curationRunning, def.Name)
		c.curationMu.Unlock()
	}()

	agentStorage := c.StorageFactory(def.Name)

	agentGraph, err := knowledge.LoadFromTier(ctx, agentStorage, runtime.TierAgent)
	if err != nil || agentGraph == nil || len(agentGraph.Nodes) == 0 {
		return
	}

	threshold := 50
	if def.CurationThreshold > 0 {
		threshold = def.CurationThreshold
	}
	if len(agentGraph.Nodes) < threshold {
		return
	}

	retentionDays := 1095
	if def.ColdRetentionDays > 0 {
		retentionDays = def.ColdRetentionDays
	}

	coldOpts := knowledge.ColdStorageOpts{
		RetentionDays: retentionDays,
	}

	nodesIn := len(agentGraph.Nodes)

	c.Events.Emit(event.Event{
		Type:      event.AgentSleep,
		AgentName: def.Name,
	})

	moved, purged, coldErr := knowledge.MoveToCold(ctx, agentStorage, agentGraph, coldOpts)
	if coldErr != nil {
		slog.Warn("idle curation: cold storage failed", "agent", def.Name, "error", coldErr)
		return
	}

	if len(moved) > 0 || purged > 0 {
		slog.Info("idle curation: cold storage complete",
			"agent", def.Name, "moved", len(moved), "purged", purged)
	}

	nodesAfterCold := len(agentGraph.Nodes)
	nodesOut := nodesAfterCold
	if nodesAfterCold >= threshold && c.conductorGenerate != nil {
		c.Events.Emit(event.Event{
			Type:            event.CurationStarted,
			CurationAgent:   def.Name,
			CurationNodesIn: nodesAfterCold,
		})

		result, curateErr := knowledge.Curate(ctx, c.conductorGenerate, def.Name, agentGraph, knowledge.CurationOpts{
			Threshold: threshold,
			ColdOpts:  coldOpts,
		})
		if curateErr != nil {
			slog.Warn("idle curation: LLM curation failed", "agent", def.Name, "error", curateErr)
		} else if validateErr := knowledge.ValidateCurated(agentGraph, result.CuratedGraph); validateErr != nil {
			slog.Warn("idle curation: validation failed", "agent", def.Name, "error", validateErr)
		} else if swapErr := knowledge.SwapCurated(ctx, agentStorage, agentGraph, result.CuratedGraph, coldOpts); swapErr != nil {
			slog.Warn("idle curation: swap failed", "agent", def.Name, "error", swapErr)
		} else {
			agentGraph = result.CuratedGraph
			nodesOut = result.NodesOut
		}
	}

	if saveErr := knowledge.SaveToTier(ctx, agentStorage, runtime.TierAgent, agentGraph); saveErr != nil {
		slog.Warn("idle curation: save failed", "agent", def.Name, "error", saveErr)
	}

	c.Events.Emit(event.Event{
		Type:              event.CurationComplete,
		CurationAgent:     def.Name,
		CurationNodesIn:   nodesIn,
		CurationNodesOut:  nodesOut,
		ColdStorageMoved:  len(moved),
		ColdStoragePurged: purged,
	})

	c.Events.Emit(event.Event{
		Type:      event.AgentWake,
		AgentName: def.Name,
	})
}

// Shutdown tears down all agents. Safe to call multiple times.
func (c *Conductor) Shutdown(ctx context.Context) error {
	c.shutdownOnce.Do(func() {
		slog.Info("shutting down fabric")
		c.CancelExecution()
		c.cancelIdleCuration()
		if c.cancelBackground != nil {
			c.cancelBackground()
		}
		c.knowledgeWg.Wait()
		c.shutdownErr = c.Lifecycle.TeardownAll(ctx)
		if c.conductorGRPCServer != nil {
			c.conductorGRPCServer.Stop()
		}
	})
	return c.shutdownErr
}

// PauseExecution pauses the active scheduler. Returns false if none is active.
func (c *Conductor) PauseExecution() bool {
	c.mu.RLock()
	sched := c.activeScheduler
	c.mu.RUnlock()

	if sched == nil {
		return false
	}
	sched.Pause()
	c.Events.Emit(event.Event{Type: event.RequestPaused})
	return true
}

func (c *Conductor) ResumeExecution() bool {
	c.mu.RLock()
	sched := c.activeScheduler
	c.mu.RUnlock()

	if sched == nil {
		return false
	}
	sched.Resume()
	c.Events.Emit(event.Event{Type: event.RequestResumed})
	return true
}

// CancelExecution cancels the active request. Returns false if none is active.
func (c *Conductor) CancelExecution() bool {
	c.mu.RLock()
	sched := c.activeScheduler
	reqCancel := c.activeReqCancel
	c.mu.RUnlock()

	if reqCancel == nil {
		return false
	}

	if sched != nil {
		sched.Cancel()
	} else {
		reqCancel()
	}

	c.Events.Emit(event.Event{
		Type:         event.RequestCancelled,
		CancelReason: "user_cancel",
	})
	return true
}

func (c *Conductor) AgentStates(ctx context.Context) []AgentStatus {
	var states []AgentStatus
	for _, def := range c.FabricDef.Agents {
		usage, _ := c.Meter.Usage(ctx, def.Name)
		states = append(states, AgentStatus{
			Name:         def.Name,
			Model:        def.Model,
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			TotalCalls:   usage.TotalCalls,
		})
	}
	return states
}

func truncateTitle(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	cut := strings.LastIndex(s[:max], " ")
	if cut <= 0 {
		cut = max
	}
	return s[:cut] + "..."
}

type AgentStatus struct {
	Name         string
	Model        string
	InputTokens  int64
	OutputTokens int64
	TotalCalls   int64
}
