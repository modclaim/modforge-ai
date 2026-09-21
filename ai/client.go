package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Default models in order of priority
var DefaultModels = []string{
	"gemini-2.5-flash",
	"gemini-3.7-flash",
	"gemini-3.8-flash",
	"gemini-3.1-flash-lite",
	"gemini-2.0-flash",
}

const DefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// Config holds configuration for the Gemini AI client
type Config struct {
	APIKeys   []string
	Model     string
	Models    []string
	BaseURL   string
	UseMockAI bool
	Cooldown  time.Duration
}

// KeyStatus tracks health and rate limiting for an individual API key
type KeyStatus struct {
	Key              string
	MaskedKey        string
	RateLimitedUntil time.Time
	FailureCount     int
	SuccessCount     int
}

// KeyManager manages thread-safe multi-API key rotation and auto-swapping
type KeyManager struct {
	mu           sync.RWMutex
	keys         []*KeyStatus
	currentIndex int
	cooldown     time.Duration
}

// NewKeyManager initializes the key manager with provided API keys
func NewKeyManager(keys []string, cooldown time.Duration) *KeyManager {
	if cooldown <= 0 {
		cooldown = 60 * time.Second
	}

	var keyStatuses []*KeyStatus
	for _, k := range keys {
		trimmed := strings.TrimSpace(k)
		if trimmed == "" {
			continue
		}
		keyStatuses = append(keyStatuses, &KeyStatus{
			Key:       trimmed,
			MaskedKey: maskAPIKey(trimmed),
		})
	}

	return &KeyManager{
		keys:     keyStatuses,
		cooldown: cooldown,
	}
}

// KeyCount returns the total number of registered keys
func (km *KeyManager) KeyCount() int {
	km.mu.RLock()
	defer km.mu.RUnlock()
	return len(km.keys)
}

// GetKey retrieves the next available, non-rate-limited key
func (km *KeyManager) GetKey() (*KeyStatus, int, error) {
	km.mu.Lock()
	defer km.mu.Unlock()

	if len(km.keys) == 0 {
		return nil, -1, fmt.Errorf("no Gemini API keys configured")
	}

	now := time.Now()
	totalKeys := len(km.keys)

	// Try to find a key that is not currently rate-limited starting from currentIndex
	for i := 0; i < totalKeys; i++ {
		idx := (km.currentIndex + i) % totalKeys
		status := km.keys[idx]
		if now.After(status.RateLimitedUntil) {
			km.currentIndex = (idx + 1) % totalKeys
			return status, idx, nil
		}
	}

	// If all keys are in cooldown, find the one that expires soonest
	earliestIdx := 0
	earliestTime := km.keys[0].RateLimitedUntil
	for idx, status := range km.keys {
		if status.RateLimitedUntil.Before(earliestTime) {
			earliestTime = status.RateLimitedUntil
			earliestIdx = idx
		}
	}

	waitTime := earliestTime.Sub(now)
	if waitTime > 0 && waitTime <= 5*time.Second {
		log.Printf("[Gemini KeyManager] All %d keys currently rate-limited. Waiting %v for key %s cooldown...",
			totalKeys, waitTime.Round(100*time.Millisecond), km.keys[earliestIdx].MaskedKey)
		time.Sleep(waitTime)
		km.currentIndex = (earliestIdx + 1) % totalKeys
		return km.keys[earliestIdx], earliestIdx, nil
	}

	// If wait is too long, still return the earliest key but warn
	log.Printf("[Gemini KeyManager] WARNING: All %d keys are rate-limited. Attempting earliest key %s",
		totalKeys, km.keys[earliestIdx].MaskedKey)
	km.currentIndex = (earliestIdx + 1) % totalKeys
	return km.keys[earliestIdx], earliestIdx, nil
}

// MarkRateLimited marks a key as rate-limited and triggers auto-swap cooldown
func (km *KeyManager) MarkRateLimited(index int, reason string) {
	km.mu.Lock()
	defer km.mu.Unlock()

	if index < 0 || index >= len(km.keys) {
		return
	}

	status := km.keys[index]
	status.RateLimitedUntil = time.Now().Add(km.cooldown)
	status.FailureCount++

	nextIdx := (index + 1) % len(km.keys)
	log.Printf("[Gemini KeyManager] AUTO-SWAP: Key %s marked rate-limited (%s). Cooldown: %v. Swapping to next key index %d (%s)",
		status.MaskedKey, reason, km.cooldown, nextIdx, km.keys[nextIdx].MaskedKey)
}

