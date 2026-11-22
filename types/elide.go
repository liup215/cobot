package types

import "fmt"

// Tool results are the part of a transcript that grows without bound: a file
// read, a grep over a large tree or one MCP call can each return thousands of
// tokens, and a single agent round may issue half a dozen such calls. Round
// based compaction cannot help there — keeping "the last 6 rounds" keeps the
// whole window, and a kept round that is bigger than the keep budget makes the
// compaction a no-op that has to run again on the very next turn.
//
// The three knobs below bound that growth at three different seams:
//
//   - MaxToolResultTokens caps ONE tool result when it is appended;
//   - RoundToolBudgetTokens caps the SUM of tool results inside one round, so
//     a batch of calls cannot add a window's worth of text in one turn;
//   - ElideToolResults bounds the tool payload of an already-recorded
//     transcript (loaded history, or the tail a compaction keeps) without
//     dropping rows.
//
// All three replace the payload with a short stub that names the tool and says
// the output can be re-obtained, so nothing silently disappears: the agent
// knows a call happened and can re-run it.
const (
	// DefaultToolResultTokens caps a single tool result message at append
	// time when neither MaxToolResultTokens nor PerMessageSoftCap is set.
	DefaultToolResultTokens = 4000

	// DefaultRoundToolBudgetPercent bounds the tool output one round may add,
	// as a percentage of MaxTokens (128k -> ~15k tokens). Exceeding it stubs
	// the OLDEST results of that round first, never the newest one.
	DefaultRoundToolBudgetPercent = 12

	// DefaultToolPayloadPercent bounds how much of the window the tool output
	// of a whole transcript may occupy (128k -> ~51k). It is the default used
	// by ElideToolResults when no explicit budget is given, and it is what
	// keeps a reloaded or freshly compacted transcript from being dominated by
	// payloads the model has already consumed.
	DefaultToolPayloadPercent = 40

	// ToolStubMinTokens is the size below which a tool result is never
	// elided: replacing a small payload saves nothing and loses detail.
	ToolStubMinTokens = 400
)

// ToolResultStub renders the replacement text for an elided tool result.
func ToolResultStub(toolCallID string, omitted int) string {
	name := toolCallID
	if name == "" {
		name = "unknown"
	}
	return fmt.Sprintf(
		"[tool output omitted: %d tokens elided from tool result %q by the context manager. Re-run the tool if you need the content again.]",
		omitted, name,
	)
}

// toolResultCap resolves the per-message cap applied to tool results.
func (c *Conversation) toolResultCap() int {
	if c.MaxToolResultTokens > 0 {
		return c.MaxToolResultTokens
	}
	return c.PerMessageSoftCap // 0 = disabled, the same convention as before
}

// roundToolBudget resolves the per-round tool output budget (0 = disabled).
func (c *Conversation) roundToolBudget() int {
	if c.RoundToolBudgetTokens > 0 {
		return c.RoundToolBudgetTokens
	}
	if c.MaxTokens <= 0 {
		return 0
	}
	return c.MaxTokens * DefaultRoundToolBudgetPercent / 100
}

// bodyStart is the index of the first message that may be elided or dropped:
// a leading system message is never touched.
func (c *Conversation) bodyStart() int {
	if c.HasSystemPrompt() {
		return 1
	}
	return 0
}

// newestRoundStart returns the index where the newest round begins (the last
// user message at or after start). When the conversation has no user message
// after start, start is returned, i.e. everything counts as the newest round.
func (c *Conversation) newestRoundStart(start int) int {
	for i := len(c.Messages) - 1; i >= start; i-- {
		if c.Messages[i].Role == RoleUser {
			return i
		}
	}
	return start
}

