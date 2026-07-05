package instance

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"copilot-go/anthropic"
	"copilot-go/config"

	"github.com/gin-gonic/gin"
)

func TestCountTokensHandlerAddsClaudeToolSurchargeForNonMCPTools(t *testing.T) {
	gin.SetMode(gin.TestMode)

	basePayload := anthropic.AnthropicMessagesPayload{
		Model:    "claude-sonnet-4",
		Messages: []anthropic.AnthropicMessage{{Role: "user", Content: "hello"}},
		Tools: []anthropic.AnthropicTool{{
			Name:        "get_weather",
			Description: "Get weather",
			InputSchema: map[string]interface{}{"type": "object"},
		}},
	}

	withSurcharge := performCountTokens(t, basePayload, "claude-code")

	basePayload.Tools[0].Name = "mcp__weather"
	withoutSurcharge := performCountTokens(t, basePayload, "claude-code")

	if withSurcharge <= withoutSurcharge {
		t.Fatalf("expected non-MCP Claude Code tools to have higher token estimate, got with=%d without=%d", withSurcharge, withoutSurcharge)
	}
}

func performCountTokens(t *testing.T, payload anthropic.AnthropicMessagesPayload, anthropicBeta string) int {
	t.Helper()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/messages/count_tokens", bytes.NewReader(body))
	if anthropicBeta != "" {
		c.Request.Header.Set("anthropic-beta", anthropicBeta)
	}

	CountTokensHandler(c, &config.State{})

	var resp struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.InputTokens
}
