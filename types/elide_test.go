package types

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// exactEstimator pins the estimator to 1 token per rune so the arithmetic in
// these tests is readable. It is the same trick compact's tests use.
func exactEstimator(t *testing.T) {
	t.Helper()
	orig := Estimator
	Estimator = func(s string) int { return utf8.RuneCountInString(s) }
	t.Cleanup(func() { Estimator = orig })
}

// big returns a payload of n runes, tagged so tests can tell instances apart.
func big(tag string, n int) string {
	return tag + strings.Repeat("x", n)
}

// toolRound appends one round (user turn + assistant + tool results) of the
// given payload sizes to the conversation.
func toolRound(conv *Conversation, tag string, sizes ...int) {
	conv.AddMessage(Message{Role: RoleUser, Content: "run " + tag})
	conv.AddMessage(Message{Role: RoleAssistant, Content: "calling " + tag})
	for i, n := range sizes {
		conv.AddMessage(Message{
			Role:       RoleTool,
			ToolCallID: fmt.Sprintf("%s-call-%d", tag, i),
			Content:    big(fmt.Sprintf("%s-%d ", tag, i), n),
		})
	}
}

// TestAddMessageCapsASingleToolResult pins the first line of defence: one tool
// result can never exceed MaxToolResultTokens, because a single 40k-token file
// dump is what makes the next compaction impossible.
func TestAddMessageCapsASingleToolResult(t *testing.T) {
	exactEstimator(t)

	conv := NewConversation()
	conv.MaxToolResultTokens = 4000
	conv.RoundToolBudgetTokens = 1 << 20 // isolate the per-message cap
	conv.AddMessage(Message{Role: RoleTool, ToolCallID: "call-1", Content: big("dump ", 40000)})

	if got := len(conv.Messages[0].Content); got > 4000 {
		t.Fatalf("tool result was stored at %d tokens, over the 4000 cap", got)
	}
	if !strings.Contains(conv.Messages[0].Content, "tokens omitted") {
		t.Fatalf("the cap did not leave an omission marker: %q", conv.Messages[0].Content)
	}
	if !strings.HasPrefix(conv.Messages[0].Content, "dump ") {
		t.Fatalf("the head of the payload was not preserved: %q", conv.Messages[0].Content)
	}
}

// TestRoundToolBudgetStubsTheOldestResultsOfTheRound pins the second line of
// defence: a batch of large calls inside ONE turn cannot add more than the
// per-round budget, and the result the model just asked for survives.
func TestRoundToolBudgetStubsTheOldestResultsOfTheRound(t *testing.T) {
	exactEstimator(t)

	conv := NewConversation()
	conv.MaxTokens = 100000
	conv.MaxToolResultTokens = 0 // disabled: isolate the round budget
	conv.PerMessageSoftCap = 0
	conv.RoundToolBudgetTokens = 20000

	sizes := []int{9000, 9000, 9000, 9000, 9000} // 45k in one turn
	toolRound(conv, "r1", sizes...)

	payload := conv.ToolPayloadTokens()
	if payload > conv.RoundToolBudgetTokens {
		t.Fatalf("round payload is %d tokens, over the %d budget", payload, conv.RoundToolBudgetTokens)
	}
	last := conv.Messages[len(conv.Messages)-1]
	if !strings.HasPrefix(last.Content, "r1-4 ") {
		t.Fatalf("the newest tool result was elided: %q", last.Content)
	}
	if strings.Contains(last.Content, "omitted") {
		t.Fatalf("the newest tool result was replaced by a stub: %q", last.Content)
	}
	// The oldest results are the ones that had to go.
	first := conv.Messages[2]
	if first.Role != RoleTool || !strings.HasPrefix(first.Content, "[tool output omitted") {
		t.Fatalf("the oldest tool result was not stubbed: %q", first.Content)
	}
}

// TestElideToolResultsFreesTheToolPayloadBudget pins the third line of
// defence: a transcript that is already recorded (loaded history, or the tail
// a compaction kept) can be bound to a tool payload budget without removing a
// single row — so a compaction anchor computed before the elision stays valid.
func TestElideToolResultsFreesTheToolPayloadBudget(t *testing.T) {
	exactEstimator(t)

	conv := NewConversation()
	conv.MaxTokens = 100000
	conv.MaxToolResultTokens = 0
	conv.PerMessageSoftCap = 0
	conv.RoundToolBudgetTokens = 0
	conv.RoundToolBudgetTokens = 1 << 20

	for i := 1; i <= 4; i++ {
		toolRound(conv, fmt.Sprintf("r%d", i), 8000, 8000)
	}
	before := len(conv.Messages)
	payloadBefore := conv.ToolPayloadTokens()
	if payloadBefore <= 12000 {
		t.Fatalf("fixture payload %d is not over the budget", payloadBefore)
	}

	freed := conv.ElideToolResults(12000)
	if freed <= 0 {
		t.Fatalf("nothing was freed")
	}
	if got := conv.ToolPayloadTokens(); got > 12000 {
		t.Fatalf("payload is still %d tokens after elision", got)
	}
	if len(conv.Messages) != before {
		t.Fatalf("elision changed the message count: %d -> %d", before, len(conv.Messages))
	}
	// The newest round is the working state and stays verbatim.
	last := conv.Messages[len(conv.Messages)-1]
	if !strings.HasPrefix(last.Content, "r4-1 ") {
		t.Fatalf("the newest tool result was elided: %q", last.Content)
	}
}

// TestElideToolResultsIsIdempotent guards the load path, which runs on every
// session open: a second pass must not keep shrinking (and must not report a
// change that never happened).
func TestElideToolResultsIsIdempotent(t *testing.T) {
	exactEstimator(t)

	conv := NewConversation()
	conv.MaxTokens = 50000
	conv.MaxToolResultTokens = 0
	conv.PerMessageSoftCap = 0
	conv.RoundToolBudgetTokens = 1 << 20
	for i := 1; i <= 3; i++ {
		toolRound(conv, fmt.Sprintf("r%d", i), 9000)
	}

	if freed := conv.ElideToolResults(10000); freed <= 0 {
		t.Fatalf("first pass freed nothing")
	}
	payload := conv.ToolPayloadTokens()
	if freed := conv.ElideToolResults(10000); freed != 0 {
		t.Fatalf("second pass freed %d more tokens (payload %d): elision is not idempotent", freed, payload)
	}
}

// TestElideToolResultsLeavesSmallToolOutputAlone pins the "no over-reaction"
// half of the contract: a transcript whose tool output already fits is not
// touched at all, so a normal session never loses payloads the model is using.
func TestElideToolResultsLeavesSmallToolOutputAlone(t *testing.T) {
	exactEstimator(t)

	conv := NewConversation()
	conv.MaxTokens = 100000
	conv.MaxToolResultTokens = 0
	conv.PerMessageSoftCap = 0
	conv.RoundToolBudgetTokens = 1 << 20
	toolRound(conv, "r1", 500, 500)
	conv.AddMessage(Message{Role: RoleTool, ToolCallID: "tiny", Content: "ok"})

	before := conv.Messages[2].Content
	if freed := conv.ElideToolResults(40000); freed != 0 {
		t.Fatalf("elided %d tokens from a transcript that already fits", freed)
	}
	if conv.Messages[2].Content != before {
		t.Fatalf("a small tool result was rewritten: %q", conv.Messages[2].Content)
	}
}
