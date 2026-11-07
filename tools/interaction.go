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
	Question      string   `json:"question"`
	Options       []string `json:"options,omitempty"`
	AllowMultiple bool     `json:"allow_multiple,omitempty"`
}

// NewAskFollowupQuestionTool creates a new ask_followup_question tool
func NewAskFollowupQuestionTool() *AskFollowupQuestionTool {
	return &AskFollowupQuestionTool{
		BaseTool: BaseTool{
			name: "ask_followup_question",
			description: "Ask the user a question to gather clarifying information or make a decision. Use this when you need more information to complete a task. " +
				"Set allow_multiple when several options may be chosen; the answer then lists the picks comma-separated.",
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
			// Resolve numeric option picks ("2", or "1,3" with allow_multiple).
			answer = mapOptionAnswer(req.Options, answer, req.AllowMultiple)
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
	answer = mapOptionAnswer(req.Options, answer, req.AllowMultiple)

	return fmt.Sprintf("User answered: %s", answer), nil
}

// mapOptionAnswer resolves numeric option picks to their texts: a single
// index ("2"), or with allow_multiple a comma-separated list ("1, 3" →
// "first, third"). Non-numeric answers pass through unchanged.
func mapOptionAnswer(options []string, answer string, allowMultiple bool) string {
	if len(options) == 0 {
		return answer
	}
	if !allowMultiple {
		if n, err := parseInt(answer); err == nil && n > 0 && n <= len(options) {
			return options[n-1]
		}
		return answer
	}
	parts := strings.Split(answer, ",")
	mapped := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if n, err := parseInt(p); err == nil && n > 0 && n <= len(options) {
			p = options[n-1]
		}
		mapped = append(mapped, p)
	}
	if len(mapped) == 0 {
		return answer
	}
	return strings.Join(mapped, ", ")
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
