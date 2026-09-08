// Command basic is the minimal cobot example: build a Config, call Run,
// print the attempt_completion deliverable from the returned RunResult.
//
// It runs fully offline: instead of APIKey it injects a scripted provider
// that replays canned LLM turns, so no API key or network is needed. In a
// real host you would set APIKey (and optionally BaseURL/Model) instead —
// everything else stays the same.
//
// Run it with:
//
//	go run ./examples/basic
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/liup215/cobot"
	"github.com/liup215/cobot/provider"
)

func main() {
	// The model plays two canned turns: one assistant turn that ends with
	// the attempt_completion tool call, which terminates the agent loop.
	// TurnScript.ToolCalls are complete provider.ToolCall chunks (ID, Name
	// and the JSON Input string), exactly like a finished OpenAI tool_call.
	prov := provider.NewScriptedProvider(
		provider.TurnScript{
			Content: "Checking the repository layout first.",
			ToolCalls: []provider.ToolCall{{
				ID:    "call-1",
				Name:  "attempt_completion",
				Input: `{"result": "All done: the login retry logic now backs off exponentially instead of hammering the server."}`,
			}},
		},
	)

	agent, err := cobot.NewAgent(cobot.Config{
		// Provider is injected directly here so the example runs offline.
		// For a real backend, delete Provider and set:
		//   APIKey: os.Getenv("OPENAI_API_KEY"),
		//   Model:  "gpt-4o", // optional, provider default when empty
		Provider:    prov,
		AutoApprove: true,
	})
	if err != nil {
		log.Fatal(err)
	}

	res, err := agent.Run(context.Background(), "fix the login bug")
	if err != nil {
		log.Fatal(err)
	}

	// RunResult is the structured outcome of the run:
	fmt.Println("deliverable :", res.FinalText) // attempt_completion's `result`
	fmt.Println("input tokens:", res.InputTokens)
	fmt.Println("output tok. :", res.OutputTokens)
	fmt.Println("turns       :", res.Turns)
	fmt.Println("duration    :", res.Duration)
}