// ToolPayloadTokens is the estimated size of every tool result in the
// transcript. It is the quantity ElideToolResults budgets against — deliberately
// not the whole transcript, so a session that is large because of ordinary
// dialogue is never shortened here (that is compaction's job).
func (c *Conversation) ToolPayloadTokens() int {
	total := 0
	for i := c.bodyStart(); i < len(c.Messages); i++ {
		if c.Messages[i].Role == RoleTool {
			total += estimateTokens(c.Messages[i].Content)
		}
	}
	return total
}

// ElideToolResults stubs oversized tool results until the transcript's tool
// payload fits toolBudget tokens (0 = MaxTokens * DefaultToolPayloadPercent).
// It returns the number of tokens freed.
//
// Policy, in order:
//  1. results older than the newest round are elided first (oldest first);
//  2. only if that is not enough, results INSIDE the newest round are elided
//     (oldest first), never the very last one — the model may still be
//     reading the freshest output.
//
// Elision never changes the message count, so a compaction anchor computed
// before it stays valid, and it is idempotent: a stubbed result is already
// small.
func (c *Conversation) ElideToolResults(toolBudget int) int {
	if toolBudget <= 0 {
		if c.MaxTokens <= 0 {
			return 0
		}
		toolBudget = c.MaxTokens * DefaultToolPayloadPercent / 100
	}
	if len(c.Messages) == 0 {
		return 0
	}
	total := c.ToolPayloadTokens()
	if total <= toolBudget {
		return 0
	}
	start := c.bodyStart()
	newest := c.newestRoundStart(start)

	freed := 0
	elide := func(i int) {
		m := &c.Messages[i]
		if m.Role != RoleTool {
			return
		}
		t := estimateTokens(m.Content)
		if t <= ToolStubMinTokens {
			return
		}
		stub := ToolResultStub(m.ToolCallID, t)
		delta := t - estimateTokens(stub)
		if delta <= 0 {
			return
		}
		m.Content = stub
		total -= delta
		freed += delta
	}

	for i := start; i < newest && total > toolBudget; i++ {
		elide(i)
	}
	if total > toolBudget {
		lastTool := -1
		for i := len(c.Messages) - 1; i >= newest; i-- {
			if c.Messages[i].Role == RoleTool {
				lastTool = i
				break
			}
		}
		for i := newest; i < len(c.Messages) && total > toolBudget; i++ {
			if i == lastTool {
				break
			}
			elide(i)
		}
	}

	if freed > 0 {
		c.updateTokenCount()
		c.ResetActualTokens()
	}
	return freed
}

// enforceRoundToolBudget stubs the oldest tool results of the round that
// contains the last message while that round's tool output exceeds the
// configured budget. It is called after a tool result is appended, so one
// turn cannot add more than the budget no matter how many tools it calls.
func (c *Conversation) enforceRoundToolBudget() int {
	budget := c.roundToolBudget()
	if budget <= 0 || len(c.Messages) == 0 {
		return 0
	}
	start := c.newestRoundStart(c.bodyStart())
	if c.Messages[len(c.Messages)-1].Role != RoleTool {
		return 0
	}

	sum := func() int {
		total := 0
		for i := start; i < len(c.Messages); i++ {
			if c.Messages[i].Role == RoleTool {
				total += estimateTokens(c.Messages[i].Content)
			}
		}
		return total
	}

	total := sum()
	if total <= budget {
		return 0
	}
	freed := 0
	// Oldest first, but never the newest result (the one just appended): the
	// model asked for it in this very turn.
	for i := start; i < len(c.Messages)-1 && total > budget; i++ {
		m := &c.Messages[i]
		if m.Role != RoleTool {
			continue
		}
		t := estimateTokens(m.Content)
		if t <= ToolStubMinTokens {
			continue
		}
		stub := ToolResultStub(m.ToolCallID, t)
		delta := t - estimateTokens(stub)
		if delta <= 0 {
			continue
		}
		m.Content = stub
		total -= delta
		freed += delta
	}
	if freed > 0 {
		c.updateTokenCount()
		c.ResetActualTokens()
	}
	return freed
}
