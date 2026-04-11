package conductor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/agent"
	"github.com/razvanmaftei/agentfab/internal/event"
	"github.com/razvanmaftei/agentfab/internal/knowledge"
	"github.com/razvanmaftei/agentfab/internal/llm"
	"github.com/razvanmaftei/agentfab/internal/message"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

// ChatMessage is a single turn in a chat conversation.
type ChatMessage struct {
	Role    string // "user" or "assistant"
	Content string
}

// ChatRequest is a user chat directed at a specific agent.
type ChatRequest struct {
	AgentName   string
	Message     string
	TaskContext string        // current task description, if any
	History     []ChatMessage // previous turns in this conversation
}

// ChatResponse holds the result of a chat interaction.
type ChatResponse struct {
	Response         string
	Amendment        *TaskAmendment
	Escalation       string // Non-empty when agent suggests escalating to coordinated work.
	TokenUsage       *message.TokenUsage
	SuggestedReplies []string // Optional quick-reply suggestions from the agent.
	Done             bool     // Agent signaled conversation is complete (CHAT_DONE marker).
}

// TaskAmendment describes a task change detected in the chat response.
type TaskAmendment struct {
	TaskID         string
	NewDescription string
	Structural     bool // requires graph restructuring
}

// Chat makes a direct LLM call using the target agent's model and system prompt.
func (c *Conductor) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if c.ModelFactory == nil {
		return nil, fmt.Errorf("no model factory configured")
	}

	var agentDef runtime.AgentDefinition
	found := false
	for _, a := range c.FabricDef.Agents {
		if a.Name == req.AgentName {
			agentDef = a
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("agent %q not found", req.AgentName)
	}

	systemPrompt := agent.BuildSystemPrompt(agentDef, "", c.FabricDef.Agents)

	// Enforce concise chat behavior.
	systemPrompt += "\n## Chat Mode Rules\n" +
		"You are in an interactive chat with the user. Follow these rules strictly:\n" +
		"- Be extremely concise. Answer in 1-3 sentences when possible.\n" +
		"- Do NOT produce plans, success criteria, milestones, or project management artifacts (EXCEPT for the AMEND_TASK/RESTRUCTURE protocol markers described below).\n" +
		"- Do NOT output complete documents, specifications, design files, or full markdown structures.\n" +
		"- Use available tools when they help answer accurately (for example, checking artifacts or files) before stating conclusions.\n" +
		"- Never claim you ran shell commands, read files, or inspected runtime state unless you actually did so via tool calls in this chat.\n" +
		"- Never output tool-use transcripts or pseudo-tool markers (for example: <attempt_tool_use>, <tool_use_result>).\n" +
		"- Do NOT enumerate options or lists unless the user explicitly asks for them.\n" +
		"- Do NOT use ASK_USER: markers — ask questions naturally as part of the conversation.\n" +
		"- Only elaborate when the user explicitly asks for more detail.\n" +
		"- Never produce file blocks (```file:path```) in chat.\n" +
		"- In chat, text-only responses ARE appropriate — the Output Format rules about file blocks do not apply here.\n" +
		"- When you need to modify files, make actual tool calls to the `shell` tool — do not describe or narrate tool usage in text.\n" +
		"- When the conversation reaches a natural end (question answered, user confirmed, nothing more to add), " +
		"include CHAT_DONE on its own line at the end of your response.\n"

	if knowledgeCtx := c.buildChatKnowledge(ctx, req.AgentName, req.Message); knowledgeCtx != "" {
		systemPrompt += "\n" + knowledgeCtx
	}

	if req.TaskContext != "" {
		if req.AgentName == "conductor" {
			// Conductor sees all tasks and can target amendments by task ID.
			systemPrompt += "\n## Current Execution Context\n" +
				req.TaskContext + "\n" +
				"If the user's message changes what an agent should do, include on its own line:\n" +
				"AMEND_TASK <taskID>: <new task description>\n" +
				"where <taskID> is the task ID from the list above (e.g. AMEND_TASK t2: Use Material Design 3 for all components).\n\n" +
				"If the change requires different agents or a fundamentally different structure, also include:\n" +
				"RESTRUCTURE: <reason>\n"
		} else {
			// Non-conductor agents can only amend their own task, never restructure.
			systemPrompt += "\n## Current Task Context\n" +
				"You are currently working on: " + req.TaskContext + "\n\n" +
				"If the user's message changes what you should do for this task, include on its own line:\n" +
				"AMEND_TASK: <new task description>\n" +
				"If the user asks for a change to your task scope or approach, emit AMEND_TASK with the updated description. " +
				"For small, specific fixes (e.g., \"rename this variable\", \"fix the CSS\"), call the `shell` tool to make the change directly — do not AMEND_TASK for trivial edits.\n" +
				"When making shell edits, write all changes to $SCRATCH_DIR (the working directory). $SHARED_DIR is read-only.\n"
		}
	} else {
		systemPrompt += "\n## Direct Assistance Mode\n" +
			"You are not currently assigned a task. Determine the right action based on the user's request:\n\n" +
			"**For questions, explanations, or inspecting existing work:**\n" +
			"Answer directly. Use the `shell` tool to read files if needed.\n\n" +
			"**For small, targeted fixes** (typo, rename, CSS tweak, single-file edit):\n" +
			"1. Call the `shell` tool to read the relevant files (e.g., from $SHARED_DIR/artifacts/)\n" +
			"2. Copy or recreate the file under $SCRATCH_DIR, then apply your changes there.\n" +
			"   Example: `mkdir -p $SCRATCH_DIR/src && cp $SHARED_DIR/artifacts/developer/src/App.tsx $SCRATCH_DIR/src/App.tsx && sed -i '' 's/old/new/' $SCRATCH_DIR/src/App.tsx`\n" +
			"   IMPORTANT: $SHARED_DIR is read-only. You MUST write all changes to $SCRATCH_DIR.\n" +
			"3. Call the `shell` tool to verify the change took effect\n" +
			"4. Report what you changed concisely\n" +
			"Changes in $SCRATCH_DIR are automatically persisted back to your artifacts.\n\n" +
			"**For new features, multi-file changes, design updates, or anything requiring multiple agents** " +
			"(e.g., \"add dark mode\", \"add a settings page\", \"switch to Material Design\", \"implement search\"):\n" +
			"Do NOT attempt these yourself. Instead, emit ESCALATE on its own line so the system can " +
			"orchestrate the right agents to do the work properly.\n\n" +
			"You MUST make actual tool calls — do not just describe what you would do.\n" +
			"Do NOT emit AMEND_TASK — you have no task to amend.\n"
	}

	systemPrompt += "\n## Cross-Agent Referrals\n" +
		"If the user's question is outside your expertise or another agent would know more, " +
		"suggest they talk to that agent. For example: " +
		"\"Another agent may know more about that — try chatting with them.\"\n" +
		"Only refer when genuinely helpful; answer directly when you can.\n\n" +
		"If the user's request requires coordinated multi-agent work (e.g., full app builds, " +
		"complex changes requiring multiple specialized agents), include on its own line:\n" +
		"ESCALATE: <reason>\n" +
		"This will switch to coordinated work mode where the conductor orchestrates multiple agents.\n"

	systemPrompt += "\n## Archived Knowledge Recovery\n" +
		"If you cannot fully answer a question and suspect the answer might be in your archived knowledge, " +
		"include on its own line:\n" +
		"COLD_STORAGE_SEARCH: <search query>\n" +
		"The system will search your archived knowledge and provide additional context.\n"

	systemPrompt += "\n## Suggested Replies\n" +
		"When your response naturally leads to a few likely follow-ups, " +
		"add 1-3 suggested replies at the end of your message, each on its own line:\n" +
		"SUGGEST_REPLY: <short reply text>\n" +
		"Only add these when helpful. Do NOT add them if the conversation is open-ended.\n"

	input := []*schema.Message{
		schema.SystemMessage(systemPrompt),
	}
	for _, h := range req.History {
		if h.Role == "user" {
			input = append(input, schema.UserMessage(h.Content))
		} else {
			input = append(input, schema.AssistantMessage(h.Content, nil))
		}
	}
	input = append(input, schema.UserMessage(req.Message))

	resp, toolCallCount, err := c.chatGenerateWithTools(ctx, agentDef, input)
	if err != nil {
		return nil, fmt.Errorf("generate: %w", err)
	}

	if toolCallCount > 0 {
		c.persistChatScratch(ctx, req.AgentName)
	}

	// Chat must stay conversational. Strip file artifacts even if the model
	// ignores chat-mode constraints and emits file blocks.
	response, hadFileBlocks := stripFileBlocks(resp.Content)
	// Some models still simulate tool traces in plain text; remove those to
	// avoid misleading users about actual execution.
	response, hadPseudoTools := stripPseudoToolTranscript(response)

	response, amendment := parseAmendmentMarkers(response, req.TaskContext != "")
	response, escalation := parseChatEscalation(response)
	response, coldQuery := parseColdStorageSearch(response)
	response, suggestions := parseSuggestedReplies(response)
	response, done := parseChatDone(response)

	// Cold storage recovery: if agent requested a cold storage search,
	// perform the lookup, inject context, and re-invoke the LLM.
	if coldQuery != "" && !done {
		coldCtx := c.buildColdStorageContext(ctx, req.AgentName, coldQuery)
		if coldCtx != "" {
			// Append cold storage results and re-generate.
			input = append(input, schema.AssistantMessage(resp.Content, nil))
			input = append(input, schema.UserMessage(coldCtx+"\n\nPlease answer the original question using this archived knowledge."))
			coldResp, _, coldErr := c.chatGenerateWithTools(ctx, agentDef, input)
			if coldErr == nil {
				response, _ = stripFileBlocks(coldResp.Content)
				response, _ = stripPseudoToolTranscript(response)
				response, amendment = parseAmendmentMarkers(response, req.TaskContext != "")
				response, escalation = parseChatEscalation(response)
				response, _ = parseColdStorageSearch(response) // strip any repeat markers
				response, suggestions = parseSuggestedReplies(response)
				response, done = parseChatDone(response)
			}
		}
	}
	if hadFileBlocks {
		note := "Chat note: file blocks are omitted here; changes only apply through the running task."
		if req.TaskContext == "" {
			note = "Chat note: file blocks were omitted. Use shell tools to write files directly."
		}
		if response == "" {
			response = note
		} else {
			response = strings.TrimSpace(response) + "\n\n" + note
		}
	}
	if hadPseudoTools {
		note := "Chat note: raw tool transcript markup was removed for readability."
		if response == "" {
			response = note
		} else {
			response = strings.TrimSpace(response) + "\n\n" + note
		}
	}
	if toolCallCount > 0 {
		note := fmt.Sprintf("[%d tool call(s) executed]", toolCallCount)
		if response == "" {
			response = note
		} else {
			response = strings.TrimSpace(response) + "\n\n" + note
		}
	}

	var usage *message.TokenUsage
	if resp.ResponseMeta != nil && resp.ResponseMeta.Usage != nil {
		u := resp.ResponseMeta.Usage
		usage = &message.TokenUsage{
			InputTokens:  int64(u.PromptTokens),
			OutputTokens: int64(u.CompletionTokens),
			TotalTokens:  int64(u.PromptTokens + u.CompletionTokens),
		}
	}

	return &ChatResponse{
		Response:         response,
		Amendment:        amendment,
		Escalation:       escalation,
		TokenUsage:       usage,
		SuggestedReplies: suggestions,
		Done:             done,
	}, nil
}