// MarkSuccess records a successful call for the key
func (km *KeyManager) MarkSuccess(index int) {
	km.mu.Lock()
	defer km.mu.Unlock()

	if index >= 0 && index < len(km.keys) {
		km.keys[index].SuccessCount++
	}
}

// Client wraps the Google Gemini API client with multi-key auto-swap and model fallback
type Client struct {
	keyManager   *KeyManager
	primaryModel string
	models       []string
	baseURL      string
	httpClient   *http.Client
	useMockAI    bool
}

// NewClient creates a new AI client. apiKey can be a single key or comma-separated keys.
func NewClient(apiKey string) *Client {
	var keys []string
	for _, k := range strings.Split(apiKey, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			keys = append(keys, k)
		}
	}

	return NewClientWithConfig(Config{
		APIKeys:   keys,
		Model:     DefaultModels[0],
		Models:    DefaultModels,
		BaseURL:   DefaultBaseURL,
		UseMockAI: len(keys) == 0,
	})
}

// NewClientWithConfig creates a new Gemini AI client with full configuration
func NewClientWithConfig(cfg Config) *Client {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	models := cfg.Models
	if len(models) == 0 {
		if cfg.Model != "" {
			models = []string{cfg.Model}
		} else {
			models = DefaultModels
		}
	}

	primary := cfg.Model
	if primary == "" {
		primary = models[0]
	}

	// Ensure primary model is first in the list
	orderedModels := []string{primary}
	for _, m := range models {
		if m != primary && strings.TrimSpace(m) != "" {
			orderedModels = append(orderedModels, strings.TrimSpace(m))
		}
	}

	return &Client{
		keyManager:   NewKeyManager(cfg.APIKeys, cfg.Cooldown),
		primaryModel: primary,
		models:       orderedModels,
		baseURL:      baseURL,
		httpClient: &http.Client{
			Timeout: 90 * time.Second,
		},
		useMockAI: cfg.UseMockAI,
	}
}

// ProcessModRequest represents a request to process a mod
type ProcessModRequest struct {
	Content        string            `json:"content"`
	PromptTemplate string            `json:"prompt_template"`
	GameType       string            `json:"game_type"`
	Variables      map[string]string `json:"variables"`
}

// ProcessModResponse represents the response from processing a mod
type ProcessModResponse struct {
	ProcessedContent string `json:"processed_content"`
	Changelog        string `json:"changelog"`
	TokensUsed       int    `json:"tokens_used"`
}

