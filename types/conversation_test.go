package types

import (
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name  string
		input string
		min   int
		max   int
	}{
		{"empty", "", 0, 0},
		{"short ascii", "hello world", 2, 6},
		{"cjk", "你好世界", 4, 4},
		{"mixed", "hello 你好", 4, 8},
		{"long text", string(make([]byte, 400)), 100, 134},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HeuristicEstimator(tt.input)
			if got < tt.min || got > tt.max {
				t.Errorf("HeuristicEstimator(%q) = %d, want in [%d,%d]", tt.input, got, tt.min, tt.max)
			}
		})
	}
}

func TestConversationAddAndCount(t *testing.T) {
	c := NewConversation()
	if c.MaxTokens != 128000 {
		t.Errorf("default MaxTokens = %d, want 128000", c.MaxTokens)
	}

	c.AddMessage(Message{Role: RoleUser, Content: "hello world"})
	c.AddMessage(Message{Role: RoleAssistant, Content: "hi there"})
	if c.MessageCount() != 2 {
		t.Fatalf("MessageCount = %d, want 2", c.MessageCount())
	}
	if c.CurrentTokens <= 0 {
		t.Errorf("CurrentTokens = %d, want > 0", c.CurrentTokens)
	}
	if c.GetTotalTokens() != c.CurrentTokens {
		t.Errorf("GetTotalTokens = %d, want %d (no actual tokens set)", c.GetTotalTokens(), c.CurrentTokens)
	}
}

func TestConversationActualTokensPriority(t *testing.T) {
	c := NewConversation()
	c.AddMessage(Message{Role: RoleUser, Content: "hello"})
	c.AddActualTokens(500, 100)
	in, out := c.GetActualTokens()
	if in != 500 || out != 100 {
		t.Fatalf("GetActualTokens = (%d,%d), want (500,100)", in, out)
	}
	if got := c.GetTotalTokens(); got != 600 {
		t.Errorf("GetTotalTokens = %d, want 500+100=600", got)
	}
	c.ResetActualTokens()
	if got := c.GetTotalTokens(); got != c.CurrentTokens {
		t.Errorf("after reset, GetTotalTokens = %d, want estimate %d", got, c.CurrentTokens)
	}
}

func TestConversationPerMessageSoftCap(t *testing.T) {
	c := NewConversation()
	c.PerMessageSoftCap = 100

	long := ""
	for i := 0; i < 200; i++ {
		long += "word " // 1000 chars ≈ 250+ tokens
	}
	c.AddMessage(Message{Role: RoleUser, Content: long})
	msg := c.Messages[0]
	if len(msg.Content) >= len(long) {
		t.Fatalf("expected truncation, got len=%d vs original=%d", len(msg.Content), len(long))
	}
	if !contains(msg.Content, "tokens omitted") {
		t.Errorf("expected omission marker in %q", msg.Content)
	}
	if !contains(msg.Content, "word") {
		t.Errorf("head/tail should be preserved in %q", msg.Content)
	}
}

func TestConversationTrimToMaxTokens(t *testing.T) {
	c := NewConversation()
	c.MaxTokens = 60
	big := strings.Repeat("lorem ipsum dolor sit amet ", 8)
	c.AddMessage(Message{Role: RoleSystem, Content: "system prompt"})
	c.AddMessage(Message{Role: RoleUser, Content: big})
	c.AddMessage(Message{Role: RoleAssistant, Content: big})
	c.AddMessage(Message{Role: RoleUser, Content: "final user message"})
	before := c.MessageCount()
	c.TrimToMaxTokens()
	if c.MessageCount() >= before {
		t.Errorf("expected messages to be trimmed, before=%d after=%d", before, c.MessageCount())
	}
	if c.Messages[0].Role != RoleSystem {
		t.Errorf("system prompt must be preserved, first role = %s", c.Messages[0].Role)
	}
}

func TestConversationTrimPreservesToolHint(t *testing.T) {
	c := NewConversation()
	c.MaxTokens = 80
	c.AddMessage(Message{Role: RoleSystem, Content: "sys"})
	big := make([]byte, 1000)
	for i := range big {
		big[i] = 'x'
	}
	c.AddMessage(Message{Role: RoleUser, Content: "task"})
	c.AddMessage(Message{Role: RoleTool, ToolCallID: "call_1", Content: string(big)})
	c.AddMessage(Message{Role: RoleAssistant, Content: "ok"})
	c.AddMessage(Message{Role: RoleUser, Content: "next"})

	c.TrimToMaxTokens()

	found := false
	for _, m := range c.Messages {
		if m.Role == RoleTool && contains(m.Content, "truncated by context manager") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a tool placeholder hint after trimming, messages=%d", c.MessageCount())
	}
}

