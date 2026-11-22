package compact

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/liup215/cobot/types"
)

// ─── test helpers ────────────────────────────────────────────────────────────

// exactEstimator swaps the package token estimator for an exact one
// (1 token per rune) so every test's arithmetic is deterministic.
func exactEstimator(t *testing.T) {
	t.Helper()
	orig := types.Estimator
	types.Estimator = func(s string) int { return utf8.RuneCountInString(s) }
	t.Cleanup(func() { types.Estimator = orig })
}

// buildConv assembles a conversation from raw messages. The per-message soft
// cap is disabled so test content is never truncated on the way in.
func buildConv(maxTokens int, msgs ...types.Message) *types.Conversation {
	conv := types.NewConversation()
	conv.PerMessageSoftCap = 0
	conv.MaxTokens = maxTokens
	for _, m := range msgs {
		conv.AddMessage(m)
	}
	return conv
}

// sysUser are the two protected messages every test conversation starts with.
const (
	sysContent  = "you are a coding agent"
	taskContent = "original task: fix the login bug"
)

// protectedMessages returns the system + first-user message pair.
func protectedMessages() []types.Message {
	return []types.Message{
		{Role: types.RoleSystem, Content: sysContent},
		{Role: types.RoleUser, Content: taskContent},
	}
}

// roundMessages returns the messages of round i (1-based): a user turn and the
// assistant answer, both tagged with a "round-<i>" marker so tests can tell
// which rounds survived a compaction.
func roundMessages(i, pad int) []types.Message {
	marker := fmt.Sprintf("round-%d ", i)
	return []types.Message{
		{Role: types.RoleUser, Content: fmt.Sprintf("%s user question %s", marker, strings.Repeat("x", pad))},
		{Role: types.RoleAssistant, Content: fmt.Sprintf("%s assistant answer %s", marker, strings.Repeat("y", pad))},
	}
}

// fillerRounds returns rounds 1..n as a flat message slice.
func fillerRounds(count, pad int) []types.Message {
	var msgs []types.Message
	for i := 1; i <= count; i++ {
		msgs = append(msgs, roundMessages(i, pad)...)
	}
	return msgs
}

// fakeSummarizer records the prompts it received and returns canned output.
type fakeSummarizer struct {
	calls   int
	prompts []string
	summary string
	err     error
	block   time.Duration
}

func (f *fakeSummarizer) Summarize(ctx context.Context, prompt string) (string, error) {
	f.calls++
	f.prompts = append(f.prompts, prompt)
	if f.block > 0 {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(f.block):
		}
	}
	return f.summary, f.err
}

// failingStrategy is a Strategy stub whose methods always fail, used to drive
// the Combo's both-fail path.
type failingStrategy struct {
	shouldCompact bool
	err           error
	calls         int
}

func (f *failingStrategy) ShouldCompact(*types.Conversation) bool { return f.shouldCompact }

func (f *failingStrategy) Compact(context.Context, *types.Conversation) (Result, error) {
	f.calls++
	return Result{}, f.err
}

// roundMarkers returns the "round-N" markers present in msgs.
func roundMarkers(msgs []types.Message) map[string]bool {
	markers := map[string]bool{}
	for _, m := range msgs {
		for i := 1; i <= 64; i++ {
			if strings.Contains(m.Content, fmt.Sprintf("round-%d ", i)) {
				markers[fmt.Sprintf("round-%d", i)] = true
			}
		}
	}
	return markers
}

// ─── SplitRounds ─────────────────────────────────────────────────────────────

func TestSplitRoundsGroupsByUserMessage(t *testing.T) {
	exactEstimator(t)

	msgs := append(protectedMessages(),
		types.Message{Role: types.RoleUser, Content: "round-1 q"},
		types.Message{Role: types.RoleAssistant, Content: "round-1 a"},
		types.Message{Role: types.RoleTool, Content: "round-1 tool"},
		types.Message{Role: types.RoleUser, Content: "round-2 q"},
		types.Message{Role: types.RoleAssistant, Content: "round-2 a"},
	)

	rounds := SplitRounds(msgs)
	if len(rounds) != 3 {
		t.Fatalf("SplitRounds returned %d rounds, want 3 (first user, round-1, round-2): %+v", len(rounds), rounds)
	}
	// The leading system message belongs to no round, so round 0 starts at 1.
	if rounds[0].Start != 1 || rounds[0].End != 2 {
		t.Errorf("rounds[0] = %+v, want {Start:1 End:2}", rounds[0])
	}
	if rounds[1].Start != 2 || rounds[1].End != 5 {
		t.Errorf("rounds[1] = %+v, want {Start:2 End:5}", rounds[1])
	}
	if rounds[2].Start != 5 || rounds[2].End != len(msgs) {
		t.Errorf("rounds[2] = %+v, want {Start:5 End:%d}", rounds[2], len(msgs))
	}
	if got := rounds[1].Len(); got != 3 {
		t.Errorf("rounds[1].Len() = %d, want 3 (assistant + its tool result stay together)", got)
	}
}

func TestSplitRoundsEdgeCases(t *testing.T) {
	exactEstimator(t)

	if rounds := SplitRounds(nil); rounds != nil {
		t.Errorf("SplitRounds(nil) = %+v, want nil", rounds)
	}
	// Only a system message: no rounds at all.
	if rounds := SplitRounds([]types.Message{{Role: types.RoleSystem, Content: "s"}}); len(rounds) != 0 {
		t.Errorf("SplitRounds(system only) = %+v, want none", rounds)
	}
	// A conversation that starts with an assistant message still forms a round.
	rounds := SplitRounds([]types.Message{
		{Role: types.RoleAssistant, Content: "a"},
		{Role: types.RoleUser, Content: "u"},
	})
	if len(rounds) != 2 || rounds[0].Start != 0 {
		t.Fatalf("SplitRounds = %+v, want two rounds starting at 0", rounds)
	}
}

// ─── Trim (gline watermark port) ────────────────────────────────────────────

// TestTrimKeepsProtectedMessagesAndRecentRounds pins the ported baseline: the
// system prompt and the original task survive, the oldest rounds are dropped
// first, and the most recent KeepRounds rounds stay verbatim.
func TestTrimKeepsProtectedMessagesAndRecentRounds(t *testing.T) {
	exactEstimator(t)

	msgs := append(protectedMessages(), fillerRounds(10, 80)...)
	conv := buildConv(2500, msgs...)

	// 10 rounds x 2 messages, each message ~95 tokens: ~1920 tokens total.
	if !conv.IsTokenAboveThreshold(60) {
		t.Fatalf("test setup: conversation should start above the 60%% watermark, have %d tokens", conv.GetTotalTokens())
	}

	strategy := &Trim{KeepRounds: 2}
	if !strategy.ShouldCompact(conv) {
		t.Fatal("ShouldCompact = false, want true above the low watermark")
	}
	if _, err := strategy.Compact(context.Background(), conv); err != nil {
		t.Fatalf("Compact returned error: %v", err)
	}

	after := conv.GetMessages()
	if after[0].Role != types.RoleSystem || after[0].Content != sysContent {
		t.Errorf("system message not preserved first: %+v", after[0])
	}
	if after[1].Content != taskContent {
		t.Errorf("first user message (the task) not preserved: %q", after[1].Content)
	}
	if after[2].Role != types.RoleUser || !strings.HasPrefix(after[2].Content, "[Context compacted:") {
		t.Fatalf("expected a user-role compaction notice at index 2, got %+v", after[2])
	}

	surviving := roundMarkers(after)
	for _, keep := range []string{"round-9", "round-10"} {
		if !surviving[keep] {
			t.Errorf("%s should be kept verbatim (inside the protected window)", keep)
		}
	}
	for _, dropped := range []string{"round-1", "round-2", "round-3", "round-5"} {
		if surviving[dropped] {
			t.Errorf("%s should have been dropped (oldest first)", dropped)
		}
	}
	if conv.GetTotalTokens() > conv.MaxTokens*60/100 {
		t.Errorf("conversation still above the low watermark after compaction: %d > %d",
			conv.GetTotalTokens(), conv.MaxTokens*60/100)
	}
}

