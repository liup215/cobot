package loop

import (
	"encoding/json"

	"github.com/liup215/cobot/tools"
	"github.com/liup215/cobot/types"
)

// requestTokens estimates how many tokens the NEXT provider call will send:
// the conversation, the system prompt and the JSON-serialized tool table.
//
// The point of routing every budget decision through this one function is that
// the three inputs are not additive on the same basis. When the provider
// reported usage for the previous turn, that number already measured the whole
// request — system prompt and tool table included — so adding them again
// inflates occupancy by the fixed overhead. On a desktop-sized tool set that
// overhead is thousands of tokens, which is enough to trip a watermark one or
// two turns early and to make the reported context usage disagree with what
// the model actually saw.
//
// Only when there is no usage fact (the very first request, or a provider that
// never reports usage) is the conversation estimate an estimate of the messages
// alone — and only then is the overhead added.
//
// pi does the same thing: estimateContextTokens() takes the last usage report
// as the baseline and adds estimates only for the messages that arrived after
// it, and estimateProjectedContextTokens() drops a usage report outright when
// it predates an edit.
func requestTokens(conv *types.Conversation, systemPrompt string, availableTools []tools.Tool) int {
	if conv == nil {
		return 0
	}
	total := conv.GetTotalTokens()
	if input, output := conv.GetActualTokens(); input+output > 0 {
		// A real measurement of the whole request: nothing to add.
		return total
	}
	total += types.Estimator(systemPrompt)
	if len(availableTools) > 0 {
		if toolJSON, err := json.Marshal(availableTools); err == nil {
			total += types.Estimator(string(toolJSON))
		}
	}
	return total
}
