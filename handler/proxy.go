package handler

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"copilot-go/config"
	"copilot-go/instance"
	"copilot-go/store"

	"github.com/gin-gonic/gin"
)

// RegisterProxy sets up the proxy server routes.
func RegisterProxy(r *gin.Engine) {
	// Initialize rate limiter from environment.
	instance.InitRateLimiter()

	// Load per-account rate limit from pool config.
	if poolCfg, err := store.GetPoolConfig(); err == nil && poolCfg != nil {
		instance.SetPerAccountRPM(poolCfg.RateLimitRPM)
	}

	r.Use(proxyAuth())

	// OpenAI compatible endpoints
	r.POST("/chat/completions", proxyCompletions)
	r.POST("/v1/chat/completions", proxyCompletions)
	r.GET("/models", proxyModels)
	r.GET("/v1/models", proxyModels)
	r.POST("/embeddings", proxyEmbeddings)
	r.POST("/v1/embeddings", proxyEmbeddings)

	// Anthropic compatible endpoints
	r.POST("/v1/messages", proxyMessages)
	r.POST("/v1/messages/count_tokens", proxyCountTokens)

	// OpenAI Responses API endpoint
	r.POST("/v1/responses", proxyResponses)
}

func proxyAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			// Also check x-api-key for Anthropic-style auth
			apiKey := c.GetHeader("x-api-key")
			if apiKey != "" {
				authHeader = "Bearer " + apiKey
			}
		}

		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization"})
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")

		// Check pool API key first
		poolCfg, _ := store.GetPoolConfig()
		if poolCfg != nil && poolCfg.Enabled && poolCfg.ApiKey == token {
			c.Set("isPool", true)
			c.Set("poolStrategy", poolCfg.Strategy)
			c.Next()
			return
		}

		// Check individual account API key
		account, err := store.GetAccountByApiKey(token)
		if err != nil || account == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid API key"})
			return
		}

		c.Set("accountID", account.ID)
		c.Set("isPool", false)
		c.Next()
	}
}

// resolvedAccount holds the resolved state and account ID.
type resolvedAccount struct {
	State     *config.State
	AccountID string
}

func resolveState(c *gin.Context, exclude map[string]bool) *resolvedAccount {
	isPool, _ := c.Get("isPool")
	if isPool == true {
		strategy := ""
		if s, ok := c.Get("poolStrategy"); ok {
			strategy = s.(string)
		}
		account, err := instance.SelectAccount(strategy, exclude)
		if err != nil || account == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no available accounts in pool"})
			return nil
		}
		state := instance.GetInstanceState(account.ID)
		if state == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "selected account instance not running"})
			return nil
		}
		return &resolvedAccount{State: state, AccountID: account.ID}
	}

	accountID, exists := c.Get("accountID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no account context"})
		return nil
	}
	aid := accountID.(string)
	state := instance.GetInstanceState(aid)
	if state == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account instance not running"})
		return nil
	}
	return &resolvedAccount{State: state, AccountID: aid}
}

// isRetryableStatus returns true for HTTP status codes that warrant a retry with a different account.
func isRetryableStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || (statusCode >= 500 && statusCode <= 599)
}

// checkRateLimit checks the rate limit for the account and writes a 429 response if exceeded.
// Returns true if the request is allowed, false if rate limited.
func checkRateLimit(c *gin.Context, accountID string) bool {
	allowed, retryAfter := instance.CheckRateLimit(accountID)
	if !allowed {
		c.Header("Retry-After", fmt.Sprintf("%.0f", retryAfter))
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": gin.H{
				"message": "rate limit exceeded",
				"type":    "rate_limit_error",
			},
		})
		return false
	}
	return true
}

// proxyCompletions handles completions with pool-mode retry support.
func proxyCompletions(c *gin.Context) {
	isPool, _ := c.Get("isPool")
	maxAttempts := 1
	if isPool == true {
		maxAttempts = 3
	}

	// Read body once for potential retries.
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	exclude := make(map[string]bool)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resolved := resolveState(c, exclude)
		if resolved == nil {
			return // resolveState already wrote the error response
		}

		// Check rate limit.
		if !checkRateLimit(c, resolved.AccountID) {
			return
		}

		sdkClient := instance.GetSDKClient(resolved.AccountID)
		if sdkClient == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "SDK client not ready"})
			return
		}

		// Record the request.
		instance.RecordRequest(resolved.AccountID, false, false)

		resp, proxyErr := instance.DoCompletionsProxy(c, resolved.State, sdkClient, bodyBytes)
		if proxyErr != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}
			instance.RecordRequest(resolved.AccountID, true, false)
			if attempt < maxAttempts-1 {
				exclude[resolved.AccountID] = true
				log.Printf("Completions proxy error for account %s, retrying: %v", resolved.AccountID, proxyErr)
				continue
			}
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("proxy request failed: %v", proxyErr)})
			return
		}

		// Check if retryable.
		if isRetryableStatus(resp.StatusCode) && attempt < maxAttempts-1 {
			is429 := resp.StatusCode == http.StatusTooManyRequests
			instance.RecordRequest(resolved.AccountID, true, is429)
			_ = resp.Body.Close()
			exclude[resolved.AccountID] = true
			log.Printf("Upstream returned %d for account %s, retrying with different account", resp.StatusCode, resolved.AccountID)
			continue
		}

		// Forward the response.
		instance.ForwardCompletionsResponse(c, resp)
		return
	}
}