// TestTrimBelowWatermarkIsNoOp pins the "don't trim when below the watermark"
// half of the baseline.
func TestTrimBelowWatermarkIsNoOp(t *testing.T) {
	exactEstimator(t)

	msgs := append(protectedMessages(), fillerRounds(4, 20)...)
	conv := buildConv(1_000_000, msgs...)
	before := conv.GetTotalTokens()

	strategy := NewTrim()
	if strategy.ShouldCompact(conv) {
		t.Fatal("ShouldCompact = true, want false below the low watermark")
	}
	if _, err := strategy.Compact(context.Background(), conv); err != nil {
		t.Fatalf("Compact returned error: %v", err)
	}
	if conv.GetTotalTokens() != before || len(conv.Messages) != len(msgs) {
		t.Fatalf("compaction below the watermark changed the conversation: %d->%d tokens, %d->%d messages",
			before, conv.GetTotalTokens(), len(msgs), len(conv.Messages))
	}
}

// TestTrimIsIdempotent pins "a second Compact on an already-compacted
// conversation is a no-op" from the Strategy contract.
func TestTrimIsIdempotent(t *testing.T) {
	exactEstimator(t)

	msgs := append(protectedMessages(), fillerRounds(10, 80)...)
	conv := buildConv(2500, msgs...)

	strategy := NewTrim()
	strategy.KeepRounds = 2
	if _, err := strategy.Compact(context.Background(), conv); err != nil {
		t.Fatalf("first Compact: %v", err)
	}
	after := make([]string, len(conv.Messages))
	for i, m := range conv.Messages {
		after[i] = fmt.Sprintf("%s:%s", m.Role, m.Content)
	}
	tokens := conv.GetTotalTokens()

	if _, err := strategy.Compact(context.Background(), conv); err != nil {
		t.Fatalf("second Compact: %v", err)
	}
	if conv.GetTotalTokens() != tokens {
		t.Errorf("second Compact changed the token count: %d -> %d", tokens, conv.GetTotalTokens())
	}
	for i, m := range conv.Messages {
		if i < len(after) && after[i] != fmt.Sprintf("%s:%s", m.Role, m.Content) {
			t.Errorf("second Compact changed message %d", i)
		}
	}
	if len(conv.Messages) != len(after) {
		t.Errorf("second Compact changed the message count: %d -> %d", len(after), len(conv.Messages))
	}
}

// TestTrimNothingCompactableFallsBackToHardTrim covers a conversation whose
// rounds all sit inside the protected window: round dropping cannot help, so
// the gline hard trim (Conversation.TrimToMaxTokens) takes over.
func TestTrimNothingCompactableFallsBackToHardTrim(t *testing.T) {
	exactEstimator(t)

	msgs := append(protectedMessages(),
		types.Message{Role: types.RoleUser, Content: strings.Repeat("z", 3000)},
		types.Message{Role: types.RoleAssistant, Content: "answer"},
	)
	msgs = append(msgs, fillerRounds(4, 20)...)
	conv := buildConv(2000, msgs...)
	before := conv.GetTotalTokens()

	strategy := &Trim{KeepRounds: 6}
	if _, err := strategy.Compact(context.Background(), conv); err != nil {
		t.Fatalf("Compact returned error: %v", err)
	}
	if conv.GetTotalTokens() >= before {
		t.Fatalf("hard trim did not shrink the conversation: %d -> %d tokens", before, conv.GetTotalTokens())
	}
	// The hard trim always leaves the system message plus at least two more.
	if len(conv.Messages) < 3 {
		t.Fatalf("hard trim dropped the protected messages: %d left", len(conv.Messages))
	}
	if conv.Messages[0].Role != types.RoleSystem {
		t.Errorf("hard trim removed the system message: %+v", conv.Messages[0])
	}
}

// TestTrimWatermarkConfiguration verifies the zero-value defaults and that
// explicit percentages are honored.
func TestTrimWatermarkConfiguration(t *testing.T) {
	exactEstimator(t)

	strategy := NewTrim()
	if strategy.lowWatermark() != DefaultLowWatermarkPercent || strategy.highWatermark() != DefaultHighWatermarkPercent {
		t.Errorf("NewTrim defaults = %d/%d, want %d/%d",
			strategy.lowWatermark(), strategy.highWatermark(),
			DefaultLowWatermarkPercent, DefaultHighWatermarkPercent)
	}
	if strategy.keepRounds() != DefaultKeepRounds {
		t.Errorf("NewTrim keepRounds = %d, want %d", strategy.keepRounds(), DefaultKeepRounds)
	}

	strategy.LowWatermarkPercent, strategy.HighWatermarkPercent, strategy.KeepRounds = 40, 90, 3
	if strategy.lowWatermark() != 40 || strategy.highWatermark() != 90 || strategy.keepRounds() != 3 {
		t.Error("explicit Trim configuration was not honored")
	}
}

// ─── Summarize ───────────────────────────────────────────────────────────────

func newSummarizeConv() *types.Conversation {
	msgs := append(protectedMessages(), fillerRounds(8, 80)...)
	return buildConv(2500, msgs...)
}

