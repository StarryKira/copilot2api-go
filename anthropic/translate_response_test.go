package anthropic

import "testing"

func TestTranslateToAnthropicSubtractsCachedTokens(t *testing.T) {
	resp := ChatCompletionResponse{
		ID:    "resp_1",
		Model: "claude-sonnet-4",
		Choices: []Choice{{
			Message: &ChoiceMsg{Role: "assistant", Content: "ok"},
		}},
		Usage: &OpenAIUsage{
			PromptTokens:     120,
			CompletionTokens: 30,
			PromptTokensDetails: &PromptTokensDetails{
				CachedTokens: 45,
			},
		},
	}

	got := TranslateToAnthropic(resp)

	if got.Usage.InputTokens != 75 {
		t.Fatalf("expected input tokens to exclude cached tokens, got %d", got.Usage.InputTokens)
	}
	if got.Usage.CacheReadInputTokens != 45 {
		t.Fatalf("expected cached token count to be preserved, got %d", got.Usage.CacheReadInputTokens)
	}
}
