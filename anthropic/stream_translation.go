package anthropic

import (
	"encoding/json"
	"fmt"
	"time"
)

// jsonUnmarshal is a package-level alias to avoid import in translate_response.go
var jsonUnmarshal = json.Unmarshal

// TranslateChunkToAnthropicEvents converts an OpenAI stream chunk to Anthropic SSE events.
func TranslateChunkToAnthropicEvents(chunk ChatCompletionResponse, state *AnthropicStreamState) []StreamEvent {
	var events []StreamEvent

	if chunk.Model != "" {
		state.Model = chunk.Model
	}
	if chunk.ID != "" {
		state.ID = chunk.ID
	}

	if chunk.Usage != nil {
		cachedTokens := 0
		if chunk.Usage.PromptTokensDetails != nil {
			cachedTokens = chunk.Usage.PromptTokensDetails.CachedTokens
		}
		state.InputTokens = maxInt(chunk.Usage.PromptTokens-cachedTokens, 0)
		state.OutputTokens = chunk.Usage.CompletionTokens
		state.CacheReadInputTokens = cachedTokens
	}

	// Send message_start on first chunk
	if !state.MessageStartSent {
		state.MessageStartSent = true
		id := state.ID
		if id == "" {
			id = fmt.Sprintf("msg_%d", time.Now().UnixNano())
		}
		events = append(events, StreamEvent{
			Event: "message_start",
			Data: MessageStartEvent{
				Type: "message_start",
				Message: AnthropicResponse{
					ID:      id,
					Type:    "message",
					Role:    "assistant",
					Model:   state.Model,
					Content: []AnthropicContentBlock{},
					Usage: AnthropicUsage{
						InputTokens:          state.InputTokens,
						OutputTokens:         0,
						CacheReadInputTokens: state.CacheReadInputTokens,
					},
				},
			},
		})
		events = append(events, StreamEvent{
			Event: "ping",
			Data:  PingEvent{Type: "ping"},
		})
	}

	for _, choice := range chunk.Choices {
		delta := choice.Delta
		if delta == nil {
			// Check for finish_reason with no delta
			if choice.FinishReason != nil {
				events = append(events, closeContentBlock(state)...)
				events = append(events, StreamEvent{
					Event: "message_delta",
					Data: MessageDeltaEvent{
						Type: "message_delta",
						Delta: MessageDelta{
							StopReason: MapOpenAIStopReasonToAnthropic(*choice.FinishReason),
						},
						Usage: &DeltaUsage{
							InputTokens:          state.InputTokens,
							OutputTokens:         state.OutputTokens,
							CacheReadInputTokens: state.CacheReadInputTokens,
						},
					},
				})
			}
			continue
		}

		// Handle text content
		if delta.Content != "" {
			if !state.ContentBlockOpen || state.OpenBlockIsTool {
				// No block open, or a tool_use block is open — start a new text block.
				events = append(events, closeContentBlock(state)...)
				events = append(events, StreamEvent{
					Event: "content_block_start",
					Data: ContentBlockStartEvent{
						Type:  "content_block_start",
						Index: state.ContentBlockIndex,
						ContentBlock: AnthropicContentBlock{
							Type: "text",
							Text: "",
						},
					},
				})
				state.ContentBlockOpen = true
				state.OpenBlockIsTool = false
			}
			events = append(events, StreamEvent{
				Event: "content_block_delta",
				Data: ContentBlockDeltaEvent{
					Type:  "content_block_delta",
					Index: state.ContentBlockIndex,
					Delta: DeltaBlock{
						Type: "text_delta",
						Text: delta.Content,
					},
				},
			})
		}

		// Handle tool calls
		if len(delta.ToolCalls) > 0 {
			for _, tc := range delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}

				if tc.ID != "" {
					// New tool call starting
					events = append(events, closeContentBlock(state)...)
					state.ToolCalls[idx] = &ToolCallState{
						ID:   tc.ID,
						Name: tc.Function.Name,
					}
					events = append(events, StreamEvent{
						Event: "content_block_start",
						Data: ContentBlockStartEvent{
							Type:  "content_block_start",
							Index: state.ContentBlockIndex,
							ContentBlock: AnthropicContentBlock{
								Type: "tool_use",
								ID:   tc.ID,
								Name: tc.Function.Name,
							},
						},
					})
					state.ContentBlockOpen = true
					state.OpenBlockIsTool = true
				}

				if tc.Function.Arguments != "" {
					if tcs, ok := state.ToolCalls[idx]; ok {
						tcs.Arguments += tc.Function.Arguments
					}
					events = append(events, StreamEvent{
						Event: "content_block_delta",
						Data: ContentBlockDeltaEvent{
							Type:  "content_block_delta",
							Index: state.ContentBlockIndex,
							Delta: DeltaBlock{
								Type:        "input_json_delta",
								PartialJSON: tc.Function.Arguments,
							},
						},
					})
				}
			}
		}

		// Handle finish reason
		if choice.FinishReason != nil {
			events = append(events, closeContentBlock(state)...)
			events = append(events, StreamEvent{
				Event: "message_delta",
				Data: MessageDeltaEvent{
					Type: "message_delta",
					Delta: MessageDelta{
						StopReason: MapOpenAIStopReasonToAnthropic(*choice.FinishReason),
					},
					Usage: &DeltaUsage{
						InputTokens:          state.InputTokens,
						OutputTokens:         state.OutputTokens,
						CacheReadInputTokens: state.CacheReadInputTokens,
					},
				},
			})
		}
	}

	return events
}

func closeContentBlock(state *AnthropicStreamState) []StreamEvent {
	if !state.ContentBlockOpen {
		return nil
	}
	state.ContentBlockOpen = false
	event := StreamEvent{
		Event: "content_block_stop",
		Data: ContentBlockStopEvent{
			Type:  "content_block_stop",
			Index: state.ContentBlockIndex,
		},
	}
	state.ContentBlockIndex++
	return []StreamEvent{event}
}
