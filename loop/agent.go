// Package loop implements the agent execution loop: it drives a Provider,
// executes tools through a Registry, and handles Plan/Act mode switching.
// Extracted from gline internal/agent/agent.go.
package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/liup215/cobot/compact"
	"github.com/liup215/cobot/prompts"
	"github.com/liup215/cobot/provider"
	"github.com/liup215/cobot/skills"
	"github.com/liup215/cobot/tools"
	"github.com/liup215/cobot/types"
)

// Provider-layer type aliases so the loop code (and its callers) can use
// the gline-style names without importing the provider package everywhere.
type (
	Provider              = provider.Provider
	StreamCallback        = provider.StreamCallback
	StreamCallbackAdapter = provider.StreamCallbackAdapter
	StreamChunk           = provider.StreamChunk
	ToolCall              = provider.ToolCall
	ToolDefinition        = provider.ToolDefinition
	MessageRequest        = provider.MessageRequest
	MessageResponse       = provider.MessageResponse
)

// noToolsUsedMsg is injected into the conversation when the assistant returns a
// response without calling any tools while tool-calling is required.
const noToolsUsedMsg = `[ERROR] You did not use a tool in your previous response.
When you have a task to perform, you MUST use one of the available tools.
Only calling attempt_completion, ask_followup_question, or plan_mode_respond can end the conversation.
Please review the task and call the appropriate tool(s).`

// Mode represents the operating mode of the Agent
type Mode string

const (
	// ModePlan is for exploration and planning without making changes
	ModePlan Mode = "plan"
	// ModeAct is for executing tasks and modifying files
	ModeAct Mode = "act"
)

// Agent is the core interface for the AI programming assistant
type Agent interface {
	// Run starts the Agent with a user prompt and no streaming callback.
	// It returns the structured outcome of the run; see RunWithCallback.
	Run(ctx context.Context, prompt string) (*RunResult, error)

	// RunWithCallback starts the Agent with a user prompt and a callback for streaming updates.
	// This is used for TUI mode to receive real-time updates.
	// It returns the structured outcome of the run; see BaseAgent.RunWithCallback.
	RunWithCallback(ctx context.Context, prompt string, callback StreamCallback) (*RunResult, error)

	// SetMode switches between Plan and Act modes
	SetMode(mode Mode) error

	// GetMode returns the current operating mode
	GetMode() Mode

	// SetSkills replaces the skills advertised in the system prompt
	// (progressive-disclosure skill list; nil clears the SKILLS section).
	SetSkills(list []skills.Skill)

	// Abort stops the current execution
	Abort()

	// IsRunning returns true if the Agent is currently processing
	IsRunning() bool

	// GetConversation returns the current conversation state
	GetConversation() *types.Conversation

	// Compact manually compacts the conversation history.
	Compact() bool

	// GetToolRegistry returns the tool registry
	GetToolRegistry() *tools.Registry
}