// Gemini API Request & Response structures
type geminiPart struct {
	Text string `json:"text,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiSystemInstruction struct {
	Parts []geminiPart `json:"parts"`
}

type geminiGenerationConfig struct {
	Temperature      float64 `json:"temperature,omitempty"`
	MaxOutputTokens  int     `json:"maxOutputTokens,omitempty"`
	ResponseMimeType string  `json:"responseMimeType,omitempty"`
}

type geminiRequest struct {
	Contents          []geminiContent          `json:"contents"`
	SystemInstruction *geminiSystemInstruction `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenerationConfig  `json:"generationConfig,omitempty"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
}

type geminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

type geminiErrorDetails struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate    `json:"candidates,omitempty"`
	UsageMetadata *geminiUsageMetadata `json:"usageMetadata,omitempty"`
	Error         *geminiErrorDetails  `json:"error,omitempty"`
}

type parsedModOutput struct {
	ProcessedContent string `json:"processed_content"`
	Changelog        string `json:"changelog"`
}

// ProcessMod processes a mod using Gemini AI with multi-key auto-swap and model fallback
func (c *Client) ProcessMod(ctx context.Context, req ProcessModRequest) (*ProcessModResponse, error) {
	prompt := c.buildPrompt(req.PromptTemplate, req.Content, req.Variables)

	systemPrompt := fmt.Sprintf(`You are an expert game modding assistant specializing in %s mods. 
Your job is to intelligently modify game mod files while preserving their technical structure.

Rules:
1. Always maintain valid JSON/file structure
2. Only modify content that makes sense to change
3. Preserve all technical IDs, keys, and references
4. Provide a brief changelog of what you modified
5. Be conservative - only make improvements that are clearly beneficial

Respond strictly with a JSON object containing:
{
  "processed_content": "the modified content",
  "changelog": "brief summary of changes made"
}`, req.GameType)

	geminiReqBody := geminiRequest{
		SystemInstruction: &geminiSystemInstruction{
			Parts: []geminiPart{{Text: systemPrompt}},
		},
		Contents: []geminiContent{
			{
				Role:  "user",
				Parts: []geminiPart{{Text: prompt}},
			},
		},
		GenerationConfig: &geminiGenerationConfig{
			Temperature:      0.7,
			MaxOutputTokens:  8192,
			ResponseMimeType: "application/json",
		},
	}

	reqBytes, err := json.Marshal(geminiReqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to encode Gemini request: %w", err)
	}

	keyCount := c.keyManager.KeyCount()
	if keyCount == 0 {
		return nil, fmt.Errorf("no Gemini API keys configured. Set GEMINI_API_KEYS in .env")
	}

	var lastErr error

	// Cascade through available models
	for _, model := range c.models {
		log.Printf("[Gemini AI] Attempting processing with model: %s (Total Keys available: %d)", model, keyCount)

		// Try up to keyCount times (auto-swapping through keys on rate limit/quota error)
		attempts := keyCount
		for attempt := 0; attempt < attempts; attempt++ {
			keyStatus, keyIdx, err := c.keyManager.GetKey()
			if err != nil {
				return nil, fmt.Errorf("failed to get API key: %w", err)
			}

			apiURL := fmt.Sprintf("%s/models/%s:generateContent?key=%s", c.baseURL, model, keyStatus.Key)

			httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(reqBytes))
			if err != nil {
				return nil, fmt.Errorf("failed to create HTTP request: %w", err)
			}
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("x-goog-api-key", keyStatus.Key)

			resp, err := c.httpClient.Do(httpReq)
			if err != nil {
				lastErr = fmt.Errorf("network error on key %s: %w", keyStatus.MaskedKey, err)
				log.Printf("[Gemini AI] %v", lastErr)
				continue
			}

			respBytes, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				lastErr = fmt.Errorf("failed to read response body: %w", err)
				continue
			}

			// Handle HTTP Statuses
			if resp.StatusCode == http.StatusTooManyRequests || isQuotaExceededStatus(resp.StatusCode, respBytes) {
				reason := fmt.Sprintf("HTTP %d Rate Limit / Quota Exceeded", resp.StatusCode)
				c.keyManager.MarkRateLimited(keyIdx, reason)
				lastErr = fmt.Errorf("gemini rate limit exceeded for key %s (HTTP %d)", keyStatus.MaskedKey, resp.StatusCode)
				continue // Auto-swap to next key
			}

			if resp.StatusCode == http.StatusServiceUnavailable {
				reason := "HTTP 503 Model High Demand"
				c.keyManager.MarkRateLimited(keyIdx, reason)
				lastErr = fmt.Errorf("gemini 503 high demand spike on model %s", model)
				continue // Auto-swap to next key or model
			}

			var geminiResp geminiResponse
			if err := json.Unmarshal(respBytes, &geminiResp); err != nil {
				lastErr = fmt.Errorf("failed to parse Gemini response (status %d): %w", resp.StatusCode, err)
				log.Printf("[Gemini AI] %v: %s", lastErr, string(respBytes))
				continue
			}

			// Handle API-level error returned in JSON
			if geminiResp.Error != nil {
				errMsg := geminiResp.Error.Message
				if isRateLimitOrQuotaMessage(errMsg) {
					c.keyManager.MarkRateLimited(keyIdx, fmt.Sprintf("API error: %s", errMsg))
					lastErr = fmt.Errorf("gemini quota error: %s", errMsg)
					continue
				}

				// If model is not found (404), break out of key loop to try next fallback model
				if geminiResp.Error.Code == 404 || strings.Contains(strings.ToLower(errMsg), "not found") {
					log.Printf("[Gemini AI] Model %s not found. Falling back to next model...", model)
					lastErr = fmt.Errorf("model %s not found: %s", model, errMsg)
					break
				}

				lastErr = fmt.Errorf("gemini API error (%d): %s", geminiResp.Error.Code, errMsg)
				continue
			}

			if len(geminiResp.Candidates) == 0 || len(geminiResp.Candidates[0].Content.Parts) == 0 {
				lastErr = fmt.Errorf("gemini returned no candidates (status %d)", resp.StatusCode)
				continue
			}

			// Success! Mark key as good
			c.keyManager.MarkSuccess(keyIdx)

			rawText := geminiResp.Candidates[0].Content.Parts[0].Text
			tokensUsed := 0
			if geminiResp.UsageMetadata != nil {
				tokensUsed = geminiResp.UsageMetadata.TotalTokenCount
			}

			// Parse response content
			processedContent, changelog := parseGeminiOutput(rawText)

			log.Printf("[Gemini AI] SUCCESS: Processed mod using model %s and key %s (Tokens used: %d)",
				model, keyStatus.MaskedKey, tokensUsed)

			return &ProcessModResponse{
				ProcessedContent: processedContent,
				Changelog:        changelog,
				TokensUsed:       tokensUsed,
			}, nil
		}
	}

	return nil, fmt.Errorf("all Gemini models and API keys exhausted. Last error: %w", lastErr)
}

// parseGeminiOutput parses JSON or text returned by Gemini
func parseGeminiOutput(raw string) (string, string) {
	cleaned := strings.TrimSpace(raw)

	// Strip markdown code fences if present (e.g. ```json ... ```)
	if strings.HasPrefix(cleaned, "```") {
		lines := strings.Split(cleaned, "\n")
		if len(lines) >= 2 {
			if strings.HasPrefix(lines[0], "```") {
				lines = lines[1:]
			}
			if len(lines) > 0 && strings.HasPrefix(lines[len(lines)-1], "```") {
				lines = lines[:len(lines)-1]
			}
			cleaned = strings.TrimSpace(strings.Join(lines, "\n"))
		}
	}

	var output parsedModOutput
	if err := json.Unmarshal([]byte(cleaned), &output); err == nil {
		if output.ProcessedContent != "" {
			cl := output.Changelog
			if cl == "" {
				cl = "Gemini AI enhancements applied"
			}
			return output.ProcessedContent, cl
		}
	}

	// Fallback to raw output if json unmarshal wasn't matched
	return raw, "Gemini AI modifications applied"
}

