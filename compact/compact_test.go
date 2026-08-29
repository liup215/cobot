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

func (f *failingStrategy) Compact(context.Context, *types.Conversation) error {
	f.calls++
	return f.err
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
	if err := strategy.Compact(context.Background(), conv); err != nil {
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
	if err := strategy.Compact(context.Background(), conv); err != nil {
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
	if err := strategy.Compact(context.Background(), conv); err != nil {
		t.Fatalf("first Compact: %v", err)
	}
	after := make([]string, len(conv.Messages))
	for i, m := range conv.Messages {
		after[i] = fmt.Sprintf("%s:%s", m.Role, m.Content)
	}
	tokens := conv.GetTotalTokens()

	if err := strategy.Compact(context.Background(), conv); err != nil {
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
	if err := strategy.Compact(context.Background(), conv); err != nil {
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
	if err := strategy.Compact(context.Background(), conv); err != nil {
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
			ToolCalls: []types.ToolCall{{ID: "call-1", Name: "read_file", Input: []byte(`{"path":"a.go"}`)}},
		},
		types.Message{Role: types.RoleTool, ToolCallID: "call-1", Content: "package main\nfunc main() {}"},
		types.Message{Role: types.RoleUser, Content: "round-2 go on"},
		types.Message{Role: types.RoleAssistant, Content: "ok"},
		types.Message{Role: types.RoleUser, Content: "round-3 done"},
		types.Message{Role: types.RoleAssistant, Content: "done"},
	)...)

	fake := &fakeSummarizer{summary: "summary"}
	strategy := &Summarize{Summarizer: fake, KeepRounds: 1}
	if err := strategy.Compact(context.Background(), conv); err != nil {
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
		"-> tool_call read_file(",    // tool calls are labeled
		"[tool result of read_file]", // tool results are labeled with their tool name
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
	if err := strategy.Compact(context.Background(), conv); err != nil {
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

	conv := buildConv(400, append(protectedMessages(),
		types.Message{Role: types.RoleUser, Content: strings.Repeat("q", 200)},
		types.Message{Role: types.RoleAssistant, Content: strings.Repeat("a", 200)},
	)...)
	fake := &fakeSummarizer{summary: "should not be used"}
	strategy := &Summarize{Summarizer: fake}

	if strategy.ShouldCompact(conv) {
		t.Error("ShouldCompact = true with no compactable round, want false")
	}
	if err := strategy.Compact(context.Background(), conv); err != nil {
		t.Fatalf("Compact returned error: %v", err)
	}
	if fake.calls != 0 {
		t.Errorf("summarizer called %d times, want 0", fake.calls)
	}
	if len(conv.Messages) != 4 {
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
		err := strategy.Compact(context.Background(), conv)
		if !errors.Is(err, ErrNoSummarizer) {
			t.Errorf("err = %v, want ErrNoSummarizer", err)
		}
	})

	t.Run("empty summary", func(t *testing.T) {
		conv := newSummarizeConv()
		strategy := &Summarize{Summarizer: &fakeSummarizer{summary: "   \n  "}}
		err := strategy.Compact(context.Background(), conv)
		if !errors.Is(err, ErrEmptySummary) {
			t.Errorf("err = %v, want ErrEmptySummary", err)
		}
	})

	t.Run("provider error", func(t *testing.T) {
		conv := newSummarizeConv()
		strategy := &Summarize{Summarizer: &fakeSummarizer{err: errors.New("boom")}}
		err := strategy.Compact(context.Background(), conv)
		if err == nil || !strings.Contains(err.Error(), "summarizer failed") {
			t.Errorf("err = %v, want a wrapped summarizer failure", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		conv := newSummarizeConv()
		strategy := &Summarize{Summarizer: &fakeSummarizer{block: 500 * time.Millisecond}, Timeout: 20 * time.Millisecond}
		err := strategy.Compact(context.Background(), conv)
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
		if err := strategy.Compact(context.Background(), conv); err != nil {
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
		if err := strategy.Compact(context.Background(), conv); err != nil {
			t.Fatalf("Compact returned error: %v", err)
		}
		if !strings.Contains(fake.prompts[0], "older message(s) omitted") {
			t.Error("transcript over budget did not report the omission")
		}
	})
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
			if err := strategy.Compact(context.Background(), conv); err != nil {
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
	if err := strategy.Compact(context.Background(), conv); err != nil {
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
	if err := strategy.Compact(context.Background(), conv); err != nil {
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
	err := strategy.Compact(context.Background(), conv)
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