func (c *Conductor) chatGenerateWithTools(ctx context.Context, agentDef runtime.AgentDefinition, input []*schema.Message) (*schema.Message, int, error) {
	generate, toolExec, closeWorkspace, err := c.buildChatGenerator(ctx, agentDef)
	if err != nil {
		return nil, 0, err
	}
	if closeWorkspace != nil {
		defer closeWorkspace()
	}
	if toolExec == nil {
		resp, err := generate(ctx, input)
		return resp, 0, err
	}
	defer toolExec.Cleanup()

	const maxToolIterations = 20
	toolCallCount := 0
	convo := append([]*schema.Message{}, input...)
	for i := 0; i < maxToolIterations; i++ {
		resp, err := generate(ctx, convo)
		if err != nil {
			return nil, toolCallCount, err
		}
		if len(resp.ToolCalls) == 0 {
			// Compatibility fallback: some models emit tool-like XML markup in
			// plain text instead of structured tool calls.
			if pseudo := extractPseudoToolCalls(resp.Content); len(pseudo) > 0 {
				resp = &schema.Message{
					Role:      resp.Role,
					Content:   resp.Content,
					ToolCalls: pseudo,
				}
			} else {
				return resp, toolCallCount, nil
			}
		}

		convo = append(convo, resp)
		for _, tc := range resp.ToolCalls {
			toolCallCount++
			result, execErr := toolExec.Execute(ctx, tc)
			if execErr != nil {
				if result == "" {
					result = fmt.Sprintf("Error: %v", execErr)
				} else {
					result = result + "\n\nError: " + execErr.Error()
				}
			}
			convo = append(convo, schema.ToolMessage(result, tc.ID))
		}
	}

	convo = append(convo, schema.UserMessage(
		"Finalize now. Use prior tool results and do not call more tools. Return a concise direct answer.",
	))
	resp, err := generate(ctx, convo)
	if err != nil {
		return nil, toolCallCount, err
	}
	if len(resp.ToolCalls) > 0 {
		return &schema.Message{
			Role:    schema.Assistant,
			Content: "I couldn't finish the tool-assisted answer within limits. Ask me to continue in coordinated work mode.",
		}, toolCallCount, nil
	}
	return resp, toolCallCount, nil
}

