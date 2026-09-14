package main

import (
	"encoding/json"
	"testing"
)

func TestUsageDetailFromMap(t *testing.T) {
	m := map[string]any{
		"total_tokens":      float64(100),
		"prompt_tokens":     float64(40),
		"completion_tokens": float64(60),
	}
	d := usageDetailFromMap(m)
	if d.TotalTokens != 100 {
		t.Fatalf("total=%d want 100", d.TotalTokens)
	}
	if d.InputTokens != 40 {
		t.Fatalf("input=%d want 40", d.InputTokens)
	}
	if d.OutputTokens != 60 {
		t.Fatalf("output=%d want 60", d.OutputTokens)
	}
}
func TestUsageDetailFromMap_Nil(t *testing.T) {
	d := usageDetailFromMap(nil)
	if d.TotalTokens != 0 {
		t.Fatal("nil should be zero")
	}
}
func TestUsageDetailFromMap_Partial(t *testing.T) {
	m := map[string]any{"total_tokens": float64(50)}
	d := usageDetailFromMap(m)
	if d.TotalTokens != 50 {
		t.Fatalf("total=%d want 50", d.TotalTokens)
	}
	// partial: only total set
}
func TestUsageDetailFromCompletion(t *testing.T) {
	payload := []byte(`{"usage":{"total_tokens":200,"prompt_tokens":80,"completion_tokens":120}}`)
	d := usageDetailFromCompletion(payload)
	if d.TotalTokens != 200 {
		t.Fatalf("total=%d want 200", d.TotalTokens)
	}
}
func TestUsageDetailFromCompletion_Invalid(t *testing.T) {
	d := usageDetailFromCompletion([]byte(`not json`))
	if d.TotalTokens != 0 {
		t.Fatal("invalid should be zero")
	}
}
func TestSseUsageCollector(t *testing.T) {
	c := &sseUsageCollector{}
	c.feed(`{"usage":{"total_tokens":42}}`)
	if c.last == nil || c.last["total_tokens"] == nil {
		t.Fatal("collector should capture usage")
	}
	c.feed(`{"choices":[]}`) // no usage
	// last should still be the previous one
	if c.last["total_tokens"] == nil {
		t.Fatal("collector should retain last usage")
	}
}

// OpenAI-style upstreams report cache hits nested under prompt_tokens_details.
// Before this was handled the count silently came back 0.
func TestUsageDetailFromMap_OpenAINestedCachedTokens(t *testing.T) {
	m := map[string]any{
		"prompt_tokens":     float64(10),
		"completion_tokens": float64(20),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": float64(2048),
		},
	}
	d := usageDetailFromMap(m)
	if d.CachedTokens != 2048 {
		t.Fatalf("CachedTokens=%d want 2048", d.CachedTokens)
	}
	// The read counter mirrors the hit counter so the panel's read column
	// is populated for OpenAI-shaped usage.
	if d.CacheReadTokens != 2048 {
		t.Fatalf("CacheReadTokens=%d want 2048", d.CacheReadTokens)
	}
}

func TestUsageDetailFromMap_NestedInputTokensDetails(t *testing.T) {
	m := map[string]any{
		"input_tokens_details": map[string]any{"cached_tokens": float64(512)},
	}
	if d := usageDetailFromMap(m); d.CachedTokens != 512 {
		t.Fatalf("CachedTokens=%d want 512", d.CachedTokens)
	}
}

// Flat values win over nested ones when both are present.
func TestUsageDetailFromMap_FlatCachedWinsOverNested(t *testing.T) {
	m := map[string]any{
		"cached_tokens":         float64(100),
		"prompt_tokens_details": map[string]any{"cached_tokens": float64(999)},
	}
	if d := usageDetailFromMap(m); d.CachedTokens != 100 {
		t.Fatalf("CachedTokens=%d want 100", d.CachedTokens)
	}
}

// The cache-write side was previously never parsed at all.
func TestUsageDetailFromMap_CacheCreationTokens(t *testing.T) {
	anthropic := map[string]any{
		"input_tokens":                float64(5),
		"cache_read_input_tokens":     float64(1200),
		"cache_creation_input_tokens": float64(3400),
	}
	d := usageDetailFromMap(anthropic)
	if d.CacheReadTokens != 1200 {
		t.Fatalf("CacheReadTokens=%d want 1200", d.CacheReadTokens)
	}
	if d.CacheCreationTokens != 3400 {
		t.Fatalf("CacheCreationTokens=%d want 3400", d.CacheCreationTokens)
	}

	openaiNested := map[string]any{
		"prompt_tokens_details": map[string]any{"cache_creation_tokens": float64(77)},
	}
	if d := usageDetailFromMap(openaiNested); d.CacheCreationTokens != 77 {
		t.Fatalf("nested CacheCreationTokens=%d want 77", d.CacheCreationTokens)
	}
}

