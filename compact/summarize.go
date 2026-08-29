package compact

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/liup215/cobot/types"
)

// Summarizer produces the replacement text for conversation rounds that are
// about to be compacted. Implementations typically perform a single LLM call;
// the prompt already contains the instructions and the transcript, so an
// implementation only has to relay it to its model and return the text.
//
// loop.ProviderSummarizer adapts any Provider (a single non-streaming call);
// hosts with special needs (cheaper model, RAG recall, human in the loop)
// implement the interface directly or use SummarizerFunc.
type Summarizer interface {
	Summarize(ctx context.Context, prompt string) (string, error)
}

// SummarizerFunc adapts a plain function to the Summarizer interface.
type SummarizerFunc func(ctx context.Context, prompt string) (string, error)

// Summarize implements Summarizer.
func (f SummarizerFunc) Summarize(ctx context.Context, prompt string) (string, error) {
	return f(ctx, prompt)
}

// DefaultSummarizePrompt instructs the model how to condense the dropped
// rounds. It is appended to the transcript, so a host that overrides Prompt
// does not need to reference the transcript explicitly — it is appended after
// whatever prompt is configured.
const DefaultSummarizePrompt = `You are the context-compaction assistant of an autonomous coding agent.
The conversation grew too long and the rounds below are about to be removed.
The most recent rounds are kept verbatim and are NOT part of the transcript.

Summarize the transcript so the agent can continue the task without them.
Be concrete and terse. Output exactly these five sections:

Task goal: the original task and any clarifications the user gave.
Completed steps: what was already done, with outcomes (commands run, edits made, results observed).
Key decisions: choices made and why, including rejected approaches and discovered constraints.
Open questions: anything unresolved or waiting on the user.
Files: every file that was read or modified and its current state.

Rules:
- Keep file paths, command lines, identifiers and error messages verbatim.
- Never invent work that is not visible in the transcript.
- Do not add commentary outside the five sections.

Transcript of the rounds being compacted (oldest first):
`

// Summarize is the LLM summarization compaction strategy: it keeps the most
// recent KeepRounds rounds verbatim and replaces everything older with one
// structured summary message.
//
// Protected content, identical to Trim: the leading system message and the
// first user message (the original task). Everything older than the retained
// window is rendered into a transcript and summarized, so nothing is lost to
// a lossy drop.
//
// Any failure — no summarizer configured, provider error, empty summary,
// timeout, over-long transcript — is reported as an error so a Combo can fall
// back to Trim.
type Summarize struct {
	// Summarizer produces the summary text. Required; without it Compact
	// returns ErrNoSummarizer and ShouldCompact reports false.
	Summarizer Summarizer

	// Prompt overrides the summarization instructions. Empty =
	// DefaultSummarizePrompt. The transcript is always appended after it.
	Prompt string

	// KeepRounds is the number of most-recent rounds kept verbatim.
	// 0 = DefaultKeepRounds.
	KeepRounds int

	// LowWatermarkPercent triggers compaction above this percentage of
	// MaxTokens. 0 = DefaultLowWatermarkPercent.
	LowWatermarkPercent int

	// HighWatermarkPercent is informational for this strategy: it is used to
	// detect "compaction did not free enough room" in ShouldCompact.
	// 0 = DefaultHighWatermarkPercent.
	HighWatermarkPercent int

	// Timeout bounds a single Summarizer call. 0 = DefaultSummarizeTimeout.
	Timeout time.Duration

	// MaxSummaryTokens caps the summary text. 0 = DefaultMaxSummaryTokens.
	MaxSummaryTokens int

	// MaxTranscriptTokens caps the transcript handed to the Summarizer.
	// 0 = DefaultMaxTranscriptTokens.
	MaxTranscriptTokens int
}

// NewSummarize returns a Summarize strategy over the given summarizer with
// default watermarks, timeout and caps.
func NewSummarize(summarizer Summarizer) *Summarize {
	return &Summarize{Summarizer: summarizer}
}

// summarizeHeaderFormat prefixes the replacement message. It is a user-role
// message so it stays in the message flow on both wire formats (see Trim).
const summarizeHeaderFormat = "[COMPACTED] Early conversation summarized: the %d oldest round(s) (%d messages) were replaced by the summary below. " +
	"Treat it as the record of what already happened; re-read files or re-run tools when you need exact contents.\n\n"