func isQuotaExceededStatus(statusCode int, body []byte) bool {
	if statusCode == 429 {
		return true
	}
	bodyStr := strings.ToLower(string(body))
	return strings.Contains(bodyStr, "resource_exhausted") ||
		strings.Contains(bodyStr, "quota exceeded") ||
		strings.Contains(bodyStr, "rate limit")
}

func isRateLimitOrQuotaMessage(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "resource_exhausted") ||
		strings.Contains(lower, "quota") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "temporarily unavailable")
}

func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[:6] + "..." + key[len(key)-4:]
}

// buildPrompt builds the final prompt from template and variables
func (c *Client) buildPrompt(template, content string, variables map[string]string) string {
	prompt := template
	prompt = strings.ReplaceAll(prompt, "{content}", content)

	for key, value := range variables {
		placeholder := fmt.Sprintf("{%s}", key)
		prompt = strings.ReplaceAll(prompt, placeholder, value)
	}

	return prompt
}

// ValidateModContent performs basic validation on mod content
func ValidateModContent(content string, gameType string) error {
	switch gameType {
	case "minecraft":
		return validateMinecraftJSON(content)
	case "skyrim":
		return validateSkyrimESP(content)
	case "lua":
		return validateLuaScript(content)
	default:
		return fmt.Errorf("unsupported game type: %s", gameType)
	}
}

func validateMinecraftJSON(content string) error {
	if !strings.Contains(content, "{") || !strings.Contains(content, "}") {
		return fmt.Errorf("invalid JSON structure")
	}
	return nil
}

func validateSkyrimESP(content string) error {
	return nil
}

func validateLuaScript(content string) error {
	return nil
}
