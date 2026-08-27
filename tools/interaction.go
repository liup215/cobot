package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// AskFollowupQuestionTool asks the user a question
type AskFollowupQuestionTool struct {
	BaseTool
	// handler, when set, will be used to prompt the user (e.g., TUI). It should return the user's answer.
	handler func(question string, options []string) (string, error)
}

// AskFollowupQuestionInput represents the input for ask_followup_question tool
type AskFollowupQuestionInput struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

// NewAskFollowupQuestionTool creates a new ask_followup_question tool
func NewAskFollowupQuestionTool() *AskFollowupQuestionTool {
	return &AskFollowupQuestionTool{
		BaseTool: BaseTool{
			name:        "ask_followup_question",
			description: "Ask the user a question to gather clarifying information or make a decision. Use this when you need more information to complete a task.",
			inputSchema: QuestionSchema,
		},
	}
}

// SetHandler sets a custom handler for prompting the user. If nil, the tool falls back to CLI stdin.
func (t *AskFollowupQuestionTool) SetHandler(h func(question string, options []string) (string, error)) {
	t.handler = h
}

// Execute asks the user a question
func (t *AskFollowupQuestionTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var req AskFollowupQuestionInput
	if err := ParseInput(input, &req); err != nil {
		return "", err
	}

	if req.Question == "" {
		return "", fmt.Errorf("question is required")
	}

	// If a handler is provided (e.g., the TUI), delegate the prompt to it.
	if t.handler != nil {
		type resp struct {
			ans string
			err error
		}
		ch := make(chan resp, 1)
		go func() {
			a, e := t.handler(req.Question, req.Options)
			ch <- resp{ans: a, err: e}
		}()

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case r := <-ch:
			if r.err != nil {
				return "", r.err
			}
			answer := strings.TrimSpace(r.ans)
			// If options provided and user answered a number, map it.
			if len(req.Options) > 0 {
				if num, err := parseInt(answer); err == nil && num > 0 && num <= len(req.Options) {
					answer = req.Options[num-1]
				}
			}
			return fmt.Sprintf("User answered: %s", answer), nil
		}
	}

	// Fallback: CLI prompt using stdin
	fmt.Println()
	fmt.Println("❓", req.Question)
	fmt.Println()

	if len(req.Options) > 0 {
		fmt.Println("Options:")
		for i, opt := range req.Options {
			fmt.Printf("  %d. %s\n", i+1, opt)
		}
		fmt.Println()
	}

	fmt.Print("Your answer: ")
	reader := bufio.NewReader(os.Stdin)
	answer, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("failed to read input: %w", err)
	}

	answer = strings.TrimSpace(answer)

	if len(req.Options) > 0 {
		if num, err := parseInt(answer); err == nil && num > 0 && num <= len(req.Options) {
			answer = req.Options[num-1]
		}
	}

	return fmt.Sprintf("User answered: %s", answer), nil
}

// parseInt parses a string to int
func parseInt(s string) (int, error) {
	var n int
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(ch-'0')
	}
	return n, nil
}

// AttemptCompletionTool signals task completion
type AttemptCompletionTool struct {
	BaseTool
}

// AttemptCompletionInput represents the input for attempt_completion tool
type AttemptCompletionInput struct {
	Result  string `json:"result"`
	Command string `json:"command,omitempty"`
}

// NewAttemptCompletionTool creates a new attempt_completion tool
func NewAttemptCompletionTool() *AttemptCompletionTool {
	return &AttemptCompletionTool{
		BaseTool: BaseTool{
			name:        "attempt_completion",
			description: "Indicate that you have completed the task and provide a summary of what was accomplished. Use this when you have finished all required work.",
			inputSchema: CompletionSchema,
		},
	}
}

// Execute completes the task
func (t *AttemptCompletionTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var req AttemptCompletionInput
	if err := ParseInput(input, &req); err != nil {
		return "", err
	}

	if req.Result == "" {
		return "", fmt.Errorf("result is required")
	}

	// Format completion message
	var builder strings.Builder
	builder.WriteString("\n")
	builder.WriteString("✅ Task Completed\n")
	builder.WriteString(strings.Repeat("=", 50))
	builder.WriteString("\n\n")
	builder.WriteString(req.Result)
	builder.WriteString("\n")

	if req.Command != "" {
		builder.WriteString("\n")
		builder.WriteString("📋 Command to review result:\n")
		builder.WriteString(fmt.Sprintf("   %s\n", req.Command))
	}

	builder.WriteString("\n")
	builder.WriteString(strings.Repeat("=", 50))
	builder.WriteString("\n")

	return builder.String(), nil
}

// PlanModeRespondTool responds in plan mode
type PlanModeRespondTool struct {
	BaseTool
}

// PlanModeRespondInput represents the input for plan_mode_respond tool
type PlanModeRespondInput struct {
	Response string `json:"response"`
}

// NewPlanModeRespondTool creates a new plan_mode_respond tool
func NewPlanModeRespondTool() *PlanModeRespondTool {
	schema := json.RawMessage(`{
"type": "object",
"properties": {
"response": {
"type": "string",
"description": "The response to present to the user in plan mode"
}
},
"required": ["response"]
}`)
	return &PlanModeRespondTool{
		BaseTool: BaseTool{
			name:        "plan_mode_respond",
			description: "Respond to the user in plan mode. Use this to present your plan, ask questions, or engage in back-and-forth conversation before implementing.",
			inputSchema: schema,
		},
	}
}

// Execute responds in plan mode
func (t *PlanModeRespondTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var req PlanModeRespondInput
	if err := ParseInput(input, &req); err != nil {
		return "", err
	}

	if req.Response == "" {
		return "", fmt.Errorf("response is required")
	}

	return req.Response, nil
}
