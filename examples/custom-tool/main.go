// Command custom-tool shows how a host extends the tool registry with its
// own Go tool. This is exactly the injection point Maestro uses to add its
// built-in host tools: take the default registry (or an empty one), call
// Registry.Register with a tools.ToolInfo wrapping your Tool implementation,
// and hand the registry to Config.Tools.
//
// The example registers a mock get_weather tool (a stand-in for any host
// capability: database lookup, internal API, ...) and lets the agent call it:
//
//	go run ./examples/custom-tool
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/liup215/cobot"
	"github.com/liup215/cobot/provider"
	"github.com/liup215/cobot/tools"
)

// weatherTool implements tools.Tool: name, description, JSON input schema
// and the Execute function. The description and schema are what the model
// sees in the AVAILABLE TOOLS section of the system prompt, so they double
// as the tool's prompt documentation.
type weatherTool struct {
	calls int
}

func (w *weatherTool) Name() string { return "get_weather" }

func (w *weatherTool) Description() string {
	return "Get the current weather for a city. Use this whenever the task depends on weather conditions."
}

func (w *weatherTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"city": {"type": "string", "description": "City name, e.g. Tokyo"}
		},
		"required": ["city"]
	}`)
}

func (w *weatherTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		City string `json:"city"`
	}
	if err := tools.ParseInput(input, &args); err != nil {
		return "", err
	}
	w.calls++
	// A real tool would call a service here; keep it deterministic.
	return fmt.Sprintf(`{"city": %q, "conditions": "sunny", "temp_c": 24}`, args.City), nil
}

func main() {
	// Extend the default registry: all built-in tools stay available and
	// get_weather is added. The ToolInfo carries the metadata the loop uses:
	// Category is informational, AllowedModes gates plan/act usage
	// ("plan", "act", or "*" for both).
	weather := &weatherTool{}
	reg := tools.InitDefaultRegistry()
	if err := reg.Register(&tools.ToolInfo{
		Tool:         weather,
		Category:     tools.CategorySearch,
		AllowedModes: []string{"plan", "act"},
	}); err != nil {
		log.Fatal(err)
	}

	prov := provider.NewScriptedProvider(
		provider.TurnScript{
			ToolCalls: []provider.ToolCall{{
				ID:    "call-1",
				Name:  "get_weather",
				Input: `{"city": "Tokyo"}`,
			}},
		},
		provider.TurnScript{
			ToolCalls: []provider.ToolCall{{
				ID:    "call-2",
				Name:  "attempt_completion",
				Input: `{"result": "Tokyo is sunny at 24°C — a good day for the outdoor launch event."}`,
			}},
		},
	)

	agent, err := cobot.NewAgent(cobot.Config{
		Provider: prov, // offline; use APIKey for a real backend
		Tools:    reg,  // nil would mean InitDefaultRegistry()
	})
	if err != nil {
		log.Fatal(err)
	}

	res, err := agent.Run(context.Background(), "should we hold the launch event outdoors in Tokyo?")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("deliverable:", res.FinalText)
	// Note: a tool without RequiresConfirmation may be pre-dispatched while
	// the stream is still open, and with an instant mock stream the
	// pre-dispatch and main-loop executions can overlap (documented gline
	// best-effort semantics), so the counter may read 2 for one logical call.
	// Side-effectful tools should set RequiresConfirmation: true.
	fmt.Println("get_weather executions:", weather.calls)
}
