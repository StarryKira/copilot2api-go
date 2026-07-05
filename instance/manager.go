package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"sync"
	"time"

	"copilot-go/config"
	vsutil "copilot-go/copilot"
	"copilot-go/store"

	sdk "github.com/github/copilot-sdk/go"
)

type ProxyInstance struct {
	Account   store.Account
	State     *config.State
	SDKClient *sdk.Client
	Status    string // "running", "stopped", "error"
	Error     string
	stopChan  chan struct{}
}

type CopilotUser struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

func StartInstance(account store.Account) error {
	mu.Lock()
	if inst, ok := instances[account.ID]; ok {
		if inst.Status == "running" {
			mu.Unlock()
			return nil
		}
	}
	mu.Unlock()

	state := config.NewState()
	state.Lock()
	state.GithubToken = account.GithubToken
	state.AccountType = account.AccountType
	state.Unlock()

	// Get VSCode version (still used for model/token HTTP calls)
	vsVer := vsutil.GetVSCodeVersion()
	state.Lock()
	state.VSCodeVersion = vsVer
	state.Unlock()

	// Get Copilot token
	if err := refreshCopilotToken(state); err != nil {
		mu.Lock()
		instances[account.ID] = &ProxyInstance{
			Account: account,
			State:   state,
			Status:  "error",
			Error:   err.Error(),
		}
		mu.Unlock()
		return err
	}

	// Get models
	if err := fetchModels(state); err != nil {
		log.Printf("Warning: failed to fetch models for account %s: %v", account.Name, err)
	}

	// Start official Copilot CLI SDK client
	sdkClient := sdk.NewClient(&sdk.ClientOptions{
		GitHubToken:     account.GithubToken,
		UseLoggedInUser: sdk.Bool(false),
	})
	if err := sdkClient.Start(context.Background()); err != nil {
		return fmt.Errorf("SDK client failed to start for %s: %w", account.Name, err)
	}

	// Cache the models the SDK actually accepts; /v1/models serves these.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := fetchSDKModels(ctx, sdkClient, state); err != nil {
			log.Printf("Warning: failed to fetch SDK models for account %s: %v", account.Name, err)
		}
		cancel()
	}

	stopChan := make(chan struct{})
	inst := &ProxyInstance{
		Account:   account,
		State:     state,
		SDKClient: sdkClient,
		Status:    "running",
		stopChan:  stopChan,
	}

	mu.Lock()
	instances[account.ID] = inst
	mu.Unlock()

	// Start background token refresh (still needed for models/embeddings endpoints)
	go tokenRefreshLoop(inst)

	log.Printf("Instance started for account: %s", account.Name)
	return nil
}

func StopInstance(accountID string) {
	mu.Lock()
	inst, ok := instances[accountID]
	if !ok {
		mu.Unlock()
		return
	}
	if inst.Status == "running" {
		close(inst.stopChan)
	}
	if inst.SDKClient != nil {
		_ = inst.SDKClient.Stop()
	}
	inst.Status = "stopped"
	mu.Unlock()
	log.Printf("Instance stopped for account: %s", inst.Account.Name)
}

// GetSDKClient returns the SDK client for the given account, or nil if not running.
func GetSDKClient(accountID string) *sdk.Client {
	mu.RLock()
	defer mu.RUnlock()
	if inst, ok := instances[accountID]; ok {
		return inst.SDKClient
	}
	return nil
}

func GetInstanceStatus(accountID string) string {
	mu.RLock()
	defer mu.RUnlock()
	if inst, ok := instances[accountID]; ok {
		return inst.Status
	}
	return "stopped"
}

func GetInstanceError(accountID string) string {
	mu.RLock()
	defer mu.RUnlock()
	if inst, ok := instances[accountID]; ok {
		return inst.Error
	}
	return ""
}

func GetInstanceState(accountID string) *config.State {
	mu.RLock()
	defer mu.RUnlock()
	if inst, ok := instances[accountID]; ok {
		return inst.State
	}
	return nil
}

// GetAllCachedModels collects and deduplicates model entries from all running instances.
func GetAllCachedModels() []config.ModelEntry {
	mu.RLock()
	defer mu.RUnlock()

	seen := make(map[string]bool)
	var result []config.ModelEntry
	for _, inst := range instances {
		if inst.Status != "running" {
			continue
		}
		inst.State.RLock()
		models := inst.State.SDKModels
		if models == nil {
			models = inst.State.Models
		}
		inst.State.RUnlock()
		if models == nil {
			continue
		}
		for _, m := range models.Data {
			if !seen[m.ID] {
				seen[m.ID] = true
				result = append(result, m)
			}
		}
	}
	return result
}