// Options contains configuration options for creating an Agent
type Options struct {
	// Provider is the LLM provider to use
	Provider Provider

	// ToolRegistry contains all available tools
	ToolRegistry *tools.Registry

	// Mode is the initial operating mode
	Mode Mode

	// AutoApprove enables automatic approval of tool calls (yolo mode)
	AutoApprove bool

	// MaxConsecutiveMistakes limits consecutive errors before stopping
	MaxConsecutiveMistakes int

	// CustomRules is extra text appended to the system prompt.
	// Ignored while SystemPromptOverride is set.
	CustomRules string

	// Skills is the set of Agent Skills advertised in the system prompt
	// (name + description + SKILL.md path only — progressive disclosure:
	// the model reads a SKILL.md on demand when a task matches). nil = no
	// SKILLS section. Replaced atomically by SetSkills between runs.
	Skills []skills.Skill

	// SystemPromptOverride completely replaces the default system prompt when
	// non-empty (role, sections, tool catalogue prose and CustomRules are all
	// dropped). The AVAILABLE TOOLS section — the tool names, descriptions and
	// input schemas of the current mode, which the model must be able to see
	// — is still rendered by the loop and appended to the override text (see
	// prompts.ApplySystemPromptOverride). Empty = default prompt.
	SystemPromptOverride string

	// AgentName is the agent identity announced in the system prompt's
	// role section ("You are <AgentName>, ..."). Empty = prompts.DefaultAgentName.
	// Ignored while SystemPromptOverride is set.
	AgentName string

	// Store is the optional persistent storage for conversation history
	Store Store

	// Guard is the optional ToolGuard approval gate consulted before every
	// tool execution (both the main-loop and pre-dispatch paths). See the
	// ToolGuard type for semantics.
	Guard ToolGuard

	// Title is the optional task title (used when creating a new task)
	Title string

	// MaxTokens is the maximum context tokens for the conversation (0 = default 128k)
	MaxTokens int

	// RetryDelays configures automatic stream-connection retries. Each entry
	// is the backoff wait before retry attempt i+1; a server Retry-After hint
	// longer than the entry wins. nil = DefaultRetryDelays (1s/2s/4s/8s);
	// empty non-nil slice disables retrying.
	RetryDelays []time.Duration

	// Compaction is the strategy applied at the token-budget trigger points
	// (the 60% pre-request budget check and the 80% auto-compact). nil =
	// compact.NewCombo over a compact.Summarize strategy (LLM summarization,
	// keep the last 6 rounds) and a compact.Trim fallback (gline watermark
	// trimming). Any summarization failure falls back to Trim.
	Compaction compact.Strategy

	// Summarizer is used by the default compaction strategy to condense old
	// rounds. nil = ProviderSummarizer over Options.Provider (one
	// non-streaming call). Ignored when Compaction is set.
	Summarizer compact.Summarizer

	// SummarizePrompt overrides the default structured-summary prompt
	// (compact.DefaultSummarizePrompt). Ignored when Compaction is set.
	SummarizePrompt string
}

// BaseAgent implements the Agent interface
type BaseAgent struct {
	provider     Provider
	toolRegistry *tools.Registry
	mode         Mode
	conversation *types.Conversation

	running                bool
	abort                  atomic.Bool
	autoApprove            bool
	maxConsecutiveMistakes int
	consecutiveMistakes    int
	customRules            string
	systemPromptOverride   string
	agentName              string

	skillsMu sync.RWMutex // guards skills (SetSkills vs systemPromptFor)
	skills   []skills.Skill

	guard      ToolGuard // optional approval gate for tool calls
	store      Store     // persistent storage
	taskID     string
	taskTitle  string
	workingDir string

	retryDelays []time.Duration // stream-connection retry schedule (nil = default)

	compaction compact.Strategy // conversation compaction strategy (never nil after New)

	// Stream pre-dispatch: tool calls start executing while the SSE stream is still ongoing.
	pendingToolCallsMu     sync.Mutex
	pendingToolCalls       []ToolCall
	preDispatchedResultsMu sync.Mutex
	preDispatchedResults   map[string]preDispatchedResult
	// preDispatchedIDs tracks call IDs that were pre-dispatched during the stream
	// so the main loop can avoid calling OnToolCallStart twice for the same call.
	preDispatchedIDsMu sync.Mutex
	preDispatchedIDs   map[string]bool
	// preDispatchWG tracks in-flight pre-dispatch goroutines so RunWithCallback
	// can await them before returning: no agent-owned goroutine outlives a run,
	// which is what makes context cancellation leak-free.
	preDispatchWG sync.WaitGroup
}

// preDispatchedResult holds the outcome of a tool call that was launched
// early, while the SSE stream was still in flight.
type preDispatchedResult struct {
	result string
	err    error
	// denied is set when the ToolGuard rejected the call during pre-dispatch;
	// result then holds the "[DENIED] <reason>" text.
	denied bool
}

