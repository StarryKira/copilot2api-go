package anthropic

import "testing"

func TestTranslateToOpenAINormalizesAnthropicModelAndMetadata(t *testing.T) {
	payload := AnthropicMessagesPayload{
		Model:    "claude-sonnet-4-20250514",
		Messages: []AnthropicMessage{{Role: "user", Content: "hello"}},
		Metadata: &Metadata{UserID: "user-123"},
	}

	got := TranslateToOpenAI(payload)

	if got.Model != "claude-sonnet-4" {
		t.Fatalf("expected normalized model claude-sonnet-4, got %q", got.Model)
	}
	if got.User != "user-123" {
		t.Fatalf("expected user metadata to be forwarded, got %q", got.User)
	}
}
