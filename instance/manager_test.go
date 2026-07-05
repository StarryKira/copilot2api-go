package instance

import (
	"testing"

	sdk "github.com/github/copilot-sdk/go"
)

func TestSDKModelsToResponse(t *testing.T) {
	prompt := 128000
	window := 200000
	models := []sdk.ModelInfo{
		{
			ID:   "claude-opus-4.8",
			Name: "Claude Opus 4.8",
			Capabilities: sdk.ModelCapabilities{
				Limits: sdk.ModelLimits{
					MaxPromptTokens:        &prompt,
					MaxContextWindowTokens: &window,
				},
			},
		},
		{ID: "gpt-5-mini", Name: "GPT-5 mini"},
	}

	resp := sdkModelsToResponse(models)
	if resp.Object != "list" || len(resp.Data) != 2 {
		t.Fatalf("unexpected response shape: %+v", resp)
	}

	first := resp.Data[0]
	if first.ID != "claude-opus-4.8" || first.Name != "Claude Opus 4.8" || first.Object != "model" {
		t.Fatalf("unexpected first entry: %+v", first)
	}
	if first.Capabilities == nil ||
		first.Capabilities.Limits.MaxPromptTokens != prompt ||
		first.Capabilities.Limits.MaxContextWindow != window {
		t.Fatalf("limits not carried over: %+v", first.Capabilities)
	}

	second := resp.Data[1]
	if second.Capabilities != nil {
		t.Fatalf("expected no capabilities when SDK reports no limits, got %+v", second.Capabilities)
	}
}