func (c *Conductor) buildChatGenerator(
	ctx context.Context,
	agentDef runtime.AgentDefinition,
) (
	func(context.Context, []*schema.Message) (*schema.Message, error),
	*agent.ToolExecutor,
	func() error,
	error,
) {
	toolInfos := agent.BuildToolInfos(agentDef.Tools)
	liveTools := agent.LiveTools(agentDef.Tools)
	storage := c.StorageFactory(agentDef.Name)
	var workspace *runtime.Workspace
	var err error
	if len(liveTools) > 0 {
		workspace, err = runtime.OpenWorkspace(ctx, storage)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("materialize workspace: %w", err)
		}
	}

	var toolExec *agent.ToolExecutor
	if len(liveTools) > 0 {
		toolExec = &agent.ToolExecutor{
			Tools:           liveTools,
			TierPaths:       workspace.TierPaths(),
			AgentName:       agentDef.Name,
			MaxOutputTokens: llm.MaxOutputTokens(agentDef.Model),
			ContextLimit:    llm.ContextLimit(agentDef.Model),
			SyncWorkspace:   workspace.Sync,
		}
	}

	closeWorkspace := func() error {
		if workspace == nil {
			return nil
		}
		return workspace.Close()
	}

	generate := func(callCtx context.Context, input []*schema.Message) (*schema.Message, error) {
		m, err := c.ModelFactory(ctx, agentDef.Model)
		if err != nil {
			return nil, fmt.Errorf("create model: %w", err)
		}

		var baseModel model.BaseChatModel = m
		if len(toolInfos) > 0 {
			if tcm, ok := m.(model.ToolCallingChatModel); ok {
				bound, bindErr := tcm.WithTools(toolInfos)
				if bindErr != nil {
					return nil, fmt.Errorf("bind tools: %w", bindErr)
				}
				baseModel = bound
			} else {
				slog.Warn("model does not support ToolCallingChatModel; tools will not be bound",
					"agent", agentDef.Name, "model", agentDef.Model)
			}
		}

		metered := &llm.MeteredModel{
			Model:     baseModel,
			AgentName: agentDef.Name,
			ModelID:   agentDef.Model,
			Meter:     c.Meter,
			DebugLog:  c.DebugLog,
			Options:   llm.ProviderOptions(agentDef.Model, c.FabricDef.Providers),
		}
		return metered.Generate(callCtx, input)
	}

	return generate, toolExec, closeWorkspace, nil
}