// New creates a new Agent instance with the given options
func New(opts Options) (*BaseAgent, error) {
	if opts.Provider == nil {
		return nil, fmt.Errorf("provider is required")
	}

	if opts.ToolRegistry == nil {
		return nil, fmt.Errorf("tool registry is required")
	}

	mode := opts.Mode
	if mode == "" {
		mode = ModeAct
	}

	maxMistakes := opts.MaxConsecutiveMistakes
	if maxMistakes == 0 {
		maxMistakes = 3
	}

	agentName := opts.AgentName
	if agentName == "" {
		agentName = prompts.DefaultAgentName
	}

	conv := types.NewConversation()
	if opts.MaxTokens > 0 {
		conv.MaxTokens = opts.MaxTokens
	}

	// Default compaction: summarize the old rounds with the session's own
	// provider and fall back to the gline watermark trim when that is
	// impossible for any reason.
	compaction := opts.Compaction
	if compaction == nil {
		summarizer := opts.Summarizer
		if summarizer == nil {
			summarizer = NewProviderSummarizer(opts.Provider)
		}
		compaction = compact.NewCombo(
			&compact.Summarize{Summarizer: summarizer, Prompt: opts.SummarizePrompt},
			compact.NewTrim(),
		)
	}

	return &BaseAgent{
		provider:               opts.Provider,
		toolRegistry:           opts.ToolRegistry,
		mode:                   mode,
		conversation:           conv,
		autoApprove:            opts.AutoApprove,
		maxConsecutiveMistakes: maxMistakes,
		customRules:            opts.CustomRules,
		systemPromptOverride:   opts.SystemPromptOverride,
		agentName:              agentName,
		skills:                 opts.Skills,
		guard:                  opts.Guard,
		store:                  opts.Store,
		taskTitle:              opts.Title,
		retryDelays:            opts.RetryDelays,
		compaction:             compaction,
	}, nil
}

// Run starts the Agent with a user prompt and no streaming callback. It is
// RunWithCallback with a no-op callback; see that method for the semantics of
// the returned *RunResult and error.
func (a *BaseAgent) Run(ctx context.Context, prompt string) (*RunResult, error) {
	// Use the no-op adapter for non-streaming scenarios
	return a.RunWithCallback(ctx, prompt, &StreamCallbackAdapter{})
}

