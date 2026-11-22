package types

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"
)

// Conversation represents a conversation between user and assistant.
// It tracks messages plus an approximate token budget with watermark-based
// compaction helpers.
type Conversation struct {
	// Messages is the list of messages.
	Messages []Message

	// MaxTokens is the maximum number of tokens allowed (context window).
	MaxTokens int

	// CurrentTokens is the estimated current token count (rough estimate).
	CurrentTokens int

	// Complete indicates if the conversation is complete.
	Complete bool

	// PerMessageSoftCap is the maximum allowed tokens for a single message
	// before it is locally truncated. 0 means disabled.
	PerMessageSoftCap int

	// MaxToolResultTokens is the cap applied to a single TOOL result message
	// when it is appended. Tool output is re-obtainable (the agent can re-run
	// the tool), so it is capped harder than ordinary messages: a handful of
	// large results is what fills a context window and makes compaction
	// unable to free anything. 0 falls back to PerMessageSoftCap (so 0 there
	// means "disabled" as before).
	MaxToolResultTokens int

	// RoundToolBudgetTokens bounds the SUM of tool results inside one round
	// (a user message plus everything it triggered). When appending a result
	// pushes the round over budget, the oldest results of that round are
	// stubbed first. 0 = MaxTokens * DefaultRoundToolBudgetPercent.
	RoundToolBudgetTokens int

	// ResponseBuffer is the number of tokens reserved for the model's response
	// when checking the request budget.
	ResponseBuffer int

	actualInputTokens  int
	actualOutputTokens int
	// lastActual* keep the most recent provider report across a mid-turn
	// ResetActualTokens (see AddActualTokens).
	lastActualInputTokens  int
	lastActualOutputTokens int
	mu                     sync.Mutex
}

// resetLastActualTokens forgets the last provider report — called only when the
// transcript's identity changes, so a new view cannot inherit the previous
// session's usage.
func (c *Conversation) resetLastActualTokens() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastActualInputTokens = 0
	c.lastActualOutputTokens = 0
}

// Default context window used when MaxTokens is not specified.
const defaultMaxTokens = 128000

// NewConversation creates a new conversation with sensible defaults
// (~128K context, 6K per-message soft cap, 4K tool-result cap, 8K response
// buffer).
func NewConversation() *Conversation {
	return &Conversation{
		Messages:            make([]Message, 0),
		MaxTokens:           defaultMaxTokens,
		PerMessageSoftCap:   6000,
		MaxToolResultTokens: DefaultToolResultTokens,
		ResponseBuffer:      8192,
	}
}

// AddActualTokens sets real API token usage for the current turn.
// It replaces (not accumulates) because usage from the API already
// represents the total for the latest request.
func (c *Conversation) AddActualTokens(input, output int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.actualInputTokens = input
	c.actualOutputTokens = output
	// The LAST reported pair survives ResetActualTokens: the live counters are
	// the accounting basis and legitimately go stale when the transcript is
	// rewritten mid-turn (tool output stubbed, window trimmed), but the host
	// still needs the provider's number to stamp the turn's usage fact. Losing
	// it is how a session ends up showing +≈ forever.
	c.lastActualInputTokens = input
	c.lastActualOutputTokens = output
}

// GetActualTokens returns the real token usage reported by the API.
func (c *Conversation) GetActualTokens() (input, output int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.actualInputTokens, c.actualOutputTokens
}

// LastActualTokens returns the most recent provider-reported usage even if the
// live counters have since been reset (see AddActualTokens). It is zeroed only
// when the transcript's identity changes (Clear / ReplaceMessages / NewTask).
func (c *Conversation) LastActualTokens() (input, output int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastActualInputTokens, c.lastActualOutputTokens
}

// ResetActualTokens resets the real token counters.
func (c *Conversation) ResetActualTokens() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.actualInputTokens = 0
	c.actualOutputTokens = 0
}

// AddMessage adds a message to the conversation. If the message content
// exceeds PerMessageSoftCap it is locally truncated to keep a single message
// from exploding the context.
func (c *Conversation) AddMessage(msg Message) {
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}
	msg = c.truncateMessage(msg)
	c.Messages = append(c.Messages, msg)
	c.updateTokenCount()
	if msg.Role == RoleTool {
		c.enforceRoundToolBudget()
	}
}

