package instance

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"

	"copilot-go/anthropic"
	"copilot-go/config"
	"copilot-go/store"

	"github.com/gin-gonic/gin"
	sdk "github.com/github/copilot-sdk/go"
)

// DoCompletionsProxy performs the upstream request for completions via the official SDK.
// The caller is responsible for closing resp.Body.
func DoCompletionsProxy(c *gin.Context, _ *config.State, sdkClient *sdk.Client, bodyBytes []byte) (*http.Response, error) {
	return SDKDoCompletions(c.Request.Context(), sdkClient, bodyBytes)
}

// ForwardCompletionsResponse writes the upstream response to the client.
func ForwardCompletionsResponse(c *gin.Context, resp *http.Response) {
	defer func() { _ = resp.Body.Close() }()

	contentType := resp.Header.Get("Content-Type")
	isStream := strings.Contains(contentType, "text/event-stream")

	if isStream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		c.Status(resp.StatusCode)

		reader := bufio.NewReaderSize(resp.Body, 10*1024*1024)
		c.Stream(func(w io.Writer) bool {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				if _, writeErr := w.Write(line); writeErr != nil {
					return false
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("Stream read error: %v", err)
				}
				return false
			}
			return true
		})
	} else {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read response"})
			return
		}
		c.Data(resp.StatusCode, "application/json", body)
	}
}

// ModelsHandler returns cached models with display ID mapping. Models the SDK
// accepts in session.create take precedence over the REST /models list, which
// contains entries that cannot actually be used through this proxy.
func ModelsHandler(c *gin.Context, state *config.State) {
	state.RLock()
	models := state.SDKModels
	if models == nil {
		models = state.Models
	}
	state.RUnlock()

	if models == nil {
		c.JSON(http.StatusOK, config.ModelsResponse{
			Object: "list",
			Data:   []config.ModelEntry{},
		})
		return
	}

	mapped := config.ModelsResponse{
		Object: models.Object,
		Data:   make([]config.ModelEntry, len(models.Data)),
	}
	for i, m := range models.Data {
		mapped.Data[i] = config.ModelEntry{
			ID:           store.ToDisplayID(m.ID),
			Object:       m.Object,
			Created:      m.Created,
			OwnedBy:      m.OwnedBy,
			Name:         m.Name,
			Version:      m.Version,
			Vendor:       m.Vendor,
			Capabilities: m.Capabilities,
		}
	}

	c.JSON(http.StatusOK, mapped)
}

// DoEmbeddingsProxy performs the upstream request for embeddings.
func DoEmbeddingsProxy(state *config.State, bodyBytes []byte) (*http.Response, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &payload); err == nil {
		if model, ok := payload["model"].(string); ok {
			payload["model"] = store.ToCopilotID(model)
			bodyBytes, _ = json.Marshal(payload)
		}
	}

	return ProxyRequestWithBytes(state, "POST", "/embeddings", bodyBytes, nil, false)
}

// ForwardEmbeddingsResponse writes the upstream embeddings response to the client.
func ForwardEmbeddingsResponse(c *gin.Context, resp *http.Response) {
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read response"})
		return
	}
	c.Data(resp.StatusCode, "application/json", body)
}

// DoMessagesProxy performs the upstream request for Anthropic messages via the official SDK.
// Returns the raw response. bodyBytes is the original Anthropic payload.
func DoMessagesProxy(c *gin.Context, state *config.State, sdkClient *sdk.Client, bodyBytes []byte) (*http.Response, error) {
	var anthropicPayload anthropic.AnthropicMessagesPayload
	if err := json.Unmarshal(bodyBytes, &anthropicPayload); err != nil {
		return nil, fmt.Errorf("invalid request: %v", err)
	}

	// Auto-fill max_tokens from model capabilities if not provided
	if anthropicPayload.MaxTokens == 0 {
		copilotModelID := anthropic.NormalizeAnthropicModel(store.ToCopilotID(anthropicPayload.Model))
		if limit := lookupMaxOutputTokens(state, copilotModelID); limit > 0 {
			anthropicPayload.MaxTokens = limit
		}
	}

	openaiPayload := anthropic.TranslateToOpenAI(anthropicPayload)

	openaiBytes, err := json.Marshal(openaiPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %v", err)
	}

	return SDKDoCompletions(c.Request.Context(), sdkClient, openaiBytes)
}

// ForwardMessagesResponse writes the upstream response to the client in Anthropic format.
// originalBody is the original Anthropic request (used to determine stream mode).
func ForwardMessagesResponse(c *gin.Context, resp *http.Response, originalBody []byte) {
	defer func() { _ = resp.Body.Close() }()

	var anthropicPayload anthropic.AnthropicMessagesPayload
	if err := json.Unmarshal(originalBody, &anthropicPayload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid request: %v", err)})
		return
	}

	if anthropicPayload.Stream {
		handleAnthropicStream(c, resp)
	} else {
		handleAnthropicNonStream(c, resp)
	}
}

func handleAnthropicNonStream(c *gin.Context, resp *http.Response) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read response"})
		return
	}

	if resp.StatusCode != 200 {
		c.Data(resp.StatusCode, "application/json", body)
		return
	}

	var openaiResp anthropic.ChatCompletionResponse
	if err := json.Unmarshal(body, &openaiResp); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to parse upstream response"})
		return
	}

	anthropicResp := anthropic.TranslateToAnthropic(openaiResp)
	c.JSON(http.StatusOK, anthropicResp)
}

