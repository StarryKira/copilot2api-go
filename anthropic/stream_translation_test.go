package anthropic

import "testing"

func intPtr(v int) *int { return &v }

// Text deltas arriving after a tool_use block must share one new text block,
// not open a fresh block per delta.
func TestTextAfterToolCallSharesOneBlock(t *testing.T) {
	state := NewStreamState()
	feed := func(delta *ChoiceMsg) []StreamEvent {
		return TranslateChunkToAnthropicEvents(ChatCompletionResponse{
			ID: "x", Model: "m",
			Choices: []Choice{{Delta: delta}},
		}, state)
	}

	feed(&ChoiceMsg{Content: "before "})
	feed(&ChoiceMsg{ToolCalls: []ToolCall{{
		ID: "call_0", Type: "function", Index: intPtr(0),
		Function: FunctionCall{Name: "f", Arguments: `{"a":1}`},
	}}})
	feed(&ChoiceMsg{Content: "after "})
	extra := feed(&ChoiceMsg{Content: "more"})

	for _, ev := range extra {
		if ev.Event == "content_block_start" {
			t.Fatalf("second post-tool text delta opened a new block: %#v", ev.Data)
		}
	}
	if !state.ContentBlockOpen || state.OpenBlockIsTool {
		t.Fatalf("expected an open text block, state: %+v", state)
	}
	// Blocks so far: text(0), tool_use(1), text(2).
	if state.ContentBlockIndex != 2 {
		t.Fatalf("expected current block index 2, got %d", state.ContentBlockIndex)
	}
}

func TestTranslateChunkToAnthropicEventsCarriesCachedUsage(t *testing.T) {
	finishReason := "stop"
	events := TranslateChunkToAnthropicEvents(ChatCompletionResponse{
		ID:    "msg_1",
		Model: "claude-sonnet-4",
		Usage: &OpenAIUsage{
			PromptTokens:     100,
			CompletionTokens: 22,
			PromptTokensDetails: &PromptTokensDetails{
				CachedTokens: 40,
			},
		},
		Choices: []Choice{{
			Delta:        &ChoiceMsg{Content: "hello"},
			FinishReason: &finishReason,
		}},
	}, NewStreamState())

	messageStart, ok := events[0].Data.(MessageStartEvent)
	if !ok {
		t.Fatalf("expected first event to be MessageStartEvent, got %#v", events[0].Data)
	}
	if messageStart.Message.Usage.InputTokens != 60 {
		t.Fatalf("expected message_start input tokens to exclude cached tokens, got %d", messageStart.Message.Usage.InputTokens)
	}
	if messageStart.Message.Usage.CacheReadInputTokens != 40 {
		t.Fatalf("expected message_start cached token count, got %d", messageStart.Message.Usage.CacheReadInputTokens)
	}

	var deltaUsage *DeltaUsage
	for _, event := range events {
		if messageDelta, ok := event.Data.(MessageDeltaEvent); ok {
			deltaUsage = messageDelta.Usage
			break
		}
	}
	if deltaUsage == nil {
		t.Fatal("expected a message_delta event with usage")
	}
	if deltaUsage.InputTokens != 60 || deltaUsage.OutputTokens != 22 || deltaUsage.CacheReadInputTokens != 40 {
		t.Fatalf("unexpected message_delta usage: %+v", *deltaUsage)
	}
}
