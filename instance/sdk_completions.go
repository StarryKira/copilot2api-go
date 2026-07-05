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

	"copilot-go/anthropic"

	sdk "github.com/github/copilot-sdk/go"
)

// Pseudo-MCP tool-call protocol markers. The official Copilot CLI SDK runs its
// own agent loop and only streams assistant *text* back to us; it never hands us
// native OpenAI tool_calls. So instead we describe the caller's tools in the
// system prompt and instruct the model to emit tool calls as text wrapped in
// these markers, then parse them back into OpenAI tool_calls here.
const (
	mcpOpenTag  = "[mcp.result]"
	mcpCloseTag = "[/mcp.result]"
)

// SDKDoCompletions handles /chat/completions via the official Copilot CLI SDK.
// bodyBytes must be OpenAI-format JSON.
func SDKDoCompletions(ctx context.Context, sdkClient *sdk.Client, bodyBytes []byte) (*http.Response, error) {
	var req struct {
		Model    string                 `json:"model"`
		Messages []interface{}          `json:"messages"`
		Stream   bool                   `json:"stream"`
		Tools    []anthropic.OpenAITool `json:"tools"`
	}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	system, prompt := buildSDKPrompt(req.Messages)
	if len(req.Tools) > 0 {
		instr := buildToolInstructions(req.Tools)
		if system == "" {
			system = instr
		} else {
			system = system + "\n\n" + instr
		}
	}

	// Always replace the SDK's system message. The default one describes the
	// SDK host process's environment (cwd, directory listing, git root), which
	// makes the model answer about the proxy's own directory instead of the
	// caller's context (issue #14).
	if system == "" {
		system = "You are a helpful assistant."
	}

	sessionCfg := &sdk.SessionConfig{
		Model:               req.Model,
		Streaming:           sdk.Bool(req.Stream),
		OnPermissionRequest: sdk.PermissionHandler.ApproveAll,
		// We are a stateless proxy: the SDK must never run its own built-in or
		// MCP tools against the host. Tool calls are surfaced to the client via
		// the [mcp.result] text protocol instead.
		ExcludedTools: sdk.NewToolSet().AddBuiltIn("*").AddMCP("*").ToSlice(),
		SystemMessage: &sdk.SystemMessageConfig{
			Content: system,
			Mode:    "replace",
		},
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

// buildToolInstructions renders the tool catalogue and the text protocol the
// model must follow to invoke a tool.
func buildToolInstructions(tools []anthropic.OpenAITool) string {
	var b strings.Builder
	b.WriteString("# Tool Calling Protocol\n")
	b.WriteString("You can call the tools listed below. When you decide to call a tool, output one or more blocks EXACTLY in this format:\n")
	b.WriteString(mcpOpenTag + `{"name":"<tool_name>","arguments":{<arguments as a JSON object>}}` + mcpCloseTag + "\n")
	b.WriteString("Rules:\n")
	b.WriteString("- `arguments` MUST be a valid JSON object matching that tool's input schema.\n")
	b.WriteString("- Every block MUST end with the closing tag " + mcpCloseTag + " — never stop before writing it.\n")
	b.WriteString("- Emit one block per tool call; you may emit several blocks to call multiple tools.\n")
	b.WriteString("- Do NOT wrap the block in code fences and do NOT invent tools that are not listed.\n")
	b.WriteString("- After emitting tool-call block(s), stop and wait for the tool result before continuing.\n\n")
	b.WriteString("Available tools:\n")
	for _, t := range tools {
		schema := "{}"
		if t.Function.Parameters != nil {
			if data, err := json.Marshal(t.Function.Parameters); err == nil {
				schema = string(data)
			}
		}
		fmt.Fprintf(&b, "- %s: %s\n  input schema: %s\n", t.Function.Name, t.Function.Description, schema)
	}
	return b.String()
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
		switch role {
		case "system":
			if t := extractMsgText(msg["content"]); t != "" {
				sysLines = append(sysLines, t)
			}
		case "user":
			if t := extractMsgText(msg["content"]); t != "" {
				historyLines = append(historyLines, "User: "+t)
			}
		case "assistant":
			line := "Assistant:"
			if t := extractMsgText(msg["content"]); t != "" {
				line += " " + t
			}
			// Re-render prior tool calls in the same text protocol so the model
			// sees what it previously requested.
			if calls := renderAssistantToolCalls(msg["tool_calls"]); calls != "" {
				line += "\n" + calls
			}
			historyLines = append(historyLines, line)
		case "tool":
			id, _ := msg["tool_call_id"].(string)
			content := extractMsgText(msg["content"])
			if id != "" {
				historyLines = append(historyLines, fmt.Sprintf("Tool result (%s): %s", id, content))
			} else {
				historyLines = append(historyLines, "Tool result: "+content)
			}
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

// renderAssistantToolCalls turns OpenAI assistant tool_calls (raw JSON maps)
// back into [mcp.result] protocol blocks.
func renderAssistantToolCalls(raw interface{}) string {
	arr, ok := raw.([]interface{})
	if !ok || len(arr) == 0 {
		return ""
	}
	var blocks []string
	for _, item := range arr {
		tc, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		fn, _ := tc["function"].(map[string]interface{})
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		argsStr, _ := fn["arguments"].(string)
		var argsVal interface{}
		if argsStr != "" {
			_ = json.Unmarshal([]byte(argsStr), &argsVal)
		}
		if argsVal == nil {
			argsVal = map[string]interface{}{}
		}
		payload, err := json.Marshal(map[string]interface{}{"name": name, "arguments": argsVal})
		if err != nil {
			continue
		}
		blocks = append(blocks, mcpOpenTag+string(payload)+mcpCloseTag)
	}
	return strings.Join(blocks, "\n")
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

// mcpParser is a streaming-safe parser that extracts [mcp.result]...[/mcp.result]
// tool-call blocks out of a text stream, emitting plain text and parsed tool
// calls via callbacks. It tolerates blocks being split across feed() calls.
type mcpParser struct {
	pending string
	toolSeq int
	sawTool bool
}

// feed processes a new chunk of text. onText receives plain text outside any
// marker; onTool receives a fully parsed tool call.
func (p *mcpParser) feed(s string, onText func(string), onTool func(idx int, id, name, args string)) {
	p.pending += s
	for {
		open := strings.Index(p.pending, mcpOpenTag)
		if open == -1 {
			// No open tag in view. Flush everything except a suffix that could
			// be the beginning of an open tag split across chunks.
			keep := partialOpenSuffix(p.pending)
			if flush := p.pending[:len(p.pending)-keep]; flush != "" {
				onText(flush)
			}
			p.pending = p.pending[len(p.pending)-keep:]
			return
		}
		if open > 0 {
			onText(p.pending[:open])
		}
		rest := p.pending[open+len(mcpOpenTag):]
		closeIdx := strings.Index(rest, mcpCloseTag)
		if closeIdx == -1 {
			// Incomplete tool block; wait for more input.
			p.pending = p.pending[open:]
			return
		}
		block := rest[:closeIdx]
		if !p.handleTool(strings.TrimSpace(block), onTool) {
			// Not a valid tool call — surface the raw block so nothing is lost.
			onText(mcpOpenTag + block + mcpCloseTag)
		}
		p.pending = rest[closeIdx+len(mcpCloseTag):]
	}
}

func (p *mcpParser) handleTool(jsonStr string, onTool func(idx int, id, name, args string)) bool {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &call); err != nil || call.Name == "" {
		return false
	}
	args := strings.TrimSpace(string(call.Arguments))
	if args == "" || args == "null" {
		args = "{}"
	}
	onTool(p.toolSeq, fmt.Sprintf("call_%d", p.toolSeq), call.Name, args)
	p.toolSeq++
	p.sawTool = true
	return true
}

// flushRemaining drains the buffer at end of stream. Models often stop right
// after the JSON payload without writing the close tag (observed live with
// "auto"), so an unterminated block whose payload parses cleanly is salvaged
// as a tool call; anything else is emitted as text so no content is dropped.
func (p *mcpParser) flushRemaining(onText func(string), onTool func(idx int, id, name, args string)) {
	if p.pending == "" {
		return
	}
	if inner, ok := strings.CutPrefix(p.pending, mcpOpenTag); ok {
		inner = strings.TrimSpace(inner)
		// Tolerate a truncated close tag after the payload.
		for k := len(mcpCloseTag) - 1; k > 0; k-- {
			if strings.HasSuffix(inner, mcpCloseTag[:k]) {
				inner = strings.TrimSpace(inner[:len(inner)-k])
				break
			}
		}
		if p.handleTool(inner, onTool) {
			p.pending = ""
			return
		}
	}
	onText(p.pending)
	p.pending = ""
}

// partialOpenSuffix returns the length of the longest suffix of s that is also
// a (proper) prefix of the open tag, so a marker split across chunks isn't
// leaked as plain text.
func partialOpenSuffix(s string) int {
	max := len(mcpOpenTag) - 1
	if max > len(s) {
		max = len(s)
	}
	for k := max; k > 0; k-- {
		if strings.HasSuffix(s, mcpOpenTag[:k]) {
			return k
		}
	}
	return 0
}

// sdkStream returns a synthetic streaming http.Response backed by an io.Pipe.
func sdkStream(ctx context.Context, session *sdk.Session, prompt, model string) *http.Response {
	pr, pw := io.Pipe()
	go func() {
		defer session.Disconnect() //nolint:errcheck

		done := make(chan struct{}, 1)
		var once sync.Once
		markDone := func() { once.Do(func() { close(done) }) }

		parser := &mcpParser{}
		var failed bool
		emit := func(s string) {
			if failed {
				return
			}
			if _, err := io.WriteString(pw, s); err != nil {
				failed = true
				markDone()
			}
		}
		writeText := func(text string) {
			if text == "" {
				return
			}
			emit(fmt.Sprintf("data: %s\n\n", marshalChunk(model, text, "")))
		}
		writeTool := func(idx int, id, name, args string) {
			emit(fmt.Sprintf("data: %s\n\n", marshalToolChunk(model, idx, id, name, args)))
		}

		unsubscribe := session.On(func(event sdk.SessionEvent) {
			switch d := event.Data.(type) {
			case *sdk.AssistantMessageDeltaData:
				parser.feed(d.DeltaContent, writeText, writeTool)
			case *sdk.SessionIdleData:
				_ = d
				parser.flushRemaining(writeText, writeTool)
				finish := "stop"
				if parser.sawTool {
					finish = "tool_calls"
				}
				emit(fmt.Sprintf("data: %s\n\ndata: [DONE]\n\n", marshalChunk(model, "", finish)))
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

	var textParts []string
	var toolCalls []map[string]interface{}
	parser := &mcpParser{}
	collectText := func(t string) { textParts = append(textParts, t) }
	collectTool := func(idx int, id, name, args string) {
		toolCalls = append(toolCalls, map[string]interface{}{
			"index": idx,
			"id":    id,
			"type":  "function",
			"function": map[string]interface{}{
				"name":      name,
				"arguments": args,
			},
		})
	}
	parser.feed(content, collectText, collectTool)
	parser.flushRemaining(collectText, collectTool)

	message := map[string]interface{}{
		"role":    "assistant",
		"content": strings.Join(textParts, ""),
	}
	finishReason := "stop"
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finishReason = "tool_calls"
	}

	body, _ := json.Marshal(map[string]interface{}{
		"id":     "chatcmpl-sdk",
		"object": "chat.completion",
		"model":  model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
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

// marshalToolChunk emits a streaming chunk carrying a single tool call. The
// arguments field is the raw JSON string, matching OpenAI's wire format and the
// downstream stream translator's expectations.
func marshalToolChunk(model string, index int, id, name, args string) string {
	delta := map[string]interface{}{
		"tool_calls": []map[string]interface{}{
			{
				"index": index,
				"id":    id,
				"type":  "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": args,
				},
			},
		},
	}
	data, _ := json.Marshal(map[string]interface{}{
		"id":     "chatcmpl-sdk",
		"object": "chat.completion.chunk",
		"model":  model,
		"choices": []map[string]interface{}{
			{"index": 0, "delta": delta, "finish_reason": nil},
		},
	})
	return string(data)
}
