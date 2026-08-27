package tools

import (
	"testing"
)

func TestParseXMLToolCalls(t *testing.T) {
	toolNames := []string{"read_file", "execute_command", "write_file"}

	tests := []struct {
		name     string
		content  string
		expected []struct {
			toolName   string
			shouldHave string // substring that must exist in the JSON input
		}
	}{
		{
			name:    "single tool call",
			content: "Let me read the file for you.\n<read_file><path>/some/path.txt</path></read_file>",
			expected: []struct {
				toolName   string
				shouldHave string
			}{{
				toolName:   "read_file",
				shouldHave: `"path":"/some/path.txt"`,
			}},
		},
		{
			name:    "multiple tool calls",
			content: "<read_file><path>/a.txt</path></read_file>\n<execute_command><command>ls -la</command></execute_command>",
			expected: []struct {
				toolName   string
				shouldHave string
			}{
				{toolName: "read_file", shouldHave: `"path":"/a.txt"`},
				{toolName: "execute_command", shouldHave: `"command":"ls -la"`},
			},
		},
		{
			name:     "unknown tool name should be ignored",
			content:  "<unknown_tool><foo>bar</foo></unknown_tool>",
			expected: nil,
		},
		{
			name:    "whitespace inside tags",
			content: "<write_file>\n<path>/test.txt</path>\n<content>hello world</content>\n</write_file>",
			expected: []struct {
				toolName   string
				shouldHave string
			}{{
				toolName:   "write_file",
				shouldHave: `"content":"hello world"`,
			}},
		},
		{
			name:     "empty content",
			content:  "",
			expected: nil,
		},
		{
			name:     "no tool calls in content",
			content:  "Here is some normal text without any XML tags.",
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ParseXMLToolCalls(tt.content, toolNames)
			if len(result) != len(tt.expected) {
				t.Fatalf("expected %d tool calls, got %d", len(tt.expected), len(result))
			}
			for i, exp := range tt.expected {
				if result[i].Name != exp.toolName {
					t.Errorf("expected tool name %q, got %q", exp.toolName, result[i].Name)
				}
				if !contains(string(result[i].Input), exp.shouldHave) {
					t.Errorf("expected input to contain %q, got %q", exp.shouldHave, result[i].Input)
				}
			}
		})
	}
}

func contains(s, substr string) bool {
	return containsInString(s, substr)
}

func containsInString(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