func GetUser(accountID string) (*CopilotUser, error) {
	state := GetInstanceState(accountID)
	if state == nil {
		return nil, fmt.Errorf("instance not found")
	}

	req, err := http.NewRequest("GET", config.GithubUserURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range config.GithubHeaders(state) {
		req.Header[k] = v
	}

	resp, err := getDefaultClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var user CopilotUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, err
	}
	return &user, nil
}

func tokenRefreshLoop(inst *ProxyInstance) {
	const fallbackInterval = 25 * time.Minute
	const minInterval = 30 * time.Second

	for {
		// Calculate next refresh time based on token expiry.
		sleepDur := fallbackInterval
		inst.State.RLock()
		expiresAt := inst.State.TokenExpiresAt
		inst.State.RUnlock()

		if expiresAt > 0 {
			remaining := time.Until(time.Unix(expiresAt, 0))
			if remaining > 0 {
				// Refresh at 80% of token lifetime (i.e., when 20% remains).
				sleepDur = time.Duration(float64(remaining) * 0.8)
				if sleepDur < minInterval {
					sleepDur = minInterval
				}
			} else {
				// Token already expired, refresh immediately.
				sleepDur = 0
			}
		}

		if sleepDur > 0 {
			timer := time.NewTimer(sleepDur)
			select {
			case <-inst.stopChan:
				timer.Stop()
				return
			case <-timer.C:
			}
		}

		// Check stop again before doing work.
		select {
		case <-inst.stopChan:
			return
		default:
		}

		if err := refreshCopilotTokenWithRetry(inst.State, 3); err != nil {
			log.Printf("Token refresh failed for %s: %v", inst.Account.Name, err)
			mu.Lock()
			inst.Status = "error"
			inst.Error = err.Error()
			mu.Unlock()
			continue
		}

	}
}

func refreshCopilotToken(state *config.State) error {
	req, err := http.NewRequest("GET", config.GithubCopilotURL, nil)
	if err != nil {
		return err
	}
	for k, v := range config.GithubHeaders(state) {
		req.Header[k] = v
	}

	resp, err := getDefaultClient().Do(req)
	if err != nil {
		return fmt.Errorf("failed to get copilot token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("copilot token request failed (status %d): %s", resp.StatusCode, string(body))
	}

	var tokenResp config.CopilotTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("failed to decode copilot token: %w", err)
	}

	state.Lock()
	state.CopilotToken = tokenResp.Token
	state.TokenExpiresAt = tokenResp.ExpiresAt
	state.Unlock()

	if tokenResp.ExpiresAt > 0 {
		expiresIn := time.Until(time.Unix(tokenResp.ExpiresAt, 0))
		log.Printf("Copilot token refreshed, expires in %v", expiresIn.Round(time.Second))
	}
	return nil
}

// refreshCopilotTokenWithRetry attempts token refresh with exponential backoff.
// Retries up to maxRetries times on failure before giving up.
func refreshCopilotTokenWithRetry(state *config.State, maxRetries int) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 2s, 8s, 18s (2 * attempt^2)
			backoff := time.Duration(2*math.Pow(float64(attempt), 2)) * time.Second
			log.Printf("Token refresh retry %d/%d after %v", attempt, maxRetries, backoff)
			time.Sleep(backoff)
		}
		lastErr = refreshCopilotToken(state)
		if lastErr == nil {
			return nil
		}
		log.Printf("Token refresh attempt %d failed: %v", attempt+1, lastErr)
	}
	return fmt.Errorf("token refresh failed after %d attempts: %w", maxRetries+1, lastErr)
}

func fetchModels(state *config.State) error {
	state.RLock()
	baseURL := config.CopilotBaseURL(state.AccountType)
	state.RUnlock()

	req, err := http.NewRequest("GET", baseURL+"/models", nil)
	if err != nil {
		return err
	}
	for k, v := range config.CopilotHeaders(state, false) {
		req.Header[k] = v
	}

	resp, err := getDefaultClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var models config.ModelsResponse
	if err := json.Unmarshal(body, &models); err != nil {
		// Try parsing as array
		var modelList []config.ModelEntry
		if err2 := json.Unmarshal(body, &modelList); err2 != nil {
			return fmt.Errorf("failed to parse models: %w", err)
		}
		models = config.ModelsResponse{
			Object: "list",
			Data:   modelList,
		}
	}

	if models.Object == "" {
		models.Object = "list"
	}

	state.Lock()
	state.Models = &models
	state.Unlock()
	return nil
}