// RunWithCallback starts the Agent with a user prompt and a callback for
// streaming updates.
//
// It returns the structured outcome of the run (RunResult) together with the
// terminal error:
//
//   - the result is non-nil for every run except a rejected "agent is
//     already running" call, so hosts can report partial progress (tokens
//     spent, turns served, best-effort final text) even when the run fails
//     or is canceled;
//   - the error is nil when the run ended normally: a terminal tool call
//     (attempt_completion / plan_mode_respond) or a host-called Abort();
//   - a canceled or expired ctx surfaces as an error wrapping
//     context.Canceled / context.DeadlineExceeded, so errors.Is works;
//   - every other failure (stream error, consecutive-mistakes limit)
//     surfaces verbatim. RunResult.Err always mirrors the error.
//
// Cancellation semantics: when ctx is done the loop stops reading the
// provider stream, stops executing tools (including in-flight pre-dispatch
// goroutines, which are awaited before returning) and stops issuing new LLM
// requests; the interrupted turn is not recorded in the conversation.
// Abort() remains the separate, non-error cancellation channel.
func (a *BaseAgent) RunWithCallback(ctx context.Context, prompt string, callback StreamCallback) (*RunResult, error) {
	if a.running {
		return nil, fmt.Errorf("agent is already running")
	}

	a.running = true
	a.abort.Store(false)
	// Ensure running is always reset, even on panic. In-flight pre-dispatch
	// goroutines are awaited first so no agent-owned goroutine outlives the
	// run (this is what makes context cancellation leak-free).
	defer func() {
		a.preDispatchWG.Wait()
		a.running = false
		// Also clear the abort flag so subsequent runs start fresh.
		a.abort.Store(false)
	}()

	// A context that is already canceled ends the run before anything is
	// recorded: no user message, no task, no LLM request.
	if cerr := canceledError(ctx); cerr != nil {
		return &RunResult{Err: cerr}, cerr
	}

	// Per-run accounting: FinalText / usage / turns are scoped to this run,
	// even though the agent and its conversation are reused across runs.
	run := newRunStats()

	// Each new user turn must reopen the conversation loop.
	a.conversation.MarkIncomplete()

	// Add user message to conversation
	a.conversation.AddMessage(types.Message{
		Role:    types.RoleUser,
		Content: prompt,
	})

	// --- Storage: create task and save user message ---
	if a.store != nil {
		if a.taskID == "" {
			providerName := a.provider.GetProviderName()
			model := a.provider.GetModel()
			id, err := a.store.CreateTask(a.taskTitle, prompt, string(a.mode), providerName, model, a.workingDir)
			if err != nil {
				logWarnf("Failed to create task record: %v", err)
			} else {
				a.taskID = id
				callback.OnTaskCreated(id)
			}
		}
		if a.taskID != "" {
			if lastMsg := a.conversation.GetLastMessage(); lastMsg != nil {
				if err := a.store.SaveMessage(a.taskID, *lastMsg); err != nil {
					logWarnf("Failed to save user message: %v", err)
				}
			}
		}
	}

	// Main conversation loop
	for !a.abort.Load() {
		if res, err, canceled := a.canceledRunResult(ctx, run, callback); canceled {
			return res, err
		}

		// Get available tools for current mode
		availableTools := a.toolRegistry.GetForMode(string(a.mode))

		// Build system prompt with tool descriptions from registry (includes
		// host-registered native tools). The AVAILABLE TOOLS section is always
		// rendered from the registry, even in SystemPromptOverride mode.
		toolDescs := make([]prompts.ToolDescription, 0, len(availableTools))
		for _, tool := range availableTools {
			toolDescs = append(toolDescs, prompts.ToolDescription{
				Name:        tool.Name(),
				Description: tool.Description(),
				InputSchema: string(tool.InputSchema()),
			})
		}
		if a.mode == ModePlan {
			toolDescs = filterPlanModeTools(toolDescs)
		}
		systemPrompt := a.systemPromptFor(toolDescs)

		// Trim conversation if it exceeds token budget before sending.
		if a.conversation.IsTokenAboveThreshold(100) {
			callback.OnCompactionStart(fmt.Sprintf("trim: conversation exceeds max tokens (%d)", a.conversation.MaxTokens))
			a.conversation.TrimToMaxTokens()
			callback.OnCompactionEnd(fmt.Sprintf("trim complete: %d tokens", a.conversation.GetTotalTokens()))
		}

		// Auto-compact if tokens exceed 60% of the max context.
		// Earlier compaction prevents the token budget from growing too
		// large and keeps API latency stable across long conversations.
		a.AutoCompact()

		// Determine whether the assistant still has pending work.
		// We require tools when:
		//   - there is at least one non-completion/non-question tool available
		//   - the conversation is not already marked complete
		needsTool := a.needsTool(availableTools)

		// Pre-request token budget check. Ensure system prompt + messages +
		// tool descriptions fit inside the context window with room for the
		// response. Trigger compaction if we are close to the limit.
		a.enforceTokenBudget(ctx, systemPrompt, availableTools, callback)

		// Create LLM request
		req := &MessageRequest{
			Messages:     a.conversation.GetMessages(),
			Tools:        convertTools(availableTools),
			SystemPrompt: systemPrompt,
		}
		// Use "auto" instead of "required" for reasoning models (DeepSeek/Kimi).
		// "required" can cause confused-stop when the model sees a complex system
		// prompt, because it does not know which specific tool is required.
		// "auto" lets the model choose based on context; if it wrongly chooses
		// not to call, the noToolsUsedMsg fallback below corrects it.
		if needsTool {
			req.ToolChoice = provider.ToolChoiceAuto
		}
		logInfof("RunWithCallback: availableTools=%d, needsTool=%v, toolChoice=%s", len(availableTools), needsTool, req.ToolChoice)

		// Serialize available tool names for diagnostic storage. We attach this
		// to the assistant message record so users can verify whether the
		// request actually contained tools when debugging empty tool_calls.
		var availableToolsJSON json.RawMessage
		if len(availableTools) > 0 {
			names := make([]string, 0, len(availableTools))
			for _, t := range availableTools {
				names = append(names, t.Name())
			}
			b, err := json.Marshal(names)
			if err != nil {
				logWarnf("Failed to marshal available tools: %v", err)
			} else {
				availableToolsJSON = b
			}
		}

		// Use streaming API
		streamChan, err := a.createStreamWithRetry(ctx, req, callback)
		if err != nil {
			// A canceled context is not a provider failure: it ends the run
			// immediately instead of burning the correction budget.
			if res, cerr, canceled := a.canceledRunResult(ctx, run, callback); canceled {
				return res, cerr
			}
			a.consecutiveMistakes++
			callback.OnError(err)
			if a.consecutiveMistakes >= a.maxConsecutiveMistakes {
				runErr := fmt.Errorf("max consecutive mistakes reached: %w", err)
				return a.buildRunResult(run, runErr), runErr
			}
			continue
		}

		// NOTE: consecutiveMistakes is reset on successful tool execution
		// (executeToolCalls), not here, so that a model that keeps refusing
		// to call tools exhausts the correction budget (FR-L2).

		// Notify callback that a new stream is starting
		callback.OnStreamStart()

		// Process the stream
		if err := a.processStream(ctx, streamChan, callback, run); err != nil {
			// Cancellation wins over a mid-stream error, so an interrupted run
			// always surfaces the wrapped context error to the caller.
			if res, cerr, canceled := a.canceledRunResult(ctx, run, callback); canceled {
				return res, cerr
			}
			a.failTask(err.Error())
			callback.OnError(err)
			return a.buildRunResult(run, err), err
		}
		run.turns++

		// Save assistant message to storage, attaching the list of tools that
		// were available to the assistant so users can diagnose why tool_calls
		// may be empty.
		if a.store != nil && a.taskID != "" {
			if msgs := a.conversation.GetMessages(); len(msgs) > 0 {
				lastMsg := msgs[len(msgs)-1]
				if lastMsg.Role == types.RoleAssistant {
					lastMsg.AvailableTools = availableToolsJSON
					lastMsg.ToolChoice = string(req.ToolChoice)
					if err := a.store.SaveMessage(a.taskID, lastMsg); err != nil {
						logWarnf("Failed to save assistant message: %v", err)
					}
				}
			}
		}

		// Execute any tool calls from the last assistant message
		messages := a.conversation.GetMessages()
		if len(messages) > 0 {
			lastMsg := messages[len(messages)-1]
			if lastMsg.Role == types.RoleAssistant && len(lastMsg.ToolCalls) > 0 {
				a.executeToolCalls(ctx, lastMsg, callback)
			}
		}

		// Tool execution runs with the run's context: a canceled context ends
		// the run here instead of feeding more work to the model.
		if res, cerr, canceled := a.canceledRunResult(ctx, run, callback); canceled {
			return res, cerr
		}

		// If the conversation is not complete but the last assistant message has
		// no tool calls while tools are still needed, inject the no-tools-used
		// reminder so the next loop iteration forces the model to call a tool.
		if !a.conversation.IsComplete() && !a.abort.Load() {
			messages := a.conversation.GetMessages()
			if len(messages) > 0 {
				lastMsg := messages[len(messages)-1]
				if lastMsg.Role == types.RoleAssistant && len(lastMsg.ToolCalls) == 0 && needsTool {
					a.consecutiveMistakes++
					if a.consecutiveMistakes >= a.maxConsecutiveMistakes {
						runErr := fmt.Errorf("max consecutive mistakes reached: model did not use a tool in %d consecutive turns", a.consecutiveMistakes)
						return a.buildRunResult(run, runErr), runErr
					}
					a.conversation.AddMessage(types.Message{
						Role:    types.RoleUser,
						Content: noToolsUsedMsg,
					})
					continue // go to next loop iteration instead of falling through
				}
			}
		}

		// Check if conversation is complete
		if a.conversation.IsComplete() || a.abort.Load() {
			if a.store != nil && a.taskID != "" {
				status := "completed"
				if a.abort.Load() {
					status = "failed"
				}
				if status == "completed" {
					if err := a.store.CompleteTask(a.taskID); err != nil {
						logWarnf("Failed to mark task as completed: %v", err)
					}
				} else {
					if err := a.store.FailTask(a.taskID, "aborted"); err != nil {
						logWarnf("Failed to mark task as failed: %v", err)
					}
				}
			}
			break
		}
	}

	callback.OnComplete()
	return a.buildRunResult(run, nil), nil
}