// truncateMessage applies a soft cap to a single message's content. It keeps
// the head and tail of the content and replaces the middle with an omission
// marker so useful context at both ends is preserved.
func (c *Conversation) truncateMessage(msg Message) Message {
	softCap := c.PerMessageSoftCap
	if msg.Role == RoleTool {
		// Tool output is the unbounded part of a transcript and the only part
		// the agent can always re-obtain, so it gets its own (tighter) cap.
		softCap = c.toolResultCap()
	}
	if softCap <= 0 {
		return msg
	}
	contentTokens := estimateTokens(msg.Content)
	if contentTokens <= softCap {
		return msg
	}

	targetHead := softCap / 4
	targetTail := softCap / 4
	if targetHead < 1 {
		targetHead = 1
	}
	if targetTail < 1 {
		targetTail = 1
	}

	head := truncateToTokens(msg.Content, targetHead)
	tail := truncateFromEndToTokens(msg.Content, targetTail)
	omitted := contentTokens - estimateTokens(head) - estimateTokens(tail)
	if omitted < 0 {
		omitted = contentTokens / 2
	}
	msg.Content = fmt.Sprintf(
		"%s\n[... %d tokens omitted ...]\n%s",
		head, omitted, tail,
	)
	return msg
}

// GetMessages returns all messages in the conversation.
func (c *Conversation) GetMessages() []Message {
	return c.Messages
}

// GetLastMessage returns the last message in the conversation, or nil.
func (c *Conversation) GetLastMessage() *Message {
	if len(c.Messages) == 0 {
		return nil
	}
	return &c.Messages[len(c.Messages)-1]
}

// Clear removes all messages from the conversation.
func (c *Conversation) Clear() {
	c.Messages = make([]Message, 0)
	c.CurrentTokens = 0
	c.ResetActualTokens()
	c.resetLastActualTokens()
	c.Complete = false
}

// MarkIncomplete marks the conversation as needing more processing.
func (c *Conversation) MarkIncomplete() { c.Complete = false }

// SetComplete marks the conversation as complete.
func (c *Conversation) SetComplete() { c.Complete = true }

// IsComplete returns true if the conversation is complete.
func (c *Conversation) IsComplete() bool { return c.Complete }

// updateTokenCount estimates the token count of the whole conversation.
func (c *Conversation) updateTokenCount() {
	total := 0
	for _, msg := range c.Messages {
		total += messageTokens(msg)
	}
	c.CurrentTokens = total
}

// messageTokens returns the estimated token count of a single message.
func messageTokens(m Message) int {
	total := estimateTokens(m.Content) +
		estimateTokens(string(m.Role)) +
		estimateTokens(m.ReasoningContent)
	for _, tc := range m.ToolCalls {
		total += estimateTokens(tc.Name)
		total += estimateTokens(string(tc.Input))
	}
	return total
}

// GetTotalTokens returns the best estimate of total tokens used.
// Uses actual API-reported tokens when available, falls back to estimation.
func (c *Conversation) GetTotalTokens() int {
	totalActual := c.actualInputTokens + c.actualOutputTokens
	if totalActual > 0 {
		return totalActual
	}
	return c.CurrentTokens
}

// TrimToMaxTokens removes oldest messages to fit within the token limit —
// TrimToBudget with the conversation's own MaxTokens as the target. It is kept
// for callers that predate budget-aware trimming.
func (c *Conversation) TrimToMaxTokens() { c.TrimToBudget(0) }

