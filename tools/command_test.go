package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/liup215/cobot/shell"
)

func TestRunUsesResolvedShell(t *testing.T) {
	sh := shell.Resolve()
	tool := NewRunTool()

	// $(( )) arithmetic is POSIX-shell syntax; cmd.exe cannot evaluate it,
	// so a "5" in the output proves the command ran under bash/sh.
	input, _ := json.Marshal(map[string]string{"command": "echo $((2+3))"})
	result, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sh.IsBash {
		if !strings.Contains(result, "5") {
			t.Fatalf("expected arithmetic result 5 under bash, got: %s", result)
		}
	} else {
		// cmd.exe fallback: the expression is not expanded; just ensure the
		// command executed and reported an exit code.
		if !strings.Contains(result, "Exit code:") {
			t.Fatalf("expected exit code in output, got: %s", result)
		}
	}
}

func TestRunRequiresCommand(t *testing.T) {
	tool := NewRunTool()
	input, _ := json.Marshal(map[string]string{"command": ""})
	if _, err := tool.Execute(context.Background(), input); err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestRunDescriptionMatchesResolvedShell(t *testing.T) {
	tool := NewRunTool()
	desc := tool.Description()
	if desc == "" {
		t.Fatal("empty description")
	}
	// The description must commit to one shell syntax (pi's lesson: the
	// model writes for the shell the description names, never "either").
	if strings.Contains(desc, "when available") || strings.Contains(desc, "falls back") {
		t.Fatalf("description hedges shell choice instead of committing: %s", desc)
	}
	switch shell.Resolve().Name() {
	case "bash":
		if !strings.Contains(desc, "POSIX syntax works") {
			t.Errorf("bash machine but description lacks POSIX guidance: %s", desc)
		}
	case "cmd":
		if !strings.Contains(desc, "cmd.exe") {
			t.Errorf("cmd machine but description lacks cmd guidance: %s", desc)
		}
	case "sh":
		if !strings.Contains(desc, "under sh") {
			t.Errorf("sh machine but description lacks sh guidance: %s", desc)
		}
	}
	// Routing guidance must steer file work to the dedicated tools.
	if !strings.Contains(desc, "use read") || !strings.Contains(desc, "grep/glob") {
		t.Errorf("description lacks tool-routing guidance: %s", desc)
	}
}

// TestRunApprovalGate: requires_approval and destructive commands ask the
// injected handler; innocent commands never do; failures fail closed.
func TestRunApprovalGate(t *testing.T) {
	calls := 0
	allow := func(ctx context.Context, cmd string) (bool, error) {
		calls++
		return false, nil // would deny — must never be reached for harmless cmds
	}

	// Innocuous command: no gate.
	tool := NewRunToolWithApproval(allow)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"echo plain"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 0 {
		t.Errorf("approval called %d times for a harmless command, want 0", calls)
	}
	if !strings.Contains(out, "Exit code: 0") {
		t.Errorf("harmless command did not run: %s", out)
	}

	// requires_approval forces the gate.
	out, err = tool.Execute(context.Background(), json.RawMessage(`{"command":"echo x","requires_approval":true}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("approval called %d times, want 1", calls)
	}
	if !strings.Contains(out, "NOT executed") || !strings.Contains(out, "denied") {
		t.Errorf("want denial message, got: %s", out)
	}
	if strings.Contains(out, "Exit code") {
		t.Error("denied command must not report an exit code")
	}

	// Destructive command triggers the gate even without the flag.
	calls = 0
	out, err = tool.Execute(context.Background(), json.RawMessage(`{"command":"rm -rf build"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("destructive command approval called %d times, want 1", calls)
	}
	if !strings.Contains(out, "NOT executed") {
		t.Errorf("want denial, got: %s", out)
	}
}

// TestRunApprovalFailsClosed: a handler error denies instead of running.
func TestRunApprovalFailsClosed(t *testing.T) {
	tool := NewRunToolWithApproval(func(ctx context.Context, cmd string) (bool, error) {
		return true, errors.New("modal exploded")
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"rm -rf build"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "NOT executed") || !strings.Contains(out, "approval request failed") {
		t.Errorf("want fail-closed message, got: %s", out)
	}
}