// systemPromptFor builds the system prompt for the current mode and tool set.
//
// Default path: prompts.GetSystemPrompt (role, section catalogue, tool
// catalogue, custom rules). With Options.SystemPromptOverride set, the host
// text replaces the default prompt entirely and the loop appends only the
// AVAILABLE TOOLS section (prompts.ApplySystemPromptOverride) — without it
// the model could not see the tools it is expected to call.
//
// In both paths the SKILLS section (when any skill is advertised) is
// appended last: skills sit closest to the conversation so the model hits
// the load-on-demand instruction right before the user's message.
func (a *BaseAgent) systemPromptFor(toolDescs []prompts.ToolDescription) string {
	var prompt string
	if a.systemPromptOverride != "" {
		prompt = prompts.ApplySystemPromptOverride(a.systemPromptOverride, toolDescs)
	} else {
		prompt = prompts.GetSystemPrompt(string(a.mode), toolDescs, a.customRules, a.agentName)
	}
	return prompt + a.skillsSection()
}

// skillsSection renders the SKILLS section for the currently advertised
// skills ("" when none), reading the list under the same lock SetSkills
// writes so a mid-run swap can never race the prompt build.
func (a *BaseAgent) skillsSection() string {
	a.skillsMu.RLock()
	defer a.skillsMu.RUnlock()
	return skills.Section(a.skills)
}

