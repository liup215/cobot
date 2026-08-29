// Package cobot is a pure-Go, zero-dependency agent SDK: assemble a
// Config, hand it a task, and the agent runs the LLM tool loop to
// completion ("attempt_completion" protocol).
//
// The quick path is a single Config and NewAgent:
//
//	agent, err := cobot.NewAgent(cobot.Config{
//		APIKey:      "sk-...",
//		Model:       "gpt-4o",
//		AutoApprove: true,
//	})
//	err = agent.Run(ctx, "fix the login bug")
//
// Everything is optional and has a sensible default: the tool set defaults
// to the built-in registry (read/write/replace/search/execute/list files
// plus the interaction tools), the mode defaults to act, and connection
// retries use exponential backoff. Long tasks are compacted automatically:
// past 60% of the context budget the oldest rounds are summarized into a
// structured recap and any summarization failure falls back to watermark
// trimming. Advanced use cases — custom providers, custom tools, approval
// gates, persistence, streaming UIs — inject the corresponding component via
// Config; the underlying packages (loop, provider, tools, compact, types,
// prompts) remain fully accessible for advanced composition.
package cobot

import (
	"fmt"
	"time"

	"github.com/liup215/cobot/loop"
	"github.com/liup215/cobot/provider"
	"github.com/liup215/cobot/tools"
)

// Agent is the runnable agent. It is an alias of loop.BaseAgent, so all
// lifecycle methods (Run, RunWithCallback, SetMode, Abort, ...) are
// available directly.
type Agent = loop.BaseAgent

// Config is the one-stop construction description for an Agent.
//
// Two styles mix freely:
//
//   - Declarative: set APIKey (+ optional BaseURL/Model) and let NewAgent
//     build the OpenAI-compatible provider and default tool registry.
//   - Injective: set Provider and/or Tools yourself for full control
//     (custom backends, curated tool subsets).
//
// All fields are optional except one of APIKey/Provider; omitted behavior
// falls back to sensible defaults documented per field.
type Config struct {
	// ── LLM backend ──────────────────────────────────────────────

	// APIKey authenticates with the OpenAI-compatible endpoint
	// (official OpenAI, Volcengine Ark, DeepSeek, Kimi, ...). Required
	// unless Provider is injected directly.
	APIKey string

	// BaseURL overrides the endpoint (e.g. "https://ark.cn-beijing.volces.com/api/v3").
	// Empty = official OpenAI API.
	BaseURL string

	// Model is the model name sent to the provider. Empty = provider
	// default ("gpt-4").
	Model string

	// Provider injects a fully custom backend, bypassing APIKey/BaseURL.
	// Use this for non-OpenAI protocols or test doubles.
	Provider provider.Provider

	// ── Tools ────────────────────────────────────────────────────

	// Tools is the tool registry made available to the model. nil =
	// InitDefaultRegistry() (the built-in tool set). To extend the
	// defaults, start from InitDefaultRegistry() and Register extras.
	Tools *tools.Registry

	// ── Behavior ─────────────────────────────────────────────────

	// Mode is the initial operating mode: ModeAct (default) executes
	// changes; ModePlan explores and plans without side effects.
	Mode Mode

	// AutoApprove skips per-tool approval gates (yolo mode). When false
	// and no Guard is configured, side-effect tools are auto-approved as
	// well; a Guard is the recommended way to control execution.
	AutoApprove bool

	// MaxConsecutiveMistakes limits how many consecutive turns the model
	// may fail to use a tool before the run aborts. 0 = default (3).
	MaxConsecutiveMistakes int

	// CustomRules is extra text appended to the system prompt.
	CustomRules string

	// AgentName is the agent identity announced in the system prompt's
	// role section ("You are <AgentName>, a highly skilled software
	// engineer..."). Empty = "cobot".
	AgentName string

	// MaxTokens overrides the conversation context budget (0 = 128k).
	MaxTokens int

	// RetryDelays configures automatic stream-connection retries (backoff
	// before retry attempt i+1; a longer server Retry-After wins).
	// nil = DefaultRetryDelays (1s/2s/4s/8s); empty non-nil = disabled.
	RetryDelays []time.Duration

	// ── Long-task context compaction ─────────────────────────────

	// Compaction overrides the conversation compaction strategy applied at
	// the token-budget trigger points (60% pre-request check, 80%
	// auto-compact). nil = compact.NewCombo(compact.NewSummarize(...),
	// compact.NewTrim()): the oldest rounds are condensed into one structured
	// summary (task goal / completed steps / decisions / open questions /
	// files) while the last 6 rounds stay verbatim, and any failure — no
	// summarizer, provider error, empty summary, timeout — falls back to the
	// gline watermark Trim.
	Compaction Strategy

	// Summarizer replaces the summarizer used by the default compaction
	// strategy. nil = one non-streaming call over the session provider
	// (NewProviderSummarizer). Ignored when Compaction is set.
	Summarizer Summarizer

	// SummarizePrompt overrides the summarization instructions
	// (DefaultSummarizePrompt). The transcript is always appended after the
	// prompt. Ignored when Compaction is set.
	SummarizePrompt string

	// ── Optional hooks ───────────────────────────────────────────

	// Guard is consulted before every tool execution (both execution
	// paths). deny → the reason is fed back to the model; ErrAbortRun →
	// the whole run aborts.
	Guard ToolGuard

	// Store persists tasks, messages and tool calls. Implement this to
	// integrate with your own storage; nil disables persistence.
	Store Store

	// Title is the task title used when a new task is created.
	Title string
}

// NewAgent validates cfg and assembles a runnable Agent.
func NewAgent(cfg Config) (*Agent, error) {
	prov := cfg.Provider
	if prov == nil {
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("cobot: Config.APIKey is required (or inject Config.Provider directly)")
		}
		prov = provider.NewOpenAIProvider(cfg.APIKey, cfg.Model, cfg.BaseURL)
	}

	reg := cfg.Tools
	if reg == nil {
		reg = tools.InitDefaultRegistry()
	}

	return loop.New(loop.Options{
		Provider:               prov,
		ToolRegistry:           reg,
		Mode:                   loop.Mode(cfg.Mode),
		AutoApprove:            cfg.AutoApprove,
		MaxConsecutiveMistakes: cfg.MaxConsecutiveMistakes,
		CustomRules:            cfg.CustomRules,
		AgentName:              cfg.AgentName,
		MaxTokens:              cfg.MaxTokens,
		RetryDelays:            cfg.RetryDelays,
		Compaction:             cfg.Compaction,
		Summarizer:             cfg.Summarizer,
		SummarizePrompt:        cfg.SummarizePrompt,
		Guard:                  cfg.Guard,
		Store:                  cfg.Store,
		Title:                  cfg.Title,
	})
}