// TestSummarizeReplacesDroppedRounds pins the core Summarize behavior: the
// dropped rounds become one structured summary message, the system prompt, the
// original task and the most recent rounds stay verbatim, and the summarizer
// sees a transcript of exactly the dropped rounds.
func TestSummarizeReplacesDroppedRounds(t *testing.T) {
	exactEstimator(t)

	conv := newSummarizeConv()
	fake := &fakeSummarizer{summary: "Task goal: fix the login bug\nCompleted steps: none yet"}
	strategy := &Summarize{Summarizer: fake, KeepRounds: 2}

	if !strategy.ShouldCompact(conv) {
		t.Fatal("ShouldCompact = false, want true above the low watermark with compactable rounds")
	}
	if _, err := strategy.Compact(context.Background(), conv); err != nil {
		t.Fatalf("Compact returned error: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("summarizer called %d times, want 1", fake.calls)
	}

	after := conv.Messages
	if after[0].Role != types.RoleSystem || after[0].Content != sysContent {
		t.Errorf("system message not preserved first: %+v", after[0])
	}
	if after[1].Content != taskContent {
		t.Errorf("first user message (the task) not preserved: %q", after[1].Content)
	}
	if after[2].Role != types.RoleUser || !strings.HasPrefix(after[2].Content, "[COMPACTED]") {
		t.Fatalf("expected a user-role summary message at index 2, got %+v", after[2])
	}
	if !strings.Contains(after[2].Content, fake.summary) {
		t.Error("summary text missing from the replacement message")
	}
	if !strings.Contains(after[2].Content, "7 oldest round(s)") {
		t.Errorf("summary header should report the dropped round count, got: %.120s", after[2].Content)
	}

	surviving := roundMarkers(after)
	for _, keep := range []string{"round-7", "round-8"} {
		if !surviving[keep] {
			t.Errorf("%s should be kept verbatim (inside the protected window)", keep)
		}
	}
	for _, dropped := range []string{"round-1", "round-4", "round-6"} {
		if surviving[dropped] {
			t.Errorf("%s should have been summarized away", dropped)
		}
	}
	if conv.GetTotalTokens() > conv.MaxTokens*60/100 {
		t.Errorf("summary did not shrink the conversation below the low watermark: %d tokens", conv.GetTotalTokens())
	}
}

// TestSummarizeTranscriptContents checks what the summarizer is asked to
// summarize: only the dropped rounds, with tool calls and tool results labeled.
func TestSummarizeTranscriptContents(t *testing.T) {
	exactEstimator(t)

	conv := buildConv(2500, append(protectedMessages(),
		types.Message{Role: types.RoleUser, Content: "round-1 read the file"},
		types.Message{Role: types.RoleAssistant, Content: "reading",
			ToolCalls: []types.ToolCall{{ID: "call-1", Name: "read", Input: []byte(`{"path":"a.go"}`)}},
		},
		types.Message{Role: types.RoleTool, ToolCallID: "call-1", Content: "package main\nfunc main() {}"},
		types.Message{Role: types.RoleUser, Content: "round-2 go on"},
		types.Message{Role: types.RoleAssistant, Content: "ok"},
		types.Message{Role: types.RoleUser, Content: "round-3 done"},
		types.Message{Role: types.RoleAssistant, Content: "done"},
	)...)

	fake := &fakeSummarizer{summary: "summary"}
	strategy := &Summarize{Summarizer: fake, KeepRounds: 1}
	if _, err := strategy.Compact(context.Background(), conv); err != nil {
		t.Fatalf("Compact returned error: %v", err)
	}

	prompt := fake.prompts[0]
	if !strings.Contains(prompt, DefaultSummarizePrompt) {
		t.Error("the default summarization prompt should be included")
	}
	for _, want := range []string{
		"round-1 read the file",
		"round-2 go on",
		"[user]", "[assistant]",
		"-> tool_call read(",    // tool calls are labeled
		"[tool result of read]", // tool results are labeled with their tool name
		"func main() {}",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("transcript missing %q", want)
		}
	}
	// Only round-3 (the protected window) stays out of the transcript; the
	// first user message is preserved verbatim elsewhere and never summarized.
	if strings.Contains(prompt, "round-3") || strings.Contains(prompt, taskContent) {
		t.Error("transcript should not contain the protected round or the task message")
	}
	// The transcript comes after the instructions.
	if !strings.Contains(prompt[strings.Index(prompt, "Transcript"):], "round-1") {
		t.Error("the transcript should be appended after the prompt")
	}
}

// TestSummarizePromptOverride pins Config.SummarizePrompt → strategy.Prompt.
func TestSummarizePromptOverride(t *testing.T) {
	exactEstimator(t)

	conv := newSummarizeConv()
	fake := &fakeSummarizer{summary: "custom summary"}
	strategy := &Summarize{Summarizer: fake, Prompt: "CUSTOM PROMPT", KeepRounds: 2}
	if _, err := strategy.Compact(context.Background(), conv); err != nil {
		t.Fatalf("Compact returned error: %v", err)
	}
	if !strings.HasPrefix(fake.prompts[0], "CUSTOM PROMPT") {
		t.Errorf("prompt override ignored, got: %.60s", fake.prompts[0])
	}
	if strings.Contains(fake.prompts[0], DefaultSummarizePrompt) {
		t.Error("the default prompt should be replaced, not concatenated")
	}
}

// TestSummarizeNoCompactableRoundsNeverCallsLLM pins the cheap-trigger rule:
// when every round is protected the strategy stays silent.
func TestSummarizeNoCompactableRoundsNeverCallsLLM(t *testing.T) {
	exactEstimator(t)

	// One round only: dropping rounds cannot free anything, and the
	// budget-aware keep window has nothing to shrink.
	conv := buildConv(400,
		types.Message{Role: types.RoleSystem, Content: sysContent},
		types.Message{Role: types.RoleUser, Content: strings.Repeat("q", 300)},
		types.Message{Role: types.RoleAssistant, Content: strings.Repeat("a", 300)},
	)
	fake := &fakeSummarizer{summary: "should not be used"}
	strategy := &Summarize{Summarizer: fake}

	if strategy.ShouldCompact(conv) {
		t.Error("ShouldCompact = true with no compactable round, want false")
	}
	if _, err := strategy.Compact(context.Background(), conv); err != nil {
		t.Fatalf("Compact returned error: %v", err)
	}
	if fake.calls != 0 {
		t.Errorf("summarizer called %d times, want 0", fake.calls)
	}
	if len(conv.Messages) != 3 {
		t.Errorf("conversation was modified: %d messages", len(conv.Messages))
	}
}