func (c *Conductor) buildChatKnowledge(ctx context.Context, agentName, question string) string {
	storage := c.StorageFactory("conductor")
	graph, err := knowledge.Load(ctx, storage)
	if err != nil || graph == nil || len(graph.Nodes) == 0 {
		return ""
	}

	result := knowledge.Lookup(graph, agentName, question, knowledge.LookupOpts{
		MaxDepth: 2,
		MaxNodes: 10,
	})
	if len(result.Own) == 0 && len(result.Related) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Your Knowledge\n")
	b.WriteString("The following is knowledge you've accumulated from prior work. Use it to inform your answers.\n\n")

	if len(result.Own) > 0 {
		for _, n := range result.Own {
			b.WriteString(fmt.Sprintf("- **%s**: %s", n.Title, n.Summary))
			if n.RequestName != "" {
				b.WriteString(fmt.Sprintf(" *(from: %s)*", n.RequestName))
			}
			b.WriteByte('\n')
		}
	}

	if len(result.Related) > 0 {
		b.WriteString("\n### Related Knowledge (from other agents)\n")
		for _, rn := range result.Related {
			b.WriteString(fmt.Sprintf("- **%s** (by %s): %s", rn.Title, rn.Agent, rn.Summary))
			if len(rn.Relations) > 0 {
				b.WriteString(fmt.Sprintf(" [%s]", strings.Join(rn.Relations, ", ")))
			}
			b.WriteByte('\n')
		}
	}

	return b.String()
}