func TestUsageDetailFromMap_ReasoningNestedFallbacks(t *testing.T) {
	openai := map[string]any{
		"completion_tokens_details": map[string]any{"reasoning_tokens": float64(30)},
	}
	if d := usageDetailFromMap(openai); d.ReasoningTokens != 30 {
		t.Fatalf("ReasoningTokens=%d want 30", d.ReasoningTokens)
	}
	responses := map[string]any{
		"output_tokens_details": map[string]any{"reasoning_tokens": float64(41)},
	}
	if d := usageDetailFromMap(responses); d.ReasoningTokens != 41 {
		t.Fatalf("output_tokens_details ReasoningTokens=%d want 41", d.ReasoningTokens)
	}
}

// A usage block with no cache counters must stay at zero rather than
// inheriting a value from an unrelated field.
func TestUsageDetailFromMap_NoCacheCounters(t *testing.T) {
	d := usageDetailFromMap(map[string]any{"total_tokens": float64(10)})
	if d.CachedTokens != 0 || d.CacheReadTokens != 0 || d.CacheCreationTokens != 0 {
		t.Fatalf("expected zero cache counters, got %+v", d)
	}
}

// realCacheHitUsage is captured live from copilot.tencent.com /v2/chat/completions
// (model glm-5.3, 2026-09) on a request that genuinely reused a prompt-cache
// prefix.
//
// The decisive detail: the FLAT "cached_tokens" is 0 while the NESTED
// prompt_tokens_details.cached_tokens is 4043. The pre-fix parser read only the
// flat key, so it reported zero cache reads for a request that hit 4043 tokens.
// Keep this fixture in sync with the upstream — if the flat key starts being
// populated, the regression it guards no longer exists in this form.
const realCacheHitUsage = `{"prompt_tokens":4443,"completion_tokens":4,"total_tokens":4447,` +
	`"completion_tokens_details":{"accepted_prediction_tokens":0,"audio_tokens":0,"reasoning_tokens":0,"rejected_prediction_tokens":0,"cached_tokens":0},` +
	`"prompt_tokens_details":{"accepted_prediction_tokens":0,"audio_tokens":0,"reasoning_tokens":0,"rejected_prediction_tokens":0,"cached_tokens":4043},` +
	`"prompt_cache_hit_tokens":4043,"prompt_cache_miss_tokens":400,` +
	`"cache_read_input_tokens":0,"cache_creation_input_tokens":0,` +
	`"prompt_cache_write_tokens":0,"completion_thinking_tokens":0,"credit":0.23,"cached_tokens":0}`

// TestUsageDetailFromMap_RealCacheHit asserts the parser reports a genuine
// upstream cache hit that only appears in the nested field.
func TestUsageDetailFromMap_RealCacheHit(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal([]byte(realCacheHitUsage), &m); err != nil {
		t.Fatalf("fixture decode: %v", err)
	}
	// Guard the premise this test depends on.
	if got := m["cached_tokens"].(float64); got != 0 {
		t.Fatalf("fixture premise broken: flat cached_tokens=%v (want 0)", got)
	}
	d := usageDetailFromMap(m)
	if d.CachedTokens != 4043 {
		t.Errorf("CachedTokens=%d want 4043 (flat-only parsing yields 0)", d.CachedTokens)
	}
	if d.CacheReadTokens != 4043 {
		t.Errorf("CacheReadTokens=%d want 4043", d.CacheReadTokens)
	}
	if d.InputTokens != 4443 || d.OutputTokens != 4 || d.TotalTokens != 4447 {
		t.Errorf("token counts wrong: in=%d out=%d total=%d", d.InputTokens, d.OutputTokens, d.TotalTokens)
	}
}

// TestUsageDetailFromMap_RealResponseNoCache covers the live cold-miss shape,
// where every cache counter is legitimately zero.
func TestUsageDetailFromMap_RealResponseNoCache(t *testing.T) {
	const cold = `{"prompt_tokens":14,"completion_tokens":16,"total_tokens":30,` +
		`"completion_tokens_details":{"reasoning_tokens":15,"cached_tokens":0},` +
		`"prompt_tokens_details":{"reasoning_tokens":0,"cached_tokens":0},` +
		`"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":14,` +
		`"cache_read_input_tokens":0,"cache_creation_input_tokens":0,` +
		`"prompt_cache_write_tokens":0,"completion_thinking_tokens":15,"cached_tokens":0}`
	var m map[string]any
	if err := json.Unmarshal([]byte(cold), &m); err != nil {
		t.Fatalf("fixture decode: %v", err)
	}
	d := usageDetailFromMap(m)
	if d.CachedTokens != 0 || d.CacheReadTokens != 0 || d.CacheCreationTokens != 0 {
		t.Errorf("cold response should have zero cache counters, got %+v", d)
	}
	// reasoning comes from the nested completion_tokens_details block.
	if d.ReasoningTokens != 15 {
		t.Errorf("ReasoningTokens=%d want 15", d.ReasoningTokens)
	}
	if d.InputTokens != 14 || d.OutputTokens != 16 || d.TotalTokens != 30 {
		t.Errorf("token counts wrong: in=%d out=%d total=%d", d.InputTokens, d.OutputTokens, d.TotalTokens)
	}
}