// TestSummarizeFailuresFallBack pins every failure mode the Combo relies on.
func TestSummarizeFailuresFallBack(t *testing.T) {
	exactEstimator(t)

	t.Run("no summarizer", func(t *testing.T) {
		conv := newSummarizeConv()
		strategy := &Summarize{}
		if strategy.ShouldCompact(conv) {
			t.Error("ShouldCompact = true without a summarizer, want false")
		}
		_, err := strategy.Compact(context.Background(), conv)
		if !errors.Is(err, ErrNoSummarizer) {
			t.Errorf("err = %v, want ErrNoSummarizer", err)
		}
	})

	t.Run("empty summary", func(t *testing.T) {
		conv := newSummarizeConv()
		strategy := &Summarize{Summarizer: &fakeSummarizer{summary: "   \n  "}}
		_, err := strategy.Compact(context.Background(), conv)
		if !errors.Is(err, ErrEmptySummary) {
			t.Errorf("err = %v, want ErrEmptySummary", err)
		}
	})

	t.Run("provider error", func(t *testing.T) {
		conv := newSummarizeConv()
		strategy := &Summarize{Summarizer: &fakeSummarizer{err: errors.New("boom")}}
		_, err := strategy.Compact(context.Background(), conv)
		if err == nil || !strings.Contains(err.Error(), "summarizer failed") {
			t.Errorf("err = %v, want a wrapped summarizer failure", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		conv := newSummarizeConv()
		strategy := &Summarize{Summarizer: &fakeSummarizer{block: 500 * time.Millisecond}, Timeout: 20 * time.Millisecond}
		_, err := strategy.Compact(context.Background(), conv)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want context.DeadlineExceeded", err)
		}
	})
}

// TestSummarizeCaps pins the two output caps.
func TestSummarizeCaps(t *testing.T) {
	exactEstimator(t)

	t.Run("summary truncated to MaxSummaryTokens", func(t *testing.T) {
		conv := newSummarizeConv()
		long := strings.Repeat("s", 5000)
		strategy := &Summarize{Summarizer: &fakeSummarizer{summary: long}, KeepRounds: 2, MaxSummaryTokens: 100}
		if _, err := strategy.Compact(context.Background(), conv); err != nil {
			t.Fatalf("Compact returned error: %v", err)
		}
		if !strings.Contains(conv.Messages[2].Content, "[... summary truncated ...]") {
			t.Error("over-long summary was not truncated")
		}
		// The header plus the capped summary must stay far below the original
		// 5000-token summary.
		if got := types.Estimator(conv.Messages[2].Content); got > 600 {
			t.Errorf("summary message still too long: %d tokens", got)
		}
	})

	t.Run("transcript truncated to MaxTranscriptTokens", func(t *testing.T) {
		conv := newSummarizeConv()
		fake := &fakeSummarizer{summary: "summary"}
		strategy := &Summarize{Summarizer: fake, KeepRounds: 2, MaxTranscriptTokens: 300}
		if _, err := strategy.Compact(context.Background(), conv); err != nil {
			t.Fatalf("Compact returned error: %v", err)
		}
		if !strings.Contains(fake.prompts[0], "older message(s) omitted") {
			t.Error("transcript over budget did not report the omission")
		}
	})
}

// ─── Summarize: task-message duplication in non-user-first histories ────────

// TestSummarizeAssistantFirstHistoryDoesNotDuplicateTask pins the M4 review
// fix. A conversation whose first body message is not a user message (the
// pre-send hard trim removed the original turn, or the session was restored
// from the store) puts the task message at the start of the retained window:
// firstUser == cut. Before the fix the task was kept there AND re-inserted
// ahead of the summary, so it appeared twice:
//
//	[system, assistant, user(task), assistant] + KeepRounds=1
//	  → [system, user(task), [COMPACTED]…, user(task), assistant]
func TestSummarizeAssistantFirstHistoryDoesNotDuplicateTask(t *testing.T) {
	exactEstimator(t)

	conv := buildConv(2500,
		types.Message{Role: types.RoleSystem, Content: sysContent},
		types.Message{Role: types.RoleAssistant, Content: "intro assistant turn"},
		types.Message{Role: types.RoleUser, Content: taskContent},
		types.Message{Role: types.RoleAssistant, Content: "working on it"},
	)

	// The setup must genuinely sit on the boundary the fix guards: the task
	// message is the first message of the retained window (firstUser == cut).
	body := conv.Messages[1:]
	if cut := compactableCut(body, 1); firstUserMessage(body) != cut {
		t.Fatalf("test setup: expected firstUser == cut (%d), got %d", cut, firstUserMessage(body))
	}

	fake := &fakeSummarizer{summary: "Task goal: fix the login bug"}
	strategy := &Summarize{Summarizer: fake, KeepRounds: 1}
	if _, err := strategy.Compact(context.Background(), conv); err != nil {
		t.Fatalf("Compact returned error: %v", err)
	}

	after := conv.Messages
	want := []types.Message{
		{Role: types.RoleSystem, Content: sysContent},
		{Role: types.RoleUser, Content: "[COMPACTED] " + fake.summary},
		{Role: types.RoleUser, Content: taskContent},
		{Role: types.RoleAssistant, Content: "working on it"},
	}
	if len(after) != len(want) {
		t.Fatalf("compacted history has %d messages, want %d: %+v", len(after), len(want), after)
	}
	for i, w := range want {
		if after[i].Role != w.Role {
			t.Errorf("message %d role = %s, want %s", i, after[i].Role, w.Role)
		}
		if w.Role == types.RoleUser && strings.HasPrefix(w.Content, "[COMPACTED]") {
			if !strings.HasPrefix(after[i].Content, "[COMPACTED]") || !strings.Contains(after[i].Content, fake.summary) {
				t.Errorf("message %d = %.80q, want the summary message", i, after[i].Content)
			}
			continue
		}
		if after[i].Content != w.Content {
			t.Errorf("message %d = %q, want %q", i, after[i].Content, w.Content)
		}
	}

	if n := strings.Count(messagesText(after), taskContent); n != 1 {
		t.Errorf("task message appears %d times, want exactly 1", n)
	}
	if strings.Contains(messagesText(after), "intro assistant turn") {
		t.Error("the dropped assistant turn should only live in the transcript, not in the history")
	}
	if !strings.Contains(fake.prompts[0], "intro assistant turn") {
		t.Error("the dropped assistant turn should be part of the summarizer transcript")
	}
}

// TestSummarizeRestoredToolFirstHistoryDoesNotDuplicateTask covers the
// store-restore variant of the same boundary: the history opens with an
// assistant tool_call and its result, so the task message again sits at
// firstUser == cut. The dropped tool round must move into the transcript as a
// whole — the assistant tool_call and its tool result are never separated.
func TestSummarizeRestoredToolFirstHistoryDoesNotDuplicateTask(t *testing.T) {
	exactEstimator(t)

	conv := buildConv(2500,
		types.Message{Role: types.RoleSystem, Content: sysContent},
		types.Message{Role: types.RoleAssistant, Content: "reading the file",
			ToolCalls: []types.ToolCall{{ID: "call-1", Name: "read", Input: []byte(`{"path":"a.go"}`)}},
		},
		types.Message{Role: types.RoleTool, ToolCallID: "call-1", Content: "package main\nfunc main() {}"},
		types.Message{Role: types.RoleUser, Content: taskContent},
		types.Message{Role: types.RoleAssistant, Content: "working on it"},
	)

	fake := &fakeSummarizer{summary: "summary text"}
	strategy := &Summarize{Summarizer: fake, KeepRounds: 1}
	if _, err := strategy.Compact(context.Background(), conv); err != nil {
		t.Fatalf("Compact returned error: %v", err)
	}

	after := conv.Messages
	if len(after) != 4 {
		t.Fatalf("compacted history has %d messages, want 4: %+v", len(after), after)
	}
	if after[0].Role != types.RoleSystem || after[0].Content != sysContent {
		t.Errorf("system message not preserved first: %+v", after[0])
	}
	if after[1].Role != types.RoleUser || !strings.HasPrefix(after[1].Content, "[COMPACTED]") {
		t.Errorf("expected the summary message at index 1, got %+v", after[1])
	}
	if after[2].Role != types.RoleUser || after[2].Content != taskContent {
		t.Errorf("the task message must survive exactly once after the summary, got %+v", after[2])
	}
	if after[3].Role != types.RoleAssistant || after[3].Content != "working on it" {
		t.Errorf("the retained round must stay verbatim, got %+v", after[3])
	}
	if n := strings.Count(messagesText(after), taskContent); n != 1 {
		t.Errorf("task message appears %d times, want exactly 1", n)
	}
	// The tool call and its result were summarized together, never separated.
	for _, want := range []string{"-> tool_call read(", "func main() {}"} {
		if !strings.Contains(fake.prompts[0], want) {
			t.Errorf("transcript missing %q", want)
		}
	}
}

// TestSummarizeTaskInsideDroppedRangePreservedOnce covers the other side of
// the guard: with a non-user opening and several dropped rounds the task
// message sits strictly inside the dropped range (firstUser < cut) and must be
// re-inserted ahead of the summary — exactly once.
func TestSummarizeTaskInsideDroppedRangePreservedOnce(t *testing.T) {
	exactEstimator(t)

	conv := buildConv(2500,
		types.Message{Role: types.RoleSystem, Content: sysContent},
		types.Message{Role: types.RoleAssistant, Content: "intro assistant turn"},
		types.Message{Role: types.RoleUser, Content: taskContent},
		types.Message{Role: types.RoleAssistant, Content: "ack"},
		types.Message{Role: types.RoleUser, Content: "round-2 question"},
		types.Message{Role: types.RoleAssistant, Content: "round-2 answer"},
	)

	fake := &fakeSummarizer{summary: "summary text"}
	strategy := &Summarize{Summarizer: fake, KeepRounds: 1}
	if _, err := strategy.Compact(context.Background(), conv); err != nil {
		t.Fatalf("Compact returned error: %v", err)
	}

	after := conv.Messages
	want := []struct {
		role    types.Role
		content string
	}{
		{types.RoleSystem, sysContent},
		{types.RoleUser, taskContent},
		{types.RoleUser, "[COMPACTED]"},
		{types.RoleUser, "round-2 question"},
		{types.RoleAssistant, "round-2 answer"},
	}
	if len(after) != len(want) {
		t.Fatalf("compacted history has %d messages, want %d: %+v", len(after), len(want), after)
	}
	for i, w := range want {
		if after[i].Role != w.role {
			t.Errorf("message %d role = %s, want %s", i, after[i].Role, w.role)
		}
		if w.content != "[COMPACTED]" && after[i].Content != w.content {
			t.Errorf("message %d = %q, want %q", i, after[i].Content, w.content)
		}
		if w.content == "[COMPACTED]" && !strings.HasPrefix(after[i].Content, "[COMPACTED]") {
			t.Errorf("message %d = %.60q, want the summary message", i, after[i].Content)
		}
	}
	if n := strings.Count(messagesText(after), taskContent); n != 1 {
		t.Errorf("task message appears %d times, want exactly 1", n)
	}
}

// messagesText concatenates the message contents so occurrence counts can be
// asserted without caring which message a text landed in.
func messagesText(msgs []types.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// TestTrimAssistantFirstHistoryKeepsTaskOnce pins the same boundary on the
// Trim side: its round-dropping loop already guarded firstUser against cut,
// and this keeps that parity from regressing while Summarize's is fixed.
func TestTrimAssistantFirstHistoryKeepsTaskOnce(t *testing.T) {
	exactEstimator(t)

	msgs := []types.Message{
		{Role: types.RoleSystem, Content: sysContent},
		{Role: types.RoleAssistant, Content: "intro assistant turn"},
		{Role: types.RoleUser, Content: taskContent},
	}
	msgs = append(msgs, fillerRounds(6, 100)...)
	conv := buildConv(2000, msgs...)
	if !conv.IsTokenAboveThreshold(60) {
		t.Fatalf("test setup: conversation should start above the 60%% watermark, have %d tokens", conv.GetTotalTokens())
	}

	strategy := &Trim{KeepRounds: 1}
	if _, err := strategy.Compact(context.Background(), conv); err != nil {
		t.Fatalf("Compact returned error: %v", err)
	}

	after := conv.Messages
	if len(after) < 4 {
		t.Fatalf("compacted history too short: %+v", after)
	}
	if after[0].Role != types.RoleSystem || after[0].Content != sysContent {
		t.Errorf("system message not preserved first: %+v", after[0])
	}
	// The task message is protected even though its round was dropped: it is
	// re-inserted before the notice, and only once.
	if after[1].Role != types.RoleUser || after[1].Content != taskContent {
		t.Errorf("task message not preserved right after the system prompt: %+v", after[1])
	}
	if !strings.HasPrefix(after[2].Content, "[Context compacted:") {
		t.Errorf("expected the compaction notice at index 2, got %.80q", after[2].Content)
	}
	if n := strings.Count(messagesText(after), taskContent); n != 1 {
		t.Errorf("task message appears %d times, want exactly 1", n)
	}
	if strings.Contains(messagesText(after), "intro assistant turn") {
		t.Error("the assistant-only opening round should have been dropped")
	}
}

// ─── Combo ───────────────────────────────────────────────────────────────────

// TestComboFallsBackWhenSummarizationFails is the acceptance case: any
// summarization failure degrades to the watermark trim instead of failing the
// run.
func TestComboFallsBackWhenSummarizationFails(t *testing.T) {
	exactEstimator(t)

	for name, fake := range map[string]*fakeSummarizer{
		"provider error": {err: errors.New("provider down")},
		"empty summary":  {summary: "  "},
	} {
		t.Run(name, func(t *testing.T) {
			conv := newSummarizeConv()
			strategy := NewCombo(&Summarize{Summarizer: fake, KeepRounds: 2}, NewTrim())
			if _, err := strategy.Compact(context.Background(), conv); err != nil {
				t.Fatalf("Combo returned error: %v", err)
			}
			if !strings.Contains(conv.Messages[2].Content, "[Context compacted:") {
				t.Fatalf("expected the Trim notice after fallback, got: %.120s", conv.Messages[2].Content)
			}
			// The default Trim keeps 6 rounds verbatim, so with this small
			// test budget it may legitimately stop between the two watermarks:
			// the hard line (80%) is what compaction must always get under.
			if conv.GetTotalTokens() > conv.MaxTokens*80/100 {
				t.Errorf("fallback did not free the budget below the hard line: %d tokens", conv.GetTotalTokens())
			}
		})
	}
}

// TestComboSkipsFallbackWhenPrimaryIsEnough: after a successful summary the
// conversation is below Trim's watermark, so the fallback must not run.
func TestComboSkipsFallbackWhenPrimaryIsEnough(t *testing.T) {
	exactEstimator(t)

	conv := newSummarizeConv()
	fake := &fakeSummarizer{summary: "Task goal: fix the login bug"}
	strategy := NewCombo(&Summarize{Summarizer: fake, KeepRounds: 2}, NewTrim())
	if _, err := strategy.Compact(context.Background(), conv); err != nil {
		t.Fatalf("Combo returned error: %v", err)
	}
	if strings.Contains(conv.Messages[2].Content, "[Context compacted:") {
		t.Error("the Trim fallback ran although the summary freed the budget")
	}
	if !strings.Contains(conv.Messages[2].Content, fake.summary) {
		t.Error("the summary message is missing")
	}
}

// TestComboRunsFallbackWhenPrimaryLeavesItOver: a summary that is too big to
// free the budget makes the Combo run the trim as well.
func TestComboRunsFallbackWhenPrimaryLeavesItOver(t *testing.T) {
	exactEstimator(t)

	conv := newSummarizeConv()
	fake := &fakeSummarizer{summary: strings.Repeat("b", 4000)}
	strategy := NewCombo(
		&Summarize{Summarizer: fake, KeepRounds: 2, MaxSummaryTokens: 100000},
		NewTrim(),
	)
	if _, err := strategy.Compact(context.Background(), conv); err != nil {
		t.Fatalf("Combo returned error: %v", err)
	}
	if conv.GetTotalTokens() > conv.MaxTokens*60/100 {
		t.Errorf("fallback trim did not free the budget: %d tokens", conv.GetTotalTokens())
	}
}

// TestComboBothFailReturnsJoinedError pins the error contract: the loop logs
// it and keeps going, so it must name both strategies.
func TestComboBothFailReturnsJoinedError(t *testing.T) {
	exactEstimator(t)

	conv := newSummarizeConv()
	primaryErr := errors.New("no summarizer")
	fallbackErr := errors.New("trim exploded")
	strategy := NewCombo(
		&failingStrategy{shouldCompact: true, err: primaryErr},
		&failingStrategy{shouldCompact: true, err: fallbackErr},
	)
	_, err := strategy.Compact(context.Background(), conv)
	if err == nil || !strings.Contains(err.Error(), primaryErr.Error()) || !strings.Contains(err.Error(), fallbackErr.Error()) {
		t.Fatalf("err = %v, want both failures joined", err)
	}
}

// TestComboShouldCompact verifies the OR semantics of the trigger check.
func TestComboShouldCompact(t *testing.T) {
	exactEstimator(t)

	conv := buildConv(1_000_000, append(protectedMessages(), fillerRounds(2, 10)...)...)

	if NewCombo(nil, nil).ShouldCompact(conv) {
		t.Error("empty combo should never want to compact")
	}
	if NewCombo(&failingStrategy{}, &failingStrategy{}).ShouldCompact(conv) {
		t.Error("combo of non-triggering strategies should not want to compact")
	}
	if !NewCombo(&failingStrategy{shouldCompact: true}, nil).ShouldCompact(conv) {
		t.Error("a triggering primary alone should be enough")
	}
	if !NewCombo(nil, &failingStrategy{shouldCompact: true}).ShouldCompact(conv) {
		t.Error("a triggering fallback alone should be enough")
	}
}

// TestDefaultComboIsSummarizeThenTrim pins the default assembly documented on
// Config.Compaction and loop.Options.Compaction.
func TestDefaultComboIsSummarizeThenTrim(t *testing.T) {
	combo := NewCombo(&Summarize{Summarizer: &fakeSummarizer{summary: "s"}}, NewTrim())
	if _, ok := combo.Primary.(*Summarize); !ok {
		t.Errorf("default primary = %T, want *Summarize", combo.Primary)
	}
	if _, ok := combo.Fallback.(*Trim); !ok {
		t.Errorf("default fallback = %T, want *Trim", combo.Fallback)
	}
}

// ─── anchorable Trim (persistence) ───────────────────────────────────────────

// TestTrimResultIsAnchorable pins the persisted shape: Trim leaves exactly the
// three parts a compaction row can describe — protected prefix, one notice
// row, verbatim tail — and reports where the tail starts plus which rows were
// kept ahead of it. Without that a reload restored every dropped round.
func TestTrimResultIsAnchorable(t *testing.T) {
	exactEstimator(t)

	msgs := append(protectedMessages(), fillerRounds(10, 80)...)
	conv := buildConv(2500, msgs...)
	pre := conv.GetMessages()
	preLen := len(pre)

	strategy := &Trim{KeepRounds: 2}
	res, err := strategy.Compact(context.Background(), conv)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !res.Anchorable {
		t.Fatalf("Trim result is not anchorable: %+v", res)
	}
	if res.SummaryRole != types.RoleUser || !strings.HasPrefix(res.Summary, "[Context compacted:") {
		t.Errorf("summary = %q (role %q), want the user-role notice", res.Summary, res.SummaryRole)
	}
	if res.KeptFromIndex <= 0 || res.KeptFromIndex >= preLen {
		t.Fatalf("KeptFromIndex = %d, want a tail start inside (0,%d)", res.KeptFromIndex, preLen)
	}
	// Prefix indices are the leading system message (and the task message when
	// it fell inside the dropped range).
	if len(res.KeptPrefixIndices) == 0 || res.KeptPrefixIndices[0] != 0 {
		t.Errorf("KeptPrefixIndices = %v, want the system row first", res.KeptPrefixIndices)
	}
	for _, idx := range res.KeptPrefixIndices {
		if idx < 0 || idx >= preLen {
			t.Fatalf("prefix index %d outside the pre-compaction conversation", idx)
		}
	}
	// The rebuilt view must equal what Compact left in memory: prefix + notice
	// + tail(pre[KeptFromIndex:]).
	rebuilt := make([]types.Message, 0, preLen)
	for _, idx := range res.KeptPrefixIndices {
		rebuilt = append(rebuilt, pre[idx])
	}
	rebuilt = append(rebuilt, types.Message{Role: res.SummaryRole, Content: res.Summary})
	rebuilt = append(rebuilt, pre[res.KeptFromIndex:]...)
	after := conv.GetMessages()
	if len(rebuilt) != len(after) {
		t.Fatalf("rebuild = %d messages, live = %d", len(rebuilt), len(after))
	}
	for i := range rebuilt {
		if rebuilt[i].Role != after[i].Role || rebuilt[i].Content != after[i].Content {
			t.Fatalf("message %d differs: rebuilt %q, live %q", i, rebuilt[i].Content, after[i].Content)
		}
	}
}

// TestTrimShouldCompactNeedsSomethingToDrop pins the anti-storm rule: in the
// 60–80% band a conversation whose rounds are all inside the protected window
// is not compactable, because dropping rounds cannot free anything there.
func TestTrimShouldCompactNeedsSomethingToDrop(t *testing.T) {
	exactEstimator(t)

	// A single round, ~65% of the window: above the low watermark, below the
	// high one. There is no other round to drop and no tool result to shrink.
	msgs := append(protectedMessages()[:1],
		types.Message{Role: types.RoleUser, Content: strings.Repeat("q", 1600)},
		types.Message{Role: types.RoleAssistant, Content: strings.Repeat("a", 100)},
	)
	conv := buildConv(2500, msgs...)
	if !conv.IsTokenAboveThreshold(60) || conv.GetTotalTokens() > conv.MaxTokens*80/100 {
		t.Fatalf("test setup: want 60%%..80%%, have %d/%d", conv.GetTotalTokens(), conv.MaxTokens)
	}
	if (&Trim{}).ShouldCompact(conv) {
		t.Error("ShouldCompact = true although no round can be dropped")
	}

	// Above the hard line the hard trim still has work, so it is compactable.
	conv.MaxTokens = 900
	if !(&Trim{}).ShouldCompact(conv) {
		t.Error("ShouldCompact = false above the high watermark, want true (hard trim)")
	}
}

// TestComboKeepsPrimaryAnchorWhenFallbackOnlyShrinks pins the persistence
// hole: a fallback that merely shrinks content in place (Trim's hard trim)
// keeps the primary's message shape, so the primary's anchor must survive.
func TestComboKeepsPrimaryAnchorWhenFallbackOnlyShrinks(t *testing.T) {
	exactEstimator(t)

	// A summary small enough to free the budget, but a fallback forced to run
	// (it reports compactable) that changes nothing about the rows.
	conv := newSummarizeConv()
	fake := &fakeSummarizer{summary: "Task goal: fix the login bug"}
	strategy := NewCombo(&Summarize{Summarizer: fake, KeepRounds: 2}, &noRowChangeTrim{})
	res, err := strategy.Compact(context.Background(), conv)
	if err != nil {
		t.Fatalf("Combo: %v", err)
	}
	if !res.Anchorable {
		t.Fatalf("result is not anchorable: %+v", res)
	}
	if !strings.Contains(res.Summary, fake.summary) {
		t.Errorf("summary = %q, want the primary summary", res.Summary)
	}
}

// noRowChangeTrim is a fallback that reports "compactable" and drops no rows,
// standing in for Trim's in-place hard trim.
type noRowChangeTrim struct{}

func (noRowChangeTrim) ShouldCompact(*types.Conversation) bool { return true }

func (noRowChangeTrim) Compact(_ context.Context, conv *types.Conversation) (Result, error) {
	return Result{}, nil
}

// TestTrimKeepWindowShrinksToFit pins the budget-aware keep window: when the
// newest rounds alone exceed the low watermark, keeping them verbatim would
// make the compaction a no-op — the conversation would sit permanently above
// the watermark and every iteration would trim, hard-trim and trim again. The
// window shrinks instead, so one compaction is enough to get back under the
// low watermark.
func TestTrimKeepWindowShrinksToFit(t *testing.T) {
	exactEstimator(t)

	if got := keepWindow(nil, nil, 6, 0, 100); got != 6 {
		t.Errorf("keepWindow on an empty transcript = %d, want the configured 6", got)
	}
	// A single round can never be dropped: the keep window collapses to 0.
	single := SplitRounds(fillerRounds(1, 10))
	if got := keepWindow(fillerRounds(1, 10), single, 6, 0, 10); got != 0 {
		t.Errorf("keepWindow with one round = %d, want 0", got)
	}
	// Rounds too big to fit: the window shrinks to the newest one.
	body := fillerRounds(10, 100) // ~200 tokens per round
	rounds := SplitRounds(body)
	if got := keepWindow(body, rounds, 6, 0, 300); got != 1 {
		t.Errorf("keepWindow with oversized rounds = %d, want 1", got)
	}
	// Rounds that fit keep the configured window, capped so something is left
	// to drop.
	if got := keepWindow(body, rounds, 6, 0, 100000); got != 6 {
		t.Errorf("keepWindow when everything fits = %d, want 6", got)
	}

	// End to end: 3 huge rounds in a small window. KeepRounds 6 would protect
	// all of them (no compaction at all); the budget-aware window shrinks so the
	// older rounds can go instead of the transcript being pinned above the
	// watermark forever.
	msgs := append(protectedMessages(), fillerRounds(3, 400)...)
	conv := buildConv(1000, msgs...)
	if conv.GetTotalTokens() <= conv.MaxTokens*60/100 {
		t.Fatalf("test setup: want above the low watermark, have %d/%d", conv.GetTotalTokens(), conv.MaxTokens)
	}
	before := conv.GetTotalTokens()
	res, err := (&Trim{}).Compact(context.Background(), conv)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !res.Anchorable {
		t.Error("Trim result is not anchorable; the reduction could not be persisted")
	}
	if conv.GetTotalTokens() >= before {
		t.Errorf("compaction freed nothing: %d -> %d", before, conv.GetTotalTokens())
	}
	joined := ""
	for _, m := range conv.GetMessages() {
		joined += m.Content
	}
	if strings.Contains(joined, "round-1 ") || strings.Contains(joined, "round-2 ") {
		t.Errorf("old rounds survived the budget-aware compaction: %.200s", joined)
	}
	if !strings.Contains(joined, "round-3 ") {
		t.Error("the newest round must never be dropped")
	}
}

// TestLowWatermarkPercent verifies the exported watermark report the agent
// loop uses for its pre-send trigger: an injected strategy's percentage must
// reach the loop (a host asking for 80% was silently gated at the 60%
// default), a Combo reports its earliest part, and unknown strategies fall
// back to the default.
func TestLowWatermarkPercent(t *testing.T) {
	if got := LowWatermarkPercent(nil); got != DefaultLowWatermarkPercent {
		t.Errorf("nil strategy = %d, want %d", got, DefaultLowWatermarkPercent)
	}
	if got := LowWatermarkPercent(NewTrim()); got != DefaultLowWatermarkPercent {
		t.Errorf("zero-value Trim = %d, want %d", got, DefaultLowWatermarkPercent)
	}
	if got := LowWatermarkPercent(&Summarize{LowWatermarkPercent: 80}); got != 80 {
		t.Errorf("Summarize@80 = %d, want 80", got)
	}
	if got := LowWatermarkPercent(NewCombo(
		&Summarize{LowWatermarkPercent: 80},
		&Trim{LowWatermarkPercent: 80},
	)); got != 80 {
		t.Errorf("Combo(80,80) = %d, want 80", got)
	}
	if got := LowWatermarkPercent(NewCombo(
		&Summarize{LowWatermarkPercent: 80},
		NewTrim(),
	)); got != DefaultLowWatermarkPercent {
		t.Errorf("Combo(80, default) = %d, want %d (earliest part)", got, DefaultLowWatermarkPercent)
	}
	if got := LowWatermarkPercent(NewCombo(&Summarize{LowWatermarkPercent: 70}, nil)); got != 70 {
		t.Errorf("Combo(70, nil) = %d, want 70", got)
	}
}

// TestComboFallbackResultStaysInPreCompactionCoordinates pins the index
// contract of Result: KeptFromIndex / KeptPrefixIndices always address the
// PRE-compaction transcript the host snapshotted.
//
// The failure mode this guards is the desktop's "compaction completed, then
// the history came back": Combo runs Summarize (which REPLACES the
// conversation with a much shorter one) and then a fallback that drops
// further rounds on top of that shorter transcript. The fallback's indices
// address the POST-summarize shape; persisted as-is against the caller's
// pre-compaction snapshot the anchor lands far too early and restores rows
// the summarize had just dropped.
//
// The pinned contract: once the primary has rewritten the conversation, the
// result Combo returns must be byte-identical to the primary running alone.
func TestComboFallbackResultStaysInPreCompactionCoordinates(t *testing.T) {
	exactEstimator(t)

	// Budget tuned so the primary (Summarize) succeeds but leaves the
	// conversation above the fallback's watermark, and the fallback then has
	// to drop rounds of its own — the two shapes diverge, which is the point.
	msgs := protectedMessages()
	for i := 1; i <= 10; i++ {
		msgs = append(msgs, roundMessages(i, 100)...)
	}

	primary := func() *Summarize {
		return &Summarize{
			Summarizer:          &fakeSummarizer{summary: strings.Repeat("s", 300)},
			LowWatermarkPercent: 80,
			KeepRounds:          6,
			MaxSummaryTokens:    100000,
		}
	}

	// Reference run: the primary alone, on an identical conversation.
	soloConv := buildConv(1000, msgs...)
	preLen := len(soloConv.Messages)
	soloRes, err := primary().Compact(context.Background(), soloConv)
	if err != nil {
		t.Fatalf("Summarize alone: %v", err)
	}
	if !soloRes.Anchorable {
		t.Fatalf("primary alone is not anchorable: %+v", soloRes)
	}
	soloRows := len(soloConv.Messages)

	// Combo run.
	conv := buildConv(1000, msgs...)
	res, err := NewCombo(primary(), NewTrim()).Compact(context.Background(), conv)
	if err != nil {
		t.Fatalf("Combo: %v", err)
	}
	if !res.Anchorable {
		t.Fatalf("Combo result is not anchorable: %+v", res)
	}
	if len(conv.Messages) >= soloRows {
		t.Fatalf("scenario is not what the test claims: fallback did not drop rows (primary alone=%d rows, combo=%d rows)",
			soloRows, len(conv.Messages))
	}
	if res.Summary != soloRes.Summary || res.SummaryRole != soloRes.SummaryRole {
		t.Errorf("Combo summary diverged from the primary's: %q vs %q", res.Summary, soloRes.Summary)
	}
	if res.KeptFromIndex != soloRes.KeptFromIndex {
		t.Fatalf("KeptFromIndex = %d, want %d (the primary's pre-compaction anchor)",
			res.KeptFromIndex, soloRes.KeptFromIndex)
	}
	if strings.Join(intsToStrings(res.KeptPrefixIndices), ",") != strings.Join(intsToStrings(soloRes.KeptPrefixIndices), ",") {
		t.Errorf("prefix indices = %v, want %v", res.KeptPrefixIndices, soloRes.KeptPrefixIndices)
	}
	// PreLen is the length of the transcript those indices address.
	if res.PreLen != preLen || soloRes.PreLen != preLen {
		t.Fatalf("PreLen = %d (primary alone %d), want %d", res.PreLen, soloRes.PreLen, preLen)
	}
	if res.KeptFromIndex <= 0 || res.KeptFromIndex >= preLen {
		t.Fatalf("anchor %d outside the pre-compaction transcript (len=%d)", res.KeptFromIndex, preLen)
	}
	for _, idx := range res.KeptPrefixIndices {
		if idx < 0 || idx >= preLen {
			t.Fatalf("prefix index %d out of pre-compaction range (len=%d)", idx, preLen)
		}
	}
}

func intsToStrings(in []int) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = fmt.Sprint(v)
	}
	return out
}

