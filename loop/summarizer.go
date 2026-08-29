package loop

import (
	"context"
	"errors"

	"github.com/liup215/cobot/types"
)

// defaultSummarizeMaxTokens caps the tokens the compaction summarizer may
// generate. Summaries are intentionally short; a bigger budget only wastes
// tokens and latency at the 60% compaction trigger point.
const defaultSummarizeMaxTokens = 1024

// ProviderSummarizer adapts a Provider to compact.Summarizer with a single
// non-streaming LLM call, so the default compaction strategy reuses the
// session's own provider (no extra client, no extra configuration). It
// mirrors gline's ProviderSummarizerCaller.
//
// Because it is a plain single-turn call it never spawns tool-using
// subagents, and it never goes through the stream/retry machinery of the main
// loop: a summarizer failure is reported to compact.Combo, which falls back
// to the watermark Trim strategy.
type ProviderSummarizer struct {
	provider Provider

	// MaxTokens caps the summary generation. 0 = defaultSummarizeMaxTokens.
	MaxTokens int
}

// NewProviderSummarizer adapts p to compact.Summarizer.
func NewProviderSummarizer(p Provider) *ProviderSummarizer {
	return &ProviderSummarizer{provider: p}
}

// Summarize performs one non-streaming completion over the prompt.
func (s *ProviderSummarizer) Summarize(ctx context.Context, prompt string) (string, error) {
	if s == nil || s.provider == nil {
		return "", errors.New("providersummarizer: no provider configured")
	}
	resp, err := s.provider.CreateMessage(ctx, &MessageRequest{
		Messages:  []types.Message{{Role: types.RoleUser, Content: prompt}},
		MaxTokens: s.maxTokens(),
	})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

func (s *ProviderSummarizer) maxTokens() int {
	if s == nil || s.MaxTokens <= 0 {
		return defaultSummarizeMaxTokens
	}
	return s.MaxTokens
}
