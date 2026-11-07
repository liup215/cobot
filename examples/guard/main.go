// Command guard demonstrates the ToolGuard approval gate and its three
// decision outcomes:
//
//  1. allow      — the call proceeds normally.
//  2. deny       — (false, reason, nil): the tool is NOT executed; the
//     reason is fed back to the model as "[DENIED] <reason>" and the loop
//     continues, so the model can adapt.
//  3. ErrAbortRun — (false, "", err wrapping ErrAbortRun): the whole run
//     ends immediately; the guarded tool and everything after it in the
//     same turn is skipped.
//
// A ToolGuard is consulted before every tool execution on both execution
// paths (main loop + stream pre-dispatch), so it MUST be side-effect free
// and idempotent. Any other non-nil error fails closed (treated as a deny).
//
//	go run ./examples/guard
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/liup215/cobot"
	"github.com/liup215/cobot/provider"
)

// runScenario runs one scripted agent with the given guard and reports what
// happened. Each scenario gets a fresh agent so conversations stay separate.
func runScenario(name string, guard cobot.ToolGuard, turns ...provider.TurnScript) {
	agent, err := cobot.NewAgent(cobot.Config{
		Provider: provider.NewScriptedProvider(turns...), // offline; use APIKey for a real backend
		Guard:    guard,
	})
	if err != nil {
		log.Fatal(err)
	}
	res, err := agent.Run(context.Background(), "clean up the temp directory")
	fmt.Printf("── %s\n", name)
	if err != nil {
		// By Run's contract res is non-nil on error paths too (except
		// concurrent-run misuse), but a host should always check err first.
		fmt.Printf("   run failed: %v\n", err)
		if res != nil {
			fmt.Printf("   partial result: turns=%d final=%q\n", res.Turns, res.FinalText)
		}
		return
	}
	fmt.Printf("   err=<nil> turns=%d final=%q\n", res.Turns, res.FinalText)
}

func main() {
	commandCall := provider.ToolCall{
		ID:   "call-1",
		Name: "run",
		// Harmless on purpose: in the allow scenario the command really
		// executes (cmd /C on Windows, sh -c elsewhere).
		Input: `{"command": "echo cleaning ./tmp"}`,
	}
	completion := provider.TurnScript{
		ToolCalls: []provider.ToolCall{{
			ID:    "call-2",
			Name:  "attempt_completion",
			Input: `{"result": "temp directory cleaned"}`,
		}},
	}

	// 1. Allow: every call proceeds.
	runScenario("allow — guard returns (true, \"\", nil)",
		func(ctx context.Context, call cobot.GuardedCall) (bool, string, error) {
			return true, "", nil
		},
		provider.TurnScript{ToolCalls: []provider.ToolCall{commandCall}},
		completion,
	)

	// 2. Deny: the destructive command is not executed; the reason is fed
	// back to the model, which adapts and ends the run another way.
	runScenario("deny — guard returns (false, reason, nil)",
		func(ctx context.Context, call cobot.GuardedCall) (bool, string, error) {
			if call.Name == "run" {
				return false, "command execution is disabled in this demo", nil
			}
			return true, "", nil
		},
		provider.TurnScript{ToolCalls: []provider.ToolCall{commandCall}},
		provider.TurnScript{
			Content: "The command was denied; asking the user instead.",
			ToolCalls: []provider.ToolCall{{
				ID:    "call-2",
				Name:  "attempt_completion",
				Input: `{"result": "Could not clean up: command execution is disabled in this demo."}`,
			}},
		},
	)

	// 3. Abort: ErrAbortRun ends the whole run on the spot. The run returns
	// cleanly (err == nil), but note turns == 1: the second turn never
	// happened because the run was aborted inside the first one.
	runScenario("abort — guard returns an error wrapping ErrAbortRun",
		func(ctx context.Context, call cobot.GuardedCall) (bool, string, error) {
			if call.Name == "run" {
				return false, "", fmt.Errorf("policy: %w", cobot.ErrAbortRun)
			}
			return true, "", nil
		},
		provider.TurnScript{ToolCalls: []provider.ToolCall{commandCall}},
		completion,
	)
}