// TestCompactionFreesTheWindowByTokensNotByRounds pins the treadmill that
// makes a long desktop session look frozen at ~100% of its context window.
//
// A tool-heavy session produces rounds that are individually enormous (a file
// dump, a test log). With a round-count window the strategy happily keeps the
// newest six — which is the entire context window — so it can never free
// anything, and the loop either re-runs it on every iteration or falls back to
// the ephemeral hard trim. The session then sits at the window limit forever
// and the model's visible context never shrinks, no matter how many times the
// UI announces a compaction.
//
// pi sizes the kept window in tokens (keepRecentTokens ≈ 20k) instead of
// rounds; the same compaction then drops ~85% of the window. This test pins
// that property for both built-in strategies.
func TestCompactionFreesTheWindowByTokensNotByRounds(t *testing.T) {
	exactEstimator(t)

	// 7 rounds of ~20k tokens each in a 128k window: the newest six alone are
	// 120k — 94% of the window, well over the 80% watermark.
	const roundTokens = 10000
	hugeRounds := func(n int) []types.Message {
		msgs := protectedMessages()
		for i := 1; i <= n; i++ {
			msgs = append(msgs, types.Message{Role: types.RoleUser, Content: fmt.Sprintf("round-%d ", i) + strings.Repeat("u", roundTokens)})
			msgs = append(msgs, types.Message{Role: types.RoleAssistant, Content: fmt.Sprintf("answer-%d ", i) + strings.Repeat("a", roundTokens)})
		}
		return msgs
	}

	t.Run("summarize", func(t *testing.T) {
		conv := buildConv(128000, hugeRounds(7)...)
		before := conv.GetTotalTokens()
		if before < 128000 {
			t.Fatalf("fixture is not over the window: %d", before)
		}
		strategy := &Summarize{Summarizer: &fakeSummarizer{summary: "Task goal: keep going"}}
		res, err := strategy.Compact(context.Background(), conv)
		if err != nil {
			t.Fatalf("Summarize: %v", err)
		}
		if !res.Anchorable {
			t.Fatalf("summary result is not anchorable")
		}
		after := conv.GetTotalTokens()
		if after > conv.MaxTokens/4 {
			t.Fatalf("summarize left %d/%d tokens (%.0f%% of the window): the kept window is sized by round count, "+
				"so the compaction cannot free the session", after, conv.MaxTokens, 100*float64(after)/float64(conv.MaxTokens))
		}
		// The newest round must survive verbatim: it is the working state.
		if !strings.Contains(lastContent(conv), "answer-7") {
			t.Fatalf("the newest round did not survive: %q", lastContent(conv))
		}
	})

	t.Run("trim", func(t *testing.T) {
		conv := buildConv(128000, hugeRounds(7)...)
		strategy := &Trim{}
		if _, err := strategy.Compact(context.Background(), conv); err != nil {
			t.Fatalf("Trim: %v", err)
		}
		if after := conv.GetTotalTokens(); after > conv.MaxTokens/4 {
			t.Fatalf("trim left %d/%d tokens: kept window is sized by round count", after, conv.MaxTokens)
		}
	})
}