// TrimToBudget drops the oldest messages until the conversation fits target
// tokens (target <= 0 means MaxTokens — the hard window, not the watermark).
//
// Before dropping a large tool result message it replaces the content with a
// short placeholder summary, so the agent still knows that a tool was called.
// It uses GetTotalTokens so real API usage has priority.
//
// The trim is round-aware: it never leaves a tool result stranded at the front
// of the conversation without the assistant message that requested it. Both
// wire formats reject an orphan tool message, and dropping message-by-message
// from the front splits tool_call/tool_result pairs exactly half the time.
// TrimToBudget drops the oldest messages until the conversation fits target
// tokens (target <= 0 means MaxTokens — the hard window, not the watermark).
//
// Before dropping anything it replaces oversized tool results with a short
// placeholder, so the agent still knows that a tool was called and can re-run
// it. It uses GetTotalTokens so real API usage has priority.
//
// Dropping is round-aware: a round is a user message plus everything the
// assistant and its tools produced until the next user message, and rounds are
// removed whole. Dropping message-by-message from the front splits
// tool_call/tool_result pairs about half the time, and both wire formats
// reject a tool result whose requesting assistant message is gone — a trim
// would then turn a too-long session into a hard 400 instead of a short one.
func (c *Conversation) TrimToBudget(target int) {
	if c.MaxTokens <= 0 {
		return
	}
	if target <= 0 || target > c.MaxTokens {
		target = c.MaxTokens
	}
	if c.GetTotalTokens() <= target {
		return
	}

	// Keep the system message if present; everything after it is droppable.
	startIdx := 0
	if c.HasSystemPrompt() {
		startIdx = 1
	}

	// 1) Shrink oversized tool results in place. This is the gentlest cut: the
	// turn structure survives, only the payload is replaced by a hint.
	for i := startIdx; i < len(c.Messages) && c.GetTotalTokens() > target; i++ {
		m := c.Messages[i]
		if m.Role != RoleTool || len(m.Content) <= 200 {
			continue
		}
		c.Messages[i].Content = fmt.Sprintf(
			"[Result of %s was %d tokens; truncated by context manager. Re-run the tool if needed.]",
			m.ToolCallID, estimateTokens(m.Content),
		)
		c.updateTokenCount()
		c.ResetActualTokens()
	}
	if c.GetTotalTokens() <= target {
		return
	}

	// 2) Drop whole rounds from the front, never the newest one.
	for c.GetTotalTokens() > target {
		if len(c.Messages) <= startIdx+2 {
			return
		}
		end := startIdx + 1
		for end < len(c.Messages) && c.Messages[end].Role != RoleUser {
			end++
		}
		if end >= len(c.Messages)-1 {
			// One giant round (a single huge tool loop, no user turn after it):
			// rounds cannot be dropped whole, so fall back to message-by-message
			// and clean up the orphaned tool result at the front.
			c.Messages = append(c.Messages[:startIdx], c.Messages[startIdx+1:]...)
			for len(c.Messages) > startIdx+1 && c.Messages[startIdx].Role == RoleTool {
				c.Messages = append(c.Messages[:startIdx], c.Messages[startIdx+1:]...)
			}
			c.updateTokenCount()
			c.ResetActualTokens()
			continue
		}
		c.Messages = append(c.Messages[:startIdx], c.Messages[end:]...)
		c.updateTokenCount()
		c.ResetActualTokens()
	}
}
func (c *Conversation) AutoCompact() {
	if c.MaxTokens <= 0 {
		return
	}
	total := c.GetTotalTokens()
	lowWatermark := c.MaxTokens * 6 / 10  // 60%
	highWatermark := c.MaxTokens * 8 / 10 // 80%

	if total <= lowWatermark {
		return
	}

	startIdx := 0
	if c.HasSystemPrompt() {
		startIdx = 1
	}

	firstPairEnd := startIdx + 1 // inclusive index of first assistant msg
	if firstPairEnd >= len(c.Messages) {
		return
	}

	// Phase 1: truncate individual oversized messages in the middle region.
	if total > highWatermark {
		for i := firstPairEnd + 1; i < len(c.Messages)-2; i++ {
			msgTokens := messageTokens(c.Messages[i])
			if c.PerMessageSoftCap > 0 && msgTokens > c.PerMessageSoftCap {
				c.Messages[i] = c.truncateMessage(c.Messages[i])
			}
		}
		c.updateTokenCount()
		total = c.GetTotalTokens()
	}

	if total <= lowWatermark {
		return
	}

	// Phase 2: drop whole messages from the middle, preferring the largest
	// token consumers first. Keep the first pair and the last pair.
	for c.GetTotalTokens() > highWatermark && len(c.Messages) > startIdx+4 {
		maxIdx := -1
		maxTokens := 0
		for i := firstPairEnd + 1; i < len(c.Messages)-2; i++ {
			t := messageTokens(c.Messages[i])
			if t > maxTokens {
				maxTokens = t
				maxIdx = i
			}
		}
		if maxIdx < 0 {
			break
		}

		removed := c.Messages[maxIdx]
		removedTokens := messageTokens(removed)
		c.Messages = append(c.Messages[:maxIdx], c.Messages[maxIdx+1:]...)
		c.CurrentTokens -= removedTokens
		if c.CurrentTokens < 0 {
			c.CurrentTokens = 0
		}
	}

	c.ResetActualTokens()
	c.updateTokenCount()

	// Insert a compact marker so the model knows context was trimmed.
	marker := Message{
		Role:    RoleSystem,
		Content: "[Context compacted: older messages were summarized or removed to fit token budget. Original task context preserved.]",
	}
	tail := append([]Message{marker}, c.Messages[firstPairEnd+1:]...)
	c.Messages = append(c.Messages[:firstPairEnd+1], tail...)
	c.updateTokenCount()
}