func proxyModels(c *gin.Context) {
	resolved := resolveState(c, nil)
	if resolved == nil {
		return
	}
	instance.ModelsHandler(c, resolved.State)
}

func proxyEmbeddings(c *gin.Context) {
	isPool, _ := c.Get("isPool")
	maxAttempts := 1
	if isPool == true {
		maxAttempts = 3
	}

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	exclude := make(map[string]bool)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resolved := resolveState(c, exclude)
		if resolved == nil {
			return
		}

		if !checkRateLimit(c, resolved.AccountID) {
			return
		}

		instance.RecordRequest(resolved.AccountID, false, false)

		resp, proxyErr := instance.DoEmbeddingsProxy(resolved.State, bodyBytes)
		if proxyErr != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}
			instance.RecordRequest(resolved.AccountID, true, false)
			if attempt < maxAttempts-1 {
				exclude[resolved.AccountID] = true
				log.Printf("Embeddings proxy error for account %s, retrying: %v", resolved.AccountID, proxyErr)
				continue
			}
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("proxy request failed: %v", proxyErr)})
			return
		}

		if isRetryableStatus(resp.StatusCode) && attempt < maxAttempts-1 {
			is429 := resp.StatusCode == http.StatusTooManyRequests
			instance.RecordRequest(resolved.AccountID, true, is429)
			_ = resp.Body.Close()
			exclude[resolved.AccountID] = true
			log.Printf("Upstream returned %d for account %s, retrying with different account", resp.StatusCode, resolved.AccountID)
			continue
		}

		instance.ForwardEmbeddingsResponse(c, resp)
		return
	}
}

func proxyMessages(c *gin.Context) {
	isPool, _ := c.Get("isPool")
	maxAttempts := 1
	if isPool == true {
		maxAttempts = 3
	}

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	exclude := make(map[string]bool)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resolved := resolveState(c, exclude)
		if resolved == nil {
			return
		}

		if !checkRateLimit(c, resolved.AccountID) {
			return
		}

		sdkClient := instance.GetSDKClient(resolved.AccountID)
		if sdkClient == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "SDK client not ready"})
			return
		}

		instance.RecordRequest(resolved.AccountID, false, false)

		resp, proxyErr := instance.DoMessagesProxy(c, resolved.State, sdkClient, bodyBytes)
		if proxyErr != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}
			instance.RecordRequest(resolved.AccountID, true, false)
			if attempt < maxAttempts-1 {
				exclude[resolved.AccountID] = true
				log.Printf("Messages proxy error for account %s, retrying: %v", resolved.AccountID, proxyErr)
				continue
			}
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("proxy request failed: %v", proxyErr)})
			return
		}

		if isRetryableStatus(resp.StatusCode) && attempt < maxAttempts-1 {
			is429 := resp.StatusCode == http.StatusTooManyRequests
			instance.RecordRequest(resolved.AccountID, true, is429)
			_ = resp.Body.Close()
			exclude[resolved.AccountID] = true
			log.Printf("Upstream returned %d for account %s, retrying with different account", resp.StatusCode, resolved.AccountID)
			continue
		}

		instance.ForwardMessagesResponse(c, resp, bodyBytes)
		return
	}
}

func proxyCountTokens(c *gin.Context) {
	resolved := resolveState(c, nil)
	if resolved == nil {
		return
	}
	instance.CountTokensHandler(c, resolved.State)
}

func proxyResponses(c *gin.Context) {
	isPool, _ := c.Get("isPool")
	maxAttempts := 1
	if isPool == true {
		maxAttempts = 3
	}

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	exclude := make(map[string]bool)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resolved := resolveState(c, exclude)
		if resolved == nil {
			return
		}

		if !checkRateLimit(c, resolved.AccountID) {
			return
		}

		instance.RecordRequest(resolved.AccountID, false, false)

		resp, proxyErr := instance.DoResponsesProxy(resolved.State, bodyBytes)
		if proxyErr != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}
			instance.RecordRequest(resolved.AccountID, true, false)
			if attempt < maxAttempts-1 {
				exclude[resolved.AccountID] = true
				log.Printf("Responses proxy error for account %s, retrying: %v", resolved.AccountID, proxyErr)
				continue
			}
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("proxy request failed: %v", proxyErr)})
			return
		}

		if isRetryableStatus(resp.StatusCode) && attempt < maxAttempts-1 {
			is429 := resp.StatusCode == http.StatusTooManyRequests
			instance.RecordRequest(resolved.AccountID, true, is429)
			_ = resp.Body.Close()
			exclude[resolved.AccountID] = true
			log.Printf("Upstream returned %d for account %s, retrying with different account", resp.StatusCode, resolved.AccountID)
			continue
		}

		instance.ForwardResponsesResponse(c, resp)
		return
	}
}