func TestConversationAutoCompactWatermarks(t *testing.T) {
	c := NewConversation()
	c.MaxTokens = 1000
	c.AddMessage(Message{Role: RoleSystem, Content: "sys"})
	c.AddMessage(Message{Role: RoleUser, Content: "first task"})
	c.AddMessage(Message{Role: RoleAssistant, Content: "ack"})

	// Below low watermark: no compaction.
	c.AddMessage(Message{Role: RoleUser, Content: "filler"})

	// Push above the high watermark with one huge middle message.
	huge := make([]byte, 2000)
	for i := range huge {
		huge[i] = 'y'
	}
	c.AddMessage(Message{Role: RoleUser, Content: string(huge)})
	c.AddMessage(Message{Role: RoleAssistant, Content: "done"})

	c.AutoCompact()

	if got := c.GetTotalTokens(); got > c.MaxTokens*8/10 {
		t.Errorf("AutoCompact left %d tokens, want <= %d (80%%)", got, c.MaxTokens*8/10)
	}
	// System prompt and first pair must survive.
	if c.Messages[0].Role != RoleSystem {
		t.Errorf("system prompt lost")
	}
	// Marker must be present.
	foundMarker := false
	for _, m := range c.Messages {
		if m.Role == RoleSystem && contains(m.Content, "[Context compacted:") {
			foundMarker = true
		}
	}
	if !foundMarker {
		t.Errorf("expected compaction marker message")
	}
}

func TestConversationSetSystemPrompt(t *testing.T) {
	c := NewConversation()
	c.SetSystemPrompt("v1")
	c.AddMessage(Message{Role: RoleUser, Content: "hi"})
	c.SetSystemPrompt("v2")
	if c.Messages[0].Content != "v2" {
		t.Errorf("system prompt = %q, want v2", c.Messages[0].Content)
	}
	if c.HasSystemPrompt() != true {
		t.Error("HasSystemPrompt should be true")
	}
}

func TestConversationCompleteness(t *testing.T) {
	c := NewConversation()
	c.AddMessage(Message{Role: RoleUser, Content: "go"})
	if c.IsComplete() {
		t.Error("fresh conversation should not be complete")
	}
	c.SetComplete()
	if !c.IsComplete() {
		t.Error("expected complete")
	}
	c.MarkIncomplete()
	if c.IsComplete() {
		t.Error("MarkIncomplete should clear complete")
	}
	c.Clear()
	if c.MessageCount() != 0 || c.CurrentTokens != 0 {
		t.Error("Clear should reset messages and tokens")
	}
}

func TestEstimatorInjection(t *testing.T) {
	orig := Estimator
	defer func() { Estimator = orig }()

	Estimator = func(s string) int { return len(s) } // exact fake

	c := NewConversation()
	c.AddMessage(Message{Role: RoleUser, Content: "abcdef"}) // 6 chars → 6 tokens
	if c.CurrentTokens == 0 {
		t.Fatal("expected nonzero tokens")
	}
	if got := c.CurrentTokens; got < 6 {
		t.Errorf("CurrentTokens = %d, want >= 6 with injected estimator", got)
	}
}

func contains(s, substr string) bool {
	return len(substr) == 0 || (len(s) >= len(substr) && indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// TestPatchMessageContent pins the deliver-once patch primitive: content
// mutates, the token estimate follows the delta, and — the whole point —
// the actual-usage counters SURVIVE (ReplaceMessages resets them, which
// would drop the turn's usage fact when a host stubs delivered images
// mid-run at the next stream start).
func TestPatchMessageContent(t *testing.T) {
	c := &Conversation{}
	c.AddMessage(Message{Role: RoleUser, Content: "[image: a.png]\nmeta"})
	c.AddMessage(Message{Role: RoleAssistant, Content: "ok"})
	c.AddActualTokens(111, 22)
	totalBefore := c.GetTotalTokens()

	patched := "[image: a.png | viewed]\nmeta"
	if !c.PatchMessageContent(0, patched) {
		t.Fatal("patch of existing index failed")
	}
	if c.Messages[0].Content != patched {
		t.Fatalf("content not patched: %q", c.Messages[0].Content)
	}
	if c.Messages[1].Content != "ok" {
		t.Fatalf("neighbor touched: %q", c.Messages[1].Content)
	}
	if in, out := c.GetActualTokens(); in != 111 || out != 22 {
		t.Fatalf("actual usage lost: %d/%d", in, out)
	}
	if total := c.GetTotalTokens(); total == 0 || total == totalBefore && totalBefore == 0 {
		t.Fatalf("token estimate not maintained: %d", total)
	}
	if c.PatchMessageContent(-1, "x") || c.PatchMessageContent(99, "x") {
		t.Fatal("out-of-range patch must report false")
	}
	// Same content = no-op success (idempotent stubbing).
	if !c.PatchMessageContent(0, patched) {
		t.Fatal("idempotent patch failed")
	}
}
