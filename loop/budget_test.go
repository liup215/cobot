package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/liup215/cobot/tools"
	"github.com/liup215/cobot/types"
)

// fakeTool is a marshalable tool stand-in: tools.Tool is an interface, so the
// loop's tool-table estimate only sees the fields a concrete type exports.
type fakeTool struct{ ToolName, ToolDesc string }

func (f fakeTool) Name() string                                             { return f.ToolName }
func (f fakeTool) Description() string                                      { return f.ToolDesc }
func (f fakeTool) InputSchema() json.RawMessage                             { return json.RawMessage(`{}`) }
func (f fakeTool) Execute(context.Context, json.RawMessage) (string, error) { return "", nil }

// TestRequestTokensDoesNotDoubleCountTheFixedOverhead pins the accounting bug
// that made a long session report "context full" while the provider was happy:
// once a usage fact exists it already measured the whole request (system prompt
// plus tool table included), so the per-turn overhead must not be added again.
func TestRequestTokensDoesNotDoubleCountTheFixedOverhead(t *testing.T) {
	orig := types.Estimator
	types.Estimator = func(s string) int { return len(s) }
	t.Cleanup(func() { types.Estimator = orig })

	conv := types.NewConversation()
	conv.MaxTokens = 128000
	conv.AddMessage(types.Message{Role: types.RoleUser, Content: strings.Repeat("x", 3000)})

	availableTools := []tools.Tool{fakeTool{ToolName: "read_file", ToolDesc: strings.Repeat("d", 5000)}}
	system := strings.Repeat("s", 1000)

	estimated := requestTokens(conv, system, availableTools)
	if estimated < 4000 {
		t.Fatalf("estimate without a usage fact = %d, want the messages (3000) plus the overhead", estimated)
	}

	// A real measurement replaces the whole basis: 90k input includes the
	// system prompt and the tool table. Adding the overhead a second time is
	// what inflated occupancy every turn and tripped the watermark early.
	conv.AddActualTokens(90000, 500)
	if got := requestTokens(conv, system, availableTools); got != 90500 {
		t.Fatalf("with a usage fact = %d, want the measured 90500 (no extra overhead)", got)
	}
}

// TestRequestTokensWithoutAConversation is the nil guard.
func TestRequestTokensWithoutAConversation(t *testing.T) {
	if got := requestTokens(nil, "system", nil); got != 0 {
		t.Fatalf("requestTokens(nil) = %d, want 0", got)
	}
}