// ReplaceMessages swaps the conversation's messages for msgs and re-estimates
// the token count. Compaction strategies use it to rewrite history in place.
// Real API-reported token usage is reset because it no longer describes the
// replaced history.
func (c *Conversation) ReplaceMessages(msgs []Message) {
	if msgs == nil {
		msgs = make([]Message, 0)
	}
	c.Messages = msgs
	c.updateTokenCount()
	c.ResetActualTokens()
	c.resetLastActualTokens()
}

// PatchMessageContent rewrites ONE message's content in place and adjusts
// the token estimate by the delta. Unlike ReplaceMessages it does NOT reset
// the actual-usage counters: hosts stamp usage at the NEXT stream start
// (closeTurn), and a session-level reset here would silently drop the turn's
// facts. Designed for deliver-once marking (host stubs delivered image
// markers between requests while the run keeps streaming).
// Returns false when idx is out of range.
func (c *Conversation) PatchMessageContent(idx int, content string) bool {
	if idx < 0 || idx >= len(c.Messages) {
		return false
	}
	m := c.Messages[idx]
	if m.Content == content {
		return true
	}
	before := MessageTokens(m)
	m.Content = content
	c.Messages[idx] = m
	c.CurrentTokens += MessageTokens(m) - before
	if c.CurrentTokens < 0 {
		c.CurrentTokens = 0
	}
	return true
}

// MessageTokens returns the estimated token count of a single message,
// including its role, reasoning content and tool calls. Compaction
// strategies use it to account for dropped or inserted messages.
func MessageTokens(m Message) int { return messageTokens(m) }

// IsTokenAboveThreshold checks if current tokens exceed the given
// threshold percentage of MaxTokens.
func (c *Conversation) IsTokenAboveThreshold(percent int) bool {
	if c.MaxTokens <= 0 {
		return false
	}
	return c.GetTotalTokens() > c.MaxTokens*percent/100
}

// ToJSON returns the conversation as indented JSON.
func (c *Conversation) ToJSON() (string, error) {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// MessageCount returns the number of messages.
func (c *Conversation) MessageCount() int { return len(c.Messages) }

// HasSystemPrompt returns true if the conversation starts with a system prompt.
func (c *Conversation) HasSystemPrompt() bool {
	return len(c.Messages) > 0 && c.Messages[0].Role == RoleSystem
}

// SetSystemPrompt sets or replaces the leading system prompt.
func (c *Conversation) SetSystemPrompt(content string) {
	if c.HasSystemPrompt() {
		c.Messages[0].Content = content
	} else {
		c.Messages = append([]Message{{Role: RoleSystem, Content: content}}, c.Messages...)
	}
	c.updateTokenCount()
}

// truncateToTokens returns the leading portion of s whose token count does
// not exceed maxTokens.
func truncateToTokens(s string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	tokens := 0
	for i, r := range s {
		runeTokens := 1
		if r < 128 && (r == ' ' || r == '\t' || r == '\n' || r == '\r') {
			runeTokens = 0
		}
		tokens += runeTokens
		if tokens > maxTokens {
			return s[:i]
		}
	}
	return s
}

// truncateFromEndToTokens returns the trailing portion of s whose token count
// does not exceed maxTokens.
func truncateFromEndToTokens(s string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	tokens := 0
	var start int
	for i := len(s); i > 0; {
		r, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
		runeTokens := 1
		if r < 128 && (r == ' ' || r == '\t' || r == '\n' || r == '\r') {
			runeTokens = 0
		}
		tokens += runeTokens
		if tokens > maxTokens {
			start = i + size
			break
		}
		start = i
	}
	return s[start:]
}