// lastContent returns the content of the conversation's last message.
func lastContent(conv *types.Conversation) string {
	if len(conv.Messages) == 0 {
		return ""
	}
	return conv.Messages[len(conv.Messages)-1].Content
}

// TestSummarizeTranscriptKeepsThePreviousSummary pins the multi-compaction
// case: the carried-forward summary of the previous compaction sits at the
// FRONT of the transcript, so a budget cut taken from the front (oldest first)
// would delete it — each further compaction would then be strictly lossier
// than the last, and after a few rounds the model has no record of the session
// at all (the "it keeps compacting but the model still forgets everything"
// symptom). pi keeps it in a <previous-summary> block; here it is reserved
// before the newest messages are fitted into the rest of the budget.
func TestSummarizeTranscriptKeepsThePreviousSummary(t *testing.T) {
	exactEstimator(t)

	prev := "[COMPACTED] Early conversation summarized: the 4 oldest round(s) (10 messages) were replaced by the summary below. " +
		"Treat it as the record of what already happened.\n\nTask goal: keep the desktop agent honest about its token budget"
	msgs := append([]types.Message{{Role: types.RoleUser, Content: prev}}, fillerRounds(6, 4000)...)

	out := renderTranscript(msgs, 6000) // far below the transcript size
	if !strings.Contains(out, "Task goal: keep the desktop agent honest") {
		t.Fatalf("the carried-forward summary was dropped from the transcript:\n%s", out)
	}
	if !strings.Contains(out, "round-6 ") {
		t.Fatalf("the newest round is missing from the transcript:\n%s", out)
	}
	if strings.Contains(out, "round-1 ") {
		t.Fatalf("the oldest round should have been omitted:\n%s", out)
	}
	if !strings.Contains(out, "older message(s) omitted") {
		t.Fatalf("the omission marker is missing:\n%s", out)
	}
}