// fetchSDKModels asks the Copilot CLI SDK which models its sessions accept and
// caches them on the state. The REST /models list (fetchModels) contains models
// that session.create rejects, so listings must come from here (issue #14).
func fetchSDKModels(ctx context.Context, sdkClient *sdk.Client, state *config.State) error {
	models, err := sdkClient.ListModels(ctx)
	if err != nil {
		return err
	}
	resp := sdkModelsToResponse(models)

	state.Lock()
	state.SDKModels = resp
	state.Unlock()
	return nil
}

// sdkModelsToResponse converts SDK model infos to the OpenAI-style model list
// served by /v1/models.
func sdkModelsToResponse(models []sdk.ModelInfo) *config.ModelsResponse {
	resp := &config.ModelsResponse{Object: "list", Data: make([]config.ModelEntry, 0, len(models))}
	for _, m := range models {
		entry := config.ModelEntry{
			ID:      m.ID,
			Object:  "model",
			OwnedBy: "github-copilot",
			Name:    m.Name,
		}
		limits := config.ModelLimits{}
		if m.Capabilities.Limits.MaxPromptTokens != nil {
			limits.MaxPromptTokens = *m.Capabilities.Limits.MaxPromptTokens
		}
		if m.Capabilities.Limits.MaxContextWindowTokens != nil {
			limits.MaxContextWindow = *m.Capabilities.Limits.MaxContextWindowTokens
		}
		if limits != (config.ModelLimits{}) {
			entry.Capabilities = &config.ModelCapabilities{Type: "chat", Limits: limits}
		}
		resp.Data = append(resp.Data, entry)
	}
	return resp
}

var (
	clientMu            sync.RWMutex
	streamingHTTPClient *http.Client
	defaultHTTPClient   *http.Client
)

func init() {
	rebuildHTTPClients()
}

func buildTransport(streaming bool, proxyRawURL string) *http.Transport {
	t := &http.Transport{
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if streaming {
		t.MaxIdleConns = 100
		t.MaxIdleConnsPerHost = 20
		t.ResponseHeaderTimeout = 2 * time.Minute
	} else {
		t.MaxIdleConns = 50
		t.MaxIdleConnsPerHost = 10
	}
	if proxyRawURL != "" {
		if parsed, err := url.Parse(proxyRawURL); err == nil {
			t.Proxy = http.ProxyURL(parsed)
		}
	}
	return t
}

func rebuildHTTPClients() {
	pURL := config.GetProxyURL()
	streaming := &http.Client{
		// No Timeout set — streaming responses can last indefinitely.
		Transport: buildTransport(true, pURL),
	}
	nonStreaming := &http.Client{
		Timeout:   15 * time.Second,
		Transport: buildTransport(false, pURL),
	}
	clientMu.Lock()
	streamingHTTPClient = streaming
	defaultHTTPClient = nonStreaming
	clientMu.Unlock()
}

// RebuildHTTPClients rebuilds the shared HTTP clients using the current proxy setting.
func RebuildHTTPClients() {
	rebuildHTTPClients()
}

func getStreamingClient() *http.Client {
	clientMu.RLock()
	c := streamingHTTPClient
	clientMu.RUnlock()
	return c
}

func getDefaultClient() *http.Client {
	clientMu.RLock()
	c := defaultHTTPClient
	clientMu.RUnlock()
	return c
}

// GetDefaultHTTPClient returns the current default HTTP client for use by other packages.
func GetDefaultHTTPClient() *http.Client {
	return getDefaultClient()
}

func ProxyRequestWithBytes(state *config.State, method, path string, bodyBytes []byte, extraHeaders http.Header, hasVision bool) (*http.Response, error) {
	return ProxyRequestWithBytesCtx(context.Background(), state, method, path, bodyBytes, extraHeaders, hasVision)
}

func ProxyRequestWithBytesCtx(ctx context.Context, state *config.State, method, path string, bodyBytes []byte, extraHeaders http.Header, hasVision bool) (*http.Response, error) {
	state.RLock()
	baseURL := config.CopilotBaseURL(state.AccountType)
	state.RUnlock()

	requestURL := baseURL + path

	req, err := http.NewRequestWithContext(ctx, method, requestURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	for k, v := range config.CopilotHeaders(state, hasVision) {
		req.Header[k] = v
	}
	for k, v := range extraHeaders {
		req.Header[k] = v
	}

	return getStreamingClient().Do(req)
}