// SetSkills replaces the advertised skill set. Safe to call between runs
// (e.g. after re-discovering the skill directories); the new section takes
// effect on the next system prompt build.
func (a *BaseAgent) SetSkills(list []skills.Skill) {
	a.skillsMu.Lock()
	a.skills = list
	a.skillsMu.Unlock()
}

// ResetTask clears the current task ID so the next RunWithCallback creates a new task.
// This is called by /newtask slash command to start a fresh conversation.
func (a *BaseAgent) ResetTask() {
	a.taskID = ""
}

// SetTaskTitle sets the title for the next task to be created.
func (a *BaseAgent) SetTaskTitle(title string) {
	a.taskTitle = title
}

// Compact manually compacts the conversation with the configured compaction
// strategy (summarize + trim fallback by default). Returns true if
// compaction actually occurred (fewer messages or fewer tokens afterwards).
func (a *BaseAgent) Compact() bool {
	beforeMsgs := a.conversation.MessageCount()
	beforeTokens := a.conversation.GetTotalTokens()
	a.runCompaction(context.Background(), a.conversation, nil)
	afterMsgs := a.conversation.MessageCount()
	compacted := afterMsgs < beforeMsgs || a.conversation.GetTotalTokens() < beforeTokens
	if compacted {
		logInfof("Compacted conversation: %d -> %d messages, %d tokens", beforeMsgs, afterMsgs, a.conversation.GetTotalTokens())
	}
	return compacted
}

// runCompaction applies the compaction strategy to conv. A strategy error is
// logged and swallowed: compaction is an optimization, never a reason to fail
// the run (the pre-send hard trim stays the safety net).
// runCompaction applies the configured strategy. A strategy error is logged
// and swallowed: compaction is an optimization, never a reason to fail the
// run (the pre-send hard trim stays the safety net). When the result is
// anchorable the typed CompactionAware extension receives it — the durable
// persistence hook (summary + anchor) that replaces guessing the shape from
// message diffs.
func (a *BaseAgent) runCompaction(ctx context.Context, conv *types.Conversation, callback provider.StreamCallback) compact.Result {
	if a.compaction == nil {
		// Zero-value agent (constructed outside New): legacy gline behavior.
		conv.AutoCompact()
		return compact.Result{}
	}
	res, err := a.compaction.Compact(ctx, conv)
	if err != nil {
		logWarnf("Compaction strategy %T failed: %v", a.compaction, err)
		return compact.Result{}
	}
	if res.Anchorable {
		if ca, ok := callback.(provider.CompactionAware); ok {
			ca.OnCompactionApplied(res)
		}
	}
	return res
}
func (a *BaseAgent) GetCompaction() compact.Strategy {
	return a.compaction
}

// needsTool determines whether the assistant should be forced to call at
// least one tool.
//
// Returns false when the only available tools are conversation-ending
// tools (attempt_completion, ask_followup_question, plan_mode_respond) or
// when there are no tools at all.
func (a *BaseAgent) needsTool(toolsList []tools.Tool) bool {
	for _, t := range toolsList {
		name := t.Name()
		if name != types.ToolAttemptCompletion.String() &&
			name != types.ToolAskFollowupQuestion.String() &&
			name != types.ToolPlanModeRespond.String() {
			return true
		}
	}
	return false
}

