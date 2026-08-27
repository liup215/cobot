// XML tool call parsing fallback for when native tool_calls are not available.
// Ported from gline internal/agent/xml_tools.go; takes plain tool names so the
// tools package does not depend on the provider package.
package tools

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/liup215/cobot/types"
)

// isKnownToolName reports whether name is in toolNames.
func isKnownToolName(name string, toolNames []string) bool {
	for _, n := range toolNames {
		if n == name {
			return true
		}
	}
	return false
}

// ParseXMLToolCalls extracts tool calls from assistant message content using XML format.
// This serves as a fallback when the LLM does not use native tool_calls but instead
// embeds XML tool calls in the response content.
// toolNames lists the tools currently offered to the model; unknown tags are ignored.
func ParseXMLToolCalls(content string, toolNames []string) []types.ToolCall {
	if content == "" || len(toolNames) == 0 {
		return nil
	}

	var result []types.ToolCall

	// Scan for known tool opening tags and manually find matching closing tags.
	// Go's regexp does not support backreferences, so we can't use `<tag>...</tag>` reliably.
	for _, toolName := range toolNames {
		openTag := "<" + toolName + ">"
		closeTag := "</" + toolName + ">"

		start := 0
		for {
			idx := strings.Index(content[start:], openTag)
			if idx < 0 {
				break
			}
			idx += start

			// Find matching closing tag
			closeIdx := strings.Index(content[idx+len(openTag):], closeTag)
			if closeIdx < 0 {
				break
			}
			closeIdx += idx + len(openTag)

			innerContent := content[idx+len(openTag) : closeIdx]

			// Parse parameters from inner content
			params := make(map[string]string)
			paramStart := 0
			for {
				pIdx := strings.Index(innerContent[paramStart:], "<")
				if pIdx < 0 {
					break
				}
				pIdx += paramStart

				// Find closing > of open tag
				pEndIdx := strings.Index(innerContent[pIdx:], ">")
				if pEndIdx < 0 {
					break
				}
				paramName := innerContent[pIdx+1 : pIdx+pEndIdx]

				// Find matching closing tag </paramName>
				closeParamTag := "</" + paramName + ">"
				pCloseIdx := strings.Index(innerContent[pIdx+pEndIdx+1:], closeParamTag)
				if pCloseIdx < 0 {
					paramStart = pIdx + pEndIdx + 1
					continue
				}
				pCloseIdx += pIdx + pEndIdx + 1

				paramValue := innerContent[pIdx+pEndIdx+1 : pCloseIdx]
				params[paramName] = strings.TrimSpace(paramValue)

				paramStart = pCloseIdx + len(closeParamTag)
			}

			jsonInput, err := json.Marshal(params)
			if err != nil {
				slog.Warn(fmt.Sprintf("Failed to marshal XML tool params: %v", err))
				start = closeIdx + len(closeTag)
				continue
			}

			toolCallID := fmt.Sprintf("call_%s_%d_%d", toolName, len(result), idx)
			result = append(result, types.ToolCall{
				ID:    toolCallID,
				Name:  toolName,
				Input: jsonInput,
			})

			slog.Info(fmt.Sprintf("Parsed XML tool call from content: tool=%s id=%s", toolName, toolCallID))

			start = closeIdx + len(closeTag)
		}
	}

	return result
}
