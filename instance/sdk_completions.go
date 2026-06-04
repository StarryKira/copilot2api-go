package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	sdk "github.com/github/copilot-sdk/go"
)

// SDKDoCompletions handles /chat/completions via the official Copilot CLI SDK.
// bodyBytes must be OpenAI-format JSON.
func SDKDoCompletions(ctx context.Context, sdkClient *sdk.Client, bodyBytes []byte) (*http.Response, error) {
	var req struct {
		Model    string        `json:"model"`
		Messages []interface{} `json:"messages"`
		Stream   bool          `json:"stream"`
	}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	system, prompt := buildSDKPrompt(req.Messages)

	sessionCfg := &sdk.SessionConfig{
		Model:               req.Model,
		Streaming:           sdk.Bool(req.Stream),
		OnPermissionRequest: sdk.PermissionHandler.ApproveAll,
	}
	if system != "" {
		sessionCfg.SystemMessage = &sdk.SystemMessageConfig{
			Content: system,
			Mode:    "replace",
		}
	}

	session, err := sdkClient.CreateSession(ctx, sessionCfg)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	if req.Stream {
		return sdkStream(ctx, session, prompt, req.Model), nil
	}
	return sdkSync(ctx, session, prompt, req.Model)
}

func buildSDKPrompt(rawMessages []interface{}) (system, prompt string) {
	var sysLines []string
	var historyLines []string

	for _, raw := range rawMessages {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		text := extractMsgText(msg["content"])
		switch role {
		case "system":
			sysLines = append(sysLines, text)
		case "user":
			historyLines = append(historyLines, "User: "+text)
		case "assistant":
			historyLines = append(historyLines, "Assistant: "+text)
		case "tool":
			historyLines = append(historyLines, "Tool: "+text)
		}
	}

	system = strings.Join(sysLines, "\n")

	switch len(historyLines) {
	case 0:
		return system, ""
	case 1:
		return system, strings.TrimPrefix(historyLines[0], "User: ")
	default:
		return system, strings.Join(historyLines, "\n")
	}
}

func extractMsgText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, item := range v {
			part, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if t, _ := part["type"].(string); t == "text" {
				if text, _ := part["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// sdkStream returns a synthetic streaming http.Response backed by an io.Pipe.
func sdkStream(ctx context.Context, session *sdk.Session, prompt, model string) *http.Response {
	pr, pw := io.Pipe()
	go func() {
		defer session.Disconnect() //nolint:errcheck

		done := make(chan struct{}, 1)
		var once sync.Once
		markDone := func() { once.Do(func() { close(done) }) }

		unsubscribe := session.On(func(event sdk.SessionEvent) {
			switch d := event.Data.(type) {
			case *sdk.AssistantMessageDeltaData:
				if _, err := fmt.Fprintf(pw, "data: %s\n\n", marshalChunk(model, d.DeltaContent, "")); err != nil {
					markDone()
				}
			case *sdk.SessionIdleData:
				_ = d
				fmt.Fprintf(pw, "data: %s\n\ndata: [DONE]\n\n", marshalChunk(model, "", "stop")) //nolint:errcheck
				markDone()
			}
		})

		if _, err := session.Send(ctx, sdk.MessageOptions{Prompt: prompt}); err != nil {
			unsubscribe()
			pw.CloseWithError(fmt.Errorf("send: %w", err))
			return
		}

		select {
		case <-done:
			unsubscribe()
			pw.Close()
		case <-ctx.Done():
			unsubscribe()
			_ = session.Abort(context.Background())
			pw.CloseWithError(ctx.Err())
		}
	}()

	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
	}
}

// sdkSync returns a synthetic non-streaming http.Response.
func sdkSync(ctx context.Context, session *sdk.Session, prompt, model string) (*http.Response, error) {
	defer session.Disconnect() //nolint:errcheck

	reply, err := session.SendAndWait(ctx, sdk.MessageOptions{Prompt: prompt})
	if err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}

	content := ""
	if reply != nil {
		if d, ok := reply.Data.(*sdk.AssistantMessageData); ok {
			content = d.Content
		}
	}

	body, _ := json.Marshal(map[string]interface{}{
		"id":     "chatcmpl-sdk",
		"object": "chat.completion",
		"model":  model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       map[string]interface{}{"role": "assistant", "content": content},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
		},
	})

	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}, nil
}

func marshalChunk(model, content, finishReason string) string {
	delta := map[string]interface{}{}
	if content != "" {
		delta["content"] = content
	}
	var fr interface{}
	if finishReason != "" {
		fr = finishReason
	}
	data, _ := json.Marshal(map[string]interface{}{
		"id":     "chatcmpl-sdk",
		"object": "chat.completion.chunk",
		"model":  model,
		"choices": []map[string]interface{}{
			{"index": 0, "delta": delta, "finish_reason": fr},
		},
	})
	return string(data)
}