// parseAmendmentMarkers extracts AMEND_TASK and RESTRUCTURE markers from response text.
// Supports two formats:
//   - AMEND_TASK: <desc>           — used by regular agents (no task ID)
//   - AMEND_TASK <taskID>: <desc>  — used by conductor (task-targeted)
//
// Returns the cleaned response and any detected amendment.
func parseAmendmentMarkers(content string, hasTaskContext bool) (string, *TaskAmendment) {
	if !hasTaskContext {
		return content, nil
	}

	var amendment *TaskAmendment
	var cleanLines []string
	structural := false

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		upper := strings.ToUpper(trimmed)

		// Look for AMEND_TASK (handle bolding like **AMEND_TASK**)
		if idx := strings.Index(upper, "AMEND_TASK"); idx >= 0 {
			// Extract everything after the marker
			rest := trimmed[idx+len("AMEND_TASK"):]
			taskID, desc := parseAmendTaskRest(rest)
			if desc != "" {
				amendment = &TaskAmendment{
					TaskID:         taskID,
					NewDescription: desc,
				}
			}
			continue
		}

		if strings.Contains(upper, "RESTRUCTURE:") {
			structural = true
			continue
		}
		cleanLines = append(cleanLines, line)
	}

	if amendment != nil {
		amendment.Structural = structural
	}

	return strings.TrimSpace(strings.Join(cleanLines, "\n")), amendment
}

// parseAmendTaskRest parses the portion after "AMEND_TASK" to extract an
// optional task ID and the description.
//
// Formats:
//
//	": <desc>"         → taskID="", desc="<desc>"
//	" <taskID>: <desc>" → taskID="<taskID>", desc="<desc>"
func parseAmendTaskRest(rest string) (taskID, desc string) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", ""
	}

	// Format 1: ": <desc>"
	if strings.HasPrefix(rest, ":") {
		return "", strings.TrimSpace(rest[1:])
	}

	// Look for a colon separating ID from description.
	if idx := strings.Index(rest, ":"); idx > 0 {
		return strings.TrimSpace(rest[:idx]), strings.TrimSpace(rest[idx+1:])
	}

	// No colon found. Check if the first word looks like a task ID (e.g., t1, t2).
	// If it doesn't, assume the whole string is the description for the current agent.
	parts := strings.Fields(rest)
	if len(parts) > 0 {
		first := parts[0]
		// Heuristic: task IDs are usually short alphanumeric like t1, t2, task1.
		if len(first) < 10 && (strings.HasPrefix(first, "t") || strings.HasPrefix(first, "T")) {
			return first, strings.TrimSpace(rest[len(first):])
		}
	}

	// Fallback: entire rest is the description.
	return "", rest
}

