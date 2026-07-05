package config

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	CopilotVersion   = "0.26.7"
	GithubClientID   = "Iv1.b507a08c87ecfe98"
	GithubAPIVersion = "2025-04-01"

	CopilotIndividualChatURL = "https://api.githubcopilot.com"

	GithubCopilotURL = "https://api.github.com/copilot_internal/v2/token"
	GithubDeviceURL  = "https://github.com/login/device/code"
	GithubTokenURL   = "https://github.com/login/oauth/access_token"
	GithubUserURL    = "https://api.github.com/user"
)

// proxyURL is the global outbound HTTP proxy. Protected by proxyMu.
var (
	proxyMu  sync.RWMutex
	proxyURL string
)

func SetProxyURL(u string) {
	proxyMu.Lock()
	proxyURL = u
	proxyMu.Unlock()
}

func GetProxyURL() string {
	proxyMu.RLock()
	u := proxyURL
	proxyMu.RUnlock()
	return u
}

// NewHTTPClient creates an HTTP client with the current proxy setting and given timeout.
func NewHTTPClient(timeout time.Duration) *http.Client {
	t := &http.Transport{
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if pURL := GetProxyURL(); pURL != "" {
		if parsed, err := url.Parse(pURL); err == nil {
			t.Proxy = http.ProxyURL(parsed)
		}
	}
	return &http.Client{Timeout: timeout, Transport: t}
}

type ModelsResponse struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}

type ModelCapabilities struct {
	Family string      `json:"family,omitempty"`
	Limits ModelLimits `json:"limits,omitempty"`
	Type   string      `json:"type,omitempty"`
}

type ModelLimits struct {
	MaxOutputTokens  int `json:"max_output_tokens,omitempty"`
	MaxPromptTokens  int `json:"max_prompt_tokens,omitempty"`
	MaxContextWindow int `json:"max_context_window,omitempty"`
}

type ModelEntry struct {
	ID           string             `json:"id"`
	Object       string             `json:"object"`
	Created      int64              `json:"created"`
	OwnedBy      string             `json:"owned_by"`
	Name         string             `json:"name,omitempty"`
	Version      string             `json:"version,omitempty"`
	Vendor       string             `json:"vendor,omitempty"`
	Capabilities *ModelCapabilities `json:"capabilities,omitempty"`
}

type CopilotTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

type State struct {
	mu             sync.RWMutex
	GithubToken    string
	CopilotToken   string
	TokenExpiresAt int64 // Unix timestamp when the Copilot token expires
	AccountType    string
	Models         *ModelsResponse
	// SDKModels are the models the Copilot CLI SDK accepts in session.create —
	// a different (usually smaller) set than the REST /models list. Clients
	// must pick from it, so it takes precedence wherever models are listed;
	// Models remains as fallback and for max_output_tokens lookup.
	SDKModels     *ModelsResponse
	VSCodeVersion string
}

func NewState() *State {
	return &State{}
}

func (s *State) Lock()    { s.mu.Lock() }
func (s *State) Unlock()  { s.mu.Unlock() }
func (s *State) RLock()   { s.mu.RLock() }
func (s *State) RUnlock() { s.mu.RUnlock() }

func CopilotBaseURL(accountType string) string {
	if accountType == "" || accountType == "individual" {
		return CopilotIndividualChatURL
	}
	return fmt.Sprintf("https://api.%s.githubcopilot.com", accountType)
}

func CopilotHeaders(state *State, vision bool) http.Header {
	state.RLock()
	defer state.RUnlock()

	h := make(http.Header)
	h.Set("Authorization", "Bearer "+state.CopilotToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("Copilot-Integration-Id", "vscode-chat")
	h.Set("Editor-Version", "vscode/"+state.VSCodeVersion)
	h.Set("Editor-Plugin-Version", "copilot-chat/"+CopilotVersion)
	h.Set("User-Agent", fmt.Sprintf("GitHubCopilotChat/%s", CopilotVersion))
	h.Set("Openai-Intent", "conversation-panel")
	h.Set("X-GitHub-API-Version", GithubAPIVersion)
	h.Set("X-Request-Id", uuid.NewString())
	h.Set("X-Vscode-User-Agent-Library-Version", "electron-fetch")
	if vision {
		h.Set("Copilot-Vision-Request", "true")
	}
	return h
}

func GithubHeaders(state *State) http.Header {
	state.RLock()
	defer state.RUnlock()

	h := make(http.Header)
	h.Set("Authorization", "token "+state.GithubToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("Editor-Version", "vscode/"+state.VSCodeVersion)
	h.Set("Editor-Plugin-Version", "copilot-chat/"+CopilotVersion)
	h.Set("User-Agent", fmt.Sprintf("GitHubCopilotChat/%s", CopilotVersion))
	h.Set("X-GitHub-API-Version", GithubAPIVersion)
	h.Set("X-Vscode-User-Agent-Library-Version", "electron-fetch")
	return h
}