func (s *Summarize) keepRounds() int {
	if s.KeepRounds <= 0 {
		return DefaultKeepRounds
	}
	return s.KeepRounds
}

func (s *Summarize) lowWatermark() int {
	if s.LowWatermarkPercent <= 0 {
		return DefaultLowWatermarkPercent
	}
	return s.LowWatermarkPercent
}

func (s *Summarize) timeout() time.Duration {
	if s.Timeout <= 0 {
		return DefaultSummarizeTimeout
	}
	return s.Timeout
}

func (s *Summarize) maxSummaryTokens() int {
	if s.MaxSummaryTokens <= 0 {
		return DefaultMaxSummaryTokens
	}
	return s.MaxSummaryTokens
}

func (s *Summarize) maxTranscriptTokens() int {
	if s.MaxTranscriptTokens <= 0 {
		return DefaultMaxTranscriptTokens
	}
	return s.MaxTranscriptTokens
}

// ShouldCompact reports whether the conversation is above the low watermark,
// a summarizer is configured and there is at least one round outside the
// protected window to summarize. Without a compactable round the strategy
// stays silent (and never spends an LLM call).
func (s *Summarize) ShouldCompact(conv *types.Conversation) bool {
	if s == nil || s.Summarizer == nil || conv == nil || conv.MaxTokens <= 0 {
		return false
	}
	if !conv.IsTokenAboveThreshold(s.lowWatermark()) {
		return false
	}
	return compactableCut(conv.Messages[systemPrefix(conv):], s.keepRounds()) > 0
}

// Compact summarizes the rounds outside the protected window and replaces
// them with a single summary message.
func (s *Summarize) Compact(ctx context.Context, conv *types.Conversation) error {
	if s == nil || conv == nil || conv.MaxTokens <= 0 {
		return nil
	}
	if s.Summarizer == nil {
		return ErrNoSummarizer
	}

	sys := systemPrefix(conv)
	body := conv.Messages[sys:]
	rounds := SplitRounds(body)
	keep := s.keepRounds()
	if len(rounds) <= keep {
		return nil // nothing outside the protected window
	}
	droppedRounds := len(rounds) - keep
	cut := rounds[droppedRounds-1].End

	// The dropped set is body[0:cut] minus the protected first user message.
	firstUser := firstUserMessage(body)
	dropped := make([]types.Message, 0, cut)
	for i := 0; i < cut; i++ {
		if i == firstUser {
			continue
		}
		dropped = append(dropped, body[i])
	}
	if len(dropped) == 0 {
		return nil
	}

	summary, err := s.call(ctx, dropped)
	if err != nil {
		return err
	}

	msgs := make([]types.Message, 0, len(conv.Messages))
	msgs = append(msgs, conv.Messages[:sys]...)
	// The original task survives verbatim, ahead of the summary — but only
	// when it actually sat in the dropped range, the same boundary the dropped
	// set was built with. When the history does not open with a user message
	// (a hard pre-send trim removed the original turn, or the session was
	// restored from the store), firstUser >= cut: the task is already part of
	// the retained window and re-inserting it here would duplicate it.
	if firstUser >= 0 && firstUser < cut {
		msgs = append(msgs, body[firstUser])
	}
	msgs = append(msgs, types.Message{
		Role:    types.RoleUser,
		Content: fmt.Sprintf(summarizeHeaderFormat, droppedRounds, len(dropped)) + summary,
	})
	msgs = append(msgs, body[cut:]...)

	conv.ReplaceMessages(msgs)
	slog.Info("compact: summarized oldest rounds",
		"rounds", droppedRounds, "droppedMessages", len(dropped),
		"summaryTokens", types.Estimator(summary), "tokens", conv.GetTotalTokens())
	return nil
}

// call renders the transcript, invokes the Summarizer under the configured
// timeout and validates the result.
func (s *Summarize) call(ctx context.Context, dropped []types.Message) (string, error) {
	prompt := s.prompt() + "\n" + renderTranscript(dropped, s.maxTranscriptTokens())

	callCtx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()

	summary, err := s.Summarizer.Summarize(callCtx, prompt)
	if err != nil {
		return "", fmt.Errorf("compact: summarizer failed: %w", err)
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "", ErrEmptySummary
	}
	return truncateToTokenBudget(summary, s.maxSummaryTokens()), nil
}