// stripMarker removes lines matching "PREFIX: value" (case-insensitive) and
// returns the cleaned text and the last matched value.
func stripMarker(content, prefix string) (cleaned string, value string) {
	var cleanLines []string
	upper := strings.ToUpper(prefix)

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(trimmed), upper) {
			value = strings.TrimSpace(trimmed[len(prefix):])
			continue
		}
		cleanLines = append(cleanLines, line)
	}

	return strings.TrimSpace(strings.Join(cleanLines, "\n")), value
}

// stripMarkerAll removes lines matching "PREFIX: value" (case-insensitive) and
// returns the cleaned text and all matched values.
func stripMarkerAll(content, prefix string) (cleaned string, values []string) {
	var cleanLines []string
	upper := strings.ToUpper(prefix)

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(trimmed), upper) {
			v := strings.TrimSpace(trimmed[len(prefix):])
			if v != "" {
				values = append(values, v)
			}
			continue
		}
		cleanLines = append(cleanLines, line)
	}

	return strings.TrimSpace(strings.Join(cleanLines, "\n")), values
}

func parseSuggestedReplies(content string) (string, []string) {
	return stripMarkerAll(content, "SUGGEST_REPLY:")
}

func parseChatDone(content string) (string, bool) {
	done := false
	var cleanLines []string

	for _, line := range strings.Split(content, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "CHAT_DONE") {
			done = true
			continue
		}
		cleanLines = append(cleanLines, line)
	}

	return strings.TrimSpace(strings.Join(cleanLines, "\n")), done
}

func parseChatEscalation(content string) (string, string) {
	return stripMarker(content, "ESCALATE:")
}

// stripFileBlocks removes ```file:path ... ``` blocks from chat responses.
// Returns cleaned content and whether any file blocks were removed.
func stripFileBlocks(content string) (string, bool) {
	var cleanLines []string
	inFileBlock := false
	removed := false

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```file:") {
			inFileBlock = true
			removed = true
			continue
		}
		if inFileBlock {
			if strings.HasPrefix(trimmed, "```") {
				inFileBlock = false
			}
			continue
		}
		cleanLines = append(cleanLines, line)
	}

	return strings.TrimSpace(strings.Join(cleanLines, "\n")), removed
}

var (
	rePseudoToolNameCode  = regexp.MustCompile(`(?is)<tool_name>\s*([^<]+?)\s*</tool_name>\s*<tool_code>\s*(.*?)\s*</tool_code>`)
	reAttemptToolUse      = regexp.MustCompile(`(?is)<attempt_tool_use>\s*(.*?)\s*</attempt_tool_use>`)
	reStripAttemptToolUse = regexp.MustCompile(`(?is)<attempt_tool_use>\s*.*?\s*</attempt_tool_use>`)
	reStripToolUseResult  = regexp.MustCompile(`(?is)<tool_use_result>\s*.*?\s*</tool_use_result>`)
	reStripToolNameCode   = regexp.MustCompile(`(?is)<tool_name>\s*.*?\s*</tool_name>\s*<tool_code>\s*.*?\s*</tool_code>`)
	reStripToolNameTag    = regexp.MustCompile(`(?is)</?tool_name>`)
	reStripToolCodeTag    = regexp.MustCompile(`(?is)</?tool_code>`)
	reBlankLines          = regexp.MustCompile(`\n[ \t]*\n+`)
)

var pseudoToolStripPatterns = []*regexp.Regexp{
	reStripAttemptToolUse,
	reStripToolUseResult,
	reStripToolNameCode,
	reStripToolNameTag,
	reStripToolCodeTag,
}

// stripPseudoToolTranscript removes fabricated tool-execution markup from chat output.
func stripPseudoToolTranscript(content string) (string, bool) {
	original := strings.TrimSpace(content)
	clean := content
	for _, re := range pseudoToolStripPatterns {
		clean = re.ReplaceAllString(clean, "")
	}
	clean = collapseChatBlankLines(strings.TrimSpace(clean))
	return clean, clean != original
}

func collapseChatBlankLines(s string) string {
	return strings.TrimSpace(reBlankLines.ReplaceAllString(s, "\n"))
}

func parseColdStorageSearch(content string) (string, string) {
	return stripMarker(content, "COLD_STORAGE_SEARCH:")
}