// AutoCompact checks whether current token usage exceeds 80% of the
// max context window and, if so, triggers a compaction.
func (a *BaseAgent) AutoCompact() {
	if a.conversation.IsTokenAboveThreshold(80) {
		a.Compact()
	}
}

// SetMode switches between Plan and Act modes
func (a *BaseAgent) SetMode(mode Mode) error {
	if mode != ModePlan && mode != ModeAct {
		return fmt.Errorf("invalid mode: %s", mode)
	}

	a.mode = mode
	return nil
}

// GetMode returns the current operating mode
func (a *BaseAgent) GetMode() Mode {
	return a.mode
}

// Abort stops the current execution at the next checkpoint: the run ends
// after the in-flight stream has been consumed and any in-flight tool has
// finished, and RunWithCallback reports a nil error (an orderly stop, not a
// failure). Abort does not force-unblock an in-flight provider read — to
// interrupt a hung stream or a hung tool immediately, cancel the run's
// context, which takes precedence over a pending abort flag.
func (a *BaseAgent) Abort() {
	a.abort.Store(true)
}

// IsRunning returns true if the Agent is currently processing
func (a *BaseAgent) IsRunning() bool {
	return a.running
}

// GetConversation returns the current conversation state
func (a *BaseAgent) GetConversation() *types.Conversation {
	return a.conversation
}

// GetProvider returns the LLM provider
func (a *BaseAgent) GetProvider() Provider {
	return a.provider
}

// GetToolRegistry returns the tool registry
func (a *BaseAgent) GetToolRegistry() *tools.Registry {
	return a.toolRegistry
}

// GetStore returns the persistent storage (may be nil).
func (a *BaseAgent) GetStore() Store {
	return a.store
}

// SetStore sets the persistent storage.
func (a *BaseAgent) SetStore(s Store) {
	a.store = s
}

// GetTaskID returns the current task ID.
func (a *BaseAgent) GetTaskID() string {
	return a.taskID
}

// SetTaskID sets the current task ID (used when resuming a task).
func (a *BaseAgent) SetTaskID(id string) {
	a.taskID = id
}

// GetWorkingDir returns the current working directory.
func (a *BaseAgent) GetWorkingDir() string {
	return a.workingDir
}

// SetWorkingDir sets the working directory.
func (a *BaseAgent) SetWorkingDir(dir string) {
	a.workingDir = dir
}

// enforceTokenBudget checks whether the upcoming request would exceed the
// conversation budget and, if so, triggers compaction. It estimates tokens
// for the system prompt and the JSON-serialized tool descriptions because
// those are also sent to the provider.
func (a *BaseAgent) enforceTokenBudget(ctx context.Context, systemPrompt string, availableTools []tools.Tool, callback StreamCallback) {
	conv := a.conversation
	if conv.MaxTokens <= 0 {
		return
	}

	budget := conv.MaxTokens
	if conv.ResponseBuffer > 0 {
		budget -= conv.ResponseBuffer
	}
	if budget <= 0 {
		budget = conv.MaxTokens
	}

	// Estimate current conversation tokens.
	total := conv.GetTotalTokens()

	// Add system prompt tokens.
	total += types.Estimator(systemPrompt)

	// Add approximate tool description tokens.
	if len(availableTools) > 0 {
		toolJSON, err := json.Marshal(availableTools)
		if err == nil {
			total += types.Estimator(string(toolJSON))
		}
	}

	if total <= budget*6/10 {
		return
	}

	reason := fmt.Sprintf("token budget %d/%d (60%% threshold)", total, budget)
	logInfof("Token budget at %d/%d; triggering compaction", total, conv.MaxTokens)
	if callback != nil {
		callback.OnCompactionStart(reason)
	}
	a.runCompaction(ctx, conv, callback)

	if conv.GetTotalTokens() > budget*8/10 {
		conv.TrimToMaxTokens()
	}
	if callback != nil {
		callback.OnCompactionEnd(fmt.Sprintf("compaction complete: %d tokens remaining", conv.GetTotalTokens()))
	}
}
