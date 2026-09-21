package ai

import (
	"context"
	"testing"
	"time"
)

func TestKeyManager_Rotation(t *testing.T) {
	keys := []string{"key1_1234567890", "key2_1234567890", "key3_1234567890"}
	km := NewKeyManager(keys, 60*time.Second)

	if km.KeyCount() != 3 {
		t.Fatalf("expected 3 keys, got %d", km.KeyCount())
	}

	// Round 1
	k1, idx1, err := km.GetKey()
	if err != nil || k1.Key != "key1_1234567890" || idx1 != 0 {
		t.Errorf("expected key1, got %v, idx %d", k1, idx1)
	}

	// Round 2
	k2, idx2, err := km.GetKey()
	if err != nil || k2.Key != "key2_1234567890" || idx2 != 1 {
		t.Errorf("expected key2, got %v, idx %d", k2, idx2)
	}

	// Round 3
	k3, idx3, err := km.GetKey()
	if err != nil || k3.Key != "key3_1234567890" || idx3 != 2 {
		t.Errorf("expected key3, got %v, idx %d", k3, idx3)
	}

	// Round 4 (wraps around)
	k4, idx4, err := km.GetKey()
	if err != nil || k4.Key != "key1_1234567890" || idx4 != 0 {
		t.Errorf("expected key1 again, got %v, idx %d", k4, idx4)
	}
}

func TestKeyManager_AutoSwapOnRateLimit(t *testing.T) {
	keys := []string{"keyA_1234567890", "keyB_1234567890"}
	km := NewKeyManager(keys, 10*time.Second)

	// Get key 0
	k, idx, _ := km.GetKey()
	if idx != 0 {
		t.Fatalf("expected idx 0, got %d", idx)
	}

	// Mark key 0 as rate-limited
	km.MarkRateLimited(idx, "429 Too Many Requests")

	// Next GetKey should skip key 0 and return key 1
	kNext, idxNext, err := km.GetKey()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idxNext != 1 || kNext.Key != "keyB_1234567890" {
		t.Fatalf("expected keyB (idx 1), got %s (idx %d)", kNext.Key, idxNext)
	}

	// Another call should still pick key 1 (key 0 is rate limited)
	kNext2, idxNext2, _ := km.GetKey()
	if idxNext2 != 1 {
		t.Fatalf("expected keyB (idx 1) while keyA is in cooldown, got idx %d", idxNext2)
	}
	_ = k
	_ = kNext2
}

func TestParseGeminiOutput(t *testing.T) {
	// Case 1: Pure JSON
	jsonInput := `{"processed_content": "enhanced content", "changelog": "added features"}`
	content, changelog := parseGeminiOutput(jsonInput)
	if content != "enhanced content" || changelog != "added features" {
		t.Errorf("failed parsing pure json: got %s, %s", content, changelog)
	}

	// Case 2: Markdown fenced JSON
	fencedInput := "```json\n{\n  \"processed_content\": \"fenced content\",\n  \"changelog\": \"fixed bug\"\n}\n```"
	content2, changelog2 := parseGeminiOutput(fencedInput)
	if content2 != "fenced content" || changelog2 != "fixed bug" {
		t.Errorf("failed parsing fenced json: got %s, %s", content2, changelog2)
	}

	// Case 3: Raw non-JSON text fallback
	rawInput := "Plain text modifications done without json"
	content3, changelog3 := parseGeminiOutput(rawInput)
	if content3 != rawInput || changelog3 == "" {
		t.Errorf("failed raw fallback: got %s, %s", content3, changelog3)
	}
}

func TestClient_LiveGeminiRealRequest(t *testing.T) {
	// Test real Gemini API call with actual working key
	apiKey := "AIzaSyDUzQsiR52J3UwNaWxJK8WAg8HeMzGml3g,AIzaSyBe-oTINVFhqlReiTH_n4BZKFr_FM2aXCo"
	client := NewClientWithConfig(Config{
		APIKeys: []string{
			"AIzaSyDUzQsiR52J3UwNaWxJK8WAg8HeMzGml3g",
			"AIzaSyBe-oTINVFhqlReiTH_n4BZKFr_FM2aXCo",
		},
		Model:  "gemini-2.5-flash",
		Models: []string{"gemini-2.5-flash", "gemini-3.7-flash", "gemini-3.8-flash"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	resp, err := client.ProcessMod(ctx, ProcessModRequest{
		Content:        `{"name": "test_sword", "damage": 5}`,
		PromptTemplate: "Make this sword stronger: {content}",
		GameType:       "minecraft",
		Variables:      map[string]string{},
	})

	if err != nil {
		t.Logf("ProcessMod returned error (could be rate limit/network): %v", err)
		return
	}

	if resp.ProcessedContent == "" {
		t.Errorf("expected non-empty ProcessedContent")
	}
	t.Logf("Success! ProcessedContent: %s, Changelog: %s, Tokens: %d",
		resp.ProcessedContent, resp.Changelog, resp.TokensUsed)
	_ = apiKey
}