func (s *Summarize) prompt() string {
	if strings.TrimSpace(s.Prompt) == "" {
		return DefaultSummarizePrompt
	}
	return s.Prompt
}

// compactableCut returns the index one past the last message that may be
// compacted (the end of the last round outside the protected window), or 0
// when every round is protected.
func compactableCut(body []types.Message, keepRounds int) int {
	rounds := SplitRounds(body)
	if len(rounds) <= keepRounds {
		return 0
	}
	return rounds[len(rounds)-keepRounds-1].End
}

// toolNames maps tool-call IDs to tool names by scanning assistant messages,
// so tool results in the transcript can be labeled with their tool name.
func toolNames(msgs []types.Message) map[string]string {
	names := make(map[string]string)
	for i := range msgs {
		for _, tc := range msgs[i].ToolCalls {
			if tc.ID != "" {
				names[tc.ID] = tc.Name
			}
		}
	}
	return names
}

// maxTranscriptMessageChars caps one rendered message in the transcript so a
// single huge message cannot eat the whole transcript budget.
const maxTranscriptMessageChars = 2000

// renderMessage renders one message for the summarizer transcript.
func renderMessage(m types.Message, names map[string]string) string {
	switch m.Role {
	case types.RoleSystem:
		return fmt.Sprintf("[system]\n%s", truncateRunes(m.Content, maxTranscriptMessageChars))
	case types.RoleUser:
		return fmt.Sprintf("[user]\n%s", truncateRunes(m.Content, maxTranscriptMessageChars))
	case types.RoleAssistant:
		var b strings.Builder
		b.WriteString("[assistant]")
		if m.Content != "" {
			b.WriteString("\n" + truncateRunes(m.Content, maxTranscriptMessageChars))
		}
		if m.ReasoningContent != "" {
			b.WriteString("\n(thinking omitted)")
		}
		for _, tc := range m.ToolCalls {
			b.WriteString(fmt.Sprintf("\n-> tool_call %s(%s)", tc.Name, truncateRunes(string(tc.Input), 200)))
		}
		return b.String()
	case types.RoleTool:
		name := names[m.ToolCallID]
		if name == "" {
			name = m.ToolCallID
		}
		return fmt.Sprintf("[tool result of %s]\n%s", name, truncateRunes(m.Content, maxTranscriptMessageChars))
	default:
		return ""
	}
}

// renderTranscript renders messages oldest-first, capped at budgetTokens
// (estimated). When the budget forces a cut, the oldest messages are dropped
// from the transcript — the newest ones sit next to the retained rounds and
// are the most valuable for continuing the task.
func renderTranscript(msgs []types.Message, budgetTokens int) string {
	if len(msgs) == 0 {
		return "(nothing to summarize)"
	}
	names := toolNames(msgs)

	type rendered struct {
		text   string
		tokens int
	}
	items := make([]rendered, 0, len(msgs))
	for i := range msgs {
		text := renderMessage(msgs[i], names)
		if strings.TrimSpace(text) == "" {
			continue
		}
		items = append(items, rendered{text: text, tokens: types.Estimator(text)})
	}
	if len(items) == 0 {
		return "(nothing to summarize)"
	}

	keep := 0
	used := 0
	for i := len(items) - 1; i >= 0; i-- {
		if keep > 0 && used+items[i].tokens > budgetTokens {
			break
		}
		used += items[i].tokens
		keep++
	}
	omitted := len(items) - keep

	var b strings.Builder
	if omitted > 0 {
		fmt.Fprintf(&b, "(%d older message(s) omitted from this transcript to fit the summarizer budget)\n\n", omitted)
	}
	for _, it := range items[len(items)-keep:] {
		b.WriteString(it.text)
		b.WriteString("\n\n")
	}
	return b.String()
}

// truncateToTokenBudget returns the longest prefix of s whose estimated token
// count fits maxTokens, appending an omission marker when it cut anything.
func truncateToTokenBudget(s string, maxTokens int) string {
	if maxTokens <= 0 || types.Estimator(s) <= maxTokens {
		return s
	}
	runes := []rune(s)
	lo, hi := 1, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if types.Estimator(string(runes[:mid])) <= maxTokens {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo]) + "\n[... summary truncated ...]"
}