func handleAnthropicStream(c *gin.Context, resp *http.Response) {
	// If upstream returned an error, translate it properly instead of trying to SSE-parse
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[Stream] Upstream returned status %d: %s", resp.StatusCode, string(body))
		c.Data(resp.StatusCode, "application/json", body)
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Header("Transfer-Encoding", "chunked")
	c.Status(http.StatusOK)

	w := c.Writer
	flusher, hasFlusher := w.(http.Flusher)
	clientGone := c.Request.Context().Done()

	state := anthropic.NewStreamState()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 10*1024*1024), 10*1024*1024)

	for scanner.Scan() {
		select {
		case <-clientGone:
			log.Printf("[Stream] Client disconnected, stopping stream")
			return
		default:
		}

		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			if err := writeSSE(w, "message_stop", map[string]string{"type": "message_stop"}); err != nil {
				log.Printf("[Stream] Write error on message_stop: %v", err)
				return
			}
			if hasFlusher {
				flusher.Flush()
			}
			return
		}

		var chunk anthropic.ChatCompletionResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			log.Printf("[Stream] Failed to parse SSE chunk: %v", err)
			continue
		}

		events := anthropic.TranslateChunkToAnthropicEvents(chunk, state)
		for _, event := range events {
			if err := writeSSE(w, event.Event, event.Data); err != nil {
				log.Printf("[Stream] Write error: %v", err)
				return
			}
		}
		if hasFlusher {
			flusher.Flush()
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("[Stream] Scanner error: %v", err)
		_ = writeSSE(w, "error", map[string]interface{}{
			"type": "error",
			"error": map[string]string{
				"type":    "stream_error",
				"message": fmt.Sprintf("upstream stream error: %v", err),
			},
		})
	} else {
		log.Printf("[Stream] Upstream closed without [DONE], sending message_stop")
		_ = writeSSE(w, "message_stop", map[string]string{"type": "message_stop"})
	}
	if hasFlusher {
		flusher.Flush()
	}
}

// lookupMaxOutputTokens finds the max_output_tokens for a model from cached capabilities.
func lookupMaxOutputTokens(state *config.State, modelID string) int {
	if state == nil || modelID == "" {
		return 0
	}
	state.RLock()
	models := state.Models
	state.RUnlock()
	if models == nil {
		return 0
	}
	for _, m := range models.Data {
		if m.ID == modelID && m.Capabilities != nil && m.Capabilities.Limits.MaxOutputTokens > 0 {
			return m.Capabilities.Limits.MaxOutputTokens
		}
	}
	return 0
}

func writeSSE(w io.Writer, event string, data interface{}) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	return err
}

// CountTokensHandler provides a simplified token count estimation.
func CountTokensHandler(c *gin.Context, _ *config.State) {
	anthropicBeta := c.GetHeader("anthropic-beta")

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	var payload anthropic.AnthropicMessagesPayload
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid request: %v", err)})
		return
	}

	openaiPayload := anthropic.TranslateToOpenAI(payload)
	inputTokens, outputTokens := estimateOpenAITokens(openaiPayload)

	if len(payload.Tools) > 0 && !hasClaudeCodeMCPTools(anthropicBeta, payload.Tools) {
		switch {
		case strings.HasPrefix(payload.Model, "claude"):
			inputTokens += 346
		case strings.HasPrefix(payload.Model, "grok"):
			inputTokens += 480
		}
	}

	finalTokenCount := inputTokens + outputTokens
	switch {
	case strings.HasPrefix(payload.Model, "claude"):
		finalTokenCount = int(math.Round(float64(finalTokenCount) * 1.15))
	case strings.HasPrefix(payload.Model, "grok"):
		finalTokenCount = int(math.Round(float64(finalTokenCount) * 1.03))
	}
	finalTokenCount = maxTokenCount(finalTokenCount, 1)

	c.JSON(http.StatusOK, gin.H{
		"input_tokens": finalTokenCount,
	})
}

func estimateOpenAITokens(payload anthropic.ChatCompletionsPayload) (int, int) {
	inputTokens := 0
	outputTokens := 0

	for _, msg := range payload.Messages {
		tokens := estimateJSONTokens(msg) + 3
		if msg.Role == "assistant" {
			outputTokens += tokens
		} else {
			inputTokens += tokens
		}
	}

	if len(payload.Tools) > 0 {
		inputTokens += estimateJSONTokens(payload.Tools)
	}

	return inputTokens, outputTokens
}

func estimateJSONTokens(v interface{}) int {
	data, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	tokens := len(data) / 4
	if tokens < 1 {
		return 1
	}
	return tokens
}

func maxTokenCount(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func hasClaudeCodeMCPTools(anthropicBeta string, tools []anthropic.AnthropicTool) bool {
	if !strings.HasPrefix(anthropicBeta, "claude-code") {
		return false
	}
	for _, tool := range tools {
		if strings.HasPrefix(tool.Name, "mcp__") {
			return true
		}
	}
	return false
}
