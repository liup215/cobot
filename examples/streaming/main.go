// Command streaming demonstrates the streaming event surface: run the agent
// with RunWithCallback and a Callback implementation that embeds
// cobot.CallbackAdapter (the root package's re-export of
// provider.StreamCallbackAdapter — a no-op base) and overrides only the
// events it cares about — OnContent for assistant text deltas and
// OnToolCallStart / OnToolCallComplete for tool lifecycle.
//
// Like every example it runs fully offline on a scripted provider:
//
//	go run ./examples/streaming
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/liup215/cobot"
	"github.com/liup215/cobot/provider"
)

// printer is a Callback that renders the run as it streams. Embedding
// cobot.CallbackAdapter gives no-op implementations for every event; we
// override only OnStreamStart, OnContent and the tool lifecycle hooks.
// Other available hooks: OnReasoning, OnToolCallUpdate, AskFollowupQuestion,
// OnError, OnComplete, OnTaskCreated, OnCompactionStart/End, OnRetry.
type printer struct {
	cobot.CallbackAdapter
}

func (p *printer) OnStreamStart() {
	fmt.Println("── stream start ──")
}

// OnContent receives assistant reply text as it streams from the provider.
func (p *printer) OnContent(delta string) {
	fmt.Print(delta)
}

// OnToolCallStart / OnToolCallComplete bracket each tool execution.
// The ToolCall argument is provider.ToolCall: ID, Name and raw JSON Input.
func (p *printer) OnToolCallStart(tc provider.ToolCall) {
	fmt.Printf("\n⚙ %s %s\n", tc.Name, tc.Input)
}

func (p *printer) OnToolCallComplete(tc provider.ToolCall, result string) {
	const max = 120
	out := result
	if len(out) > max {
		out = out[:max] + "…"
	}
	fmt.Printf("✓ %s → %s\n", tc.Name, out)
}

func main() {
	prov := provider.NewScriptedProvider(
		// Turn 1: some streamed text, then a tool call. The loop executes
		// ls (a real built-in tool) and feeds the result back.
		provider.TurnScript{
			Content: "Let me look at the project layout first.",
			ToolCalls: []provider.ToolCall{{
				ID:    "call-1",
				Name:  "ls",
				Input: `{"path": "."}`,
			}},
		},
		// Turn 2: final text plus the attempt_completion tool call that
		// ends the run.
		provider.TurnScript{
			Content: "The project root looks good. ",
			ToolCalls: []provider.ToolCall{{
				ID:    "call-2",
				Name:  "attempt_completion",
				Input: `{"result": "Listed the project root; structure is as expected."}`,
			}},
		},
	)

	agent, err := cobot.NewAgent(cobot.Config{
		Provider:    prov, // offline; use APIKey for a real backend
		AutoApprove: true,
	})
	if err != nil {
		log.Fatal(err)
	}

	res, err := agent.RunWithCallback(context.Background(), "show me the project layout", &printer{})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("\n── done: %d turn(s), %d/%d tokens, final: %s\n",
		res.Turns, res.InputTokens, res.OutputTokens, res.FinalText)
}