func (c *Conductor) buildColdStorageContext(ctx context.Context, agentName, query string) string {
	agentStorage := c.StorageFactory(agentName)
	coldGraph, err := knowledge.LoadCold(ctx, agentStorage)
	if err != nil || coldGraph == nil || len(coldGraph.Nodes) == 0 {
		return ""
	}

	results := knowledge.LookupCold(coldGraph, query, 10)
	if len(results) == 0 {
		return ""
	}

	c.Events.Emit(event.Event{
		Type:      event.ColdStorageLookup,
		AgentName: agentName,
	})

	return knowledge.FormatColdResults(results)
}

func extractPseudoToolCalls(content string) []schema.ToolCall {
	var calls []schema.ToolCall

	for _, m := range rePseudoToolNameCode.FindAllStringSubmatch(content, -1) {
		if len(m) < 3 {
			continue
		}
		name := strings.TrimSpace(m[1])
		code := strings.TrimSpace(m[2])
		if name == "" || code == "" {
			continue
		}
		args := map[string]any{"command": code}
		if name != "shell" {
			args = map[string]any{"code": code}
		}
		argJSON, _ := json.Marshal(args)
		idx := len(calls)
		calls = append(calls, schema.ToolCall{
			Index: &idx,
			ID:    fmt.Sprintf("pseudo-tool-%d", idx+1),
			Type:  "function",
			Function: schema.FunctionCall{
				Name:      name,
				Arguments: string(argJSON),
			},
		})
	}

	for _, m := range reAttemptToolUse.FindAllStringSubmatch(content, -1) {
		if len(m) < 2 {
			continue
		}
		payload := strings.TrimSpace(m[1])
		if payload == "" {
			continue
		}

		var raw struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(payload), &raw); err != nil || strings.TrimSpace(raw.Name) == "" {
			continue
		}
		if raw.Arguments == nil {
			raw.Arguments = map[string]any{}
		}
		argJSON, _ := json.Marshal(raw.Arguments)
		idx := len(calls)
		calls = append(calls, schema.ToolCall{
			Index: &idx,
			ID:    fmt.Sprintf("pseudo-tool-%d", idx+1),
			Type:  "function",
			Function: schema.FunctionCall{
				Name:      strings.TrimSpace(raw.Name),
				Arguments: string(argJSON),
			},
		})
	}

	return calls
}

func (c *Conductor) persistChatScratch(ctx context.Context, agentName string) {
	storage := c.StorageFactory(agentName)
	files, err := storage.ListAll(ctx, runtime.TierScratch, "")
	if err != nil || len(files) == 0 {
		return
	}

	const maxFileSize = 1 << 20 // 1MB
	persisted := 0
	artifactDir := "artifacts/" + agentName

	for _, rel := range files {
		if rel == "" {
			continue
		}
		lower := strings.ToLower(filepath.ToSlash(rel))
		if strings.Contains(lower, "node_modules/") ||
			strings.Contains(lower, "/.git/") ||
			strings.Contains(lower, "dist/") ||
			strings.Contains(lower, "build/") ||
			strings.Contains(lower, ".next/") ||
			strings.Contains(lower, "__pycache__/") ||
			strings.Contains(lower, "vendor/") ||
			strings.Contains(lower, ".cache/") ||
			strings.Contains(lower, "coverage/") ||
			strings.Contains(lower, ".vite/") ||
			strings.Contains(lower, "_requests/") ||
			strings.Contains(lower, ".tool-results/") {
			continue
		}
		// Skip hidden files, logs, and sandbox temp files.
		base := filepath.Base(rel)
		if strings.HasPrefix(base, ".") {
			continue
		}

		data, readErr := storage.Read(ctx, runtime.TierScratch, rel)
		if readErr != nil || len(data) == 0 || len(data) > maxFileSize {
			continue
		}

		storagePath := artifactDir + "/" + filepath.ToSlash(rel)
		if err := storage.Write(ctx, runtime.TierShared, storagePath, data); err != nil {
			slog.Warn("failed to persist chat scratch file", "path", rel, "error", err)
		} else {
			persisted++
		}
	}

	if persisted > 0 {
		slog.Info("persisted chat scratch files to shared storage",
			"agent", agentName, "files", persisted)
	}
}
