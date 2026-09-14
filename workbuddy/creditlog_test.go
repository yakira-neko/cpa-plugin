package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestCreditSnapshotAggregatesRequestModelHourAndSession(t *testing.T) {
	resetCreditLog(t)
	at := time.Date(2026, 9, 3, 14, 0, 0, 0, time.Local)
	recordCreditUsage("u1", "a1", "alias", "glm-5.3", at, usageDetailLite{InputTokens: 1000, OutputTokens: 1000}, false, 200, "", time.Second, "s1")
	got := creditSnapshot("u1", 10, 24)
	if got["requests"].(int64) != 1 || got["credits"].(int64) == 0 {
		t.Fatalf("unexpected totals: %#v", got)
	}
	if len(got["models"].([]creditModelTotal)) != 1 || len(got["sessions"].([]creditSession)) != 1 {
		t.Fatalf("missing aggregates: %#v", got)
	}
}

func TestParseSessionIDFromRequest(t *testing.T) {
	if got := parseSessionIDFromRequest([]byte(`{"session_id":"session-42"}`)); got != "session-42" {
		t.Fatalf("got %q", got)
	}
	if got := parseSessionIDFromRequest([]byte(`{"messages":[{"content":[{"session_id":"nested"}]}]}`)); got != "nested" {
		t.Fatalf("got %q", got)
	}
}

// resetCreditLog isolates a test from the process-wide store.
func resetCreditLog(t *testing.T) {
	t.Helper()
	t.Setenv("WB_CREDIT_LOG_PATH", filepath.Join(t.TempDir(), "usage.jsonl"))
	creditLog = &creditLogStore{byAuth: map[string]*creditAuthLog{}, sessions: map[string]*creditSession{}}
	creditLogLoaded = false
}

// Cache reads and cache writes must be recorded as distinct counters: folding
// them into one field made the panel's read/write split impossible.
func TestRecordCreditUsageKeepsCacheReadAndWriteSeparate(t *testing.T) {
	resetCreditLog(t)
	at := time.Date(2026, 9, 3, 14, 0, 0, 0, time.Local)
	recordCreditUsage("u1", "a1", "alias", "glm-5.3", at, usageDetailLite{
		InputTokens:         100,
		OutputTokens:        50,
		CacheReadTokens:     2000,
		CacheCreationTokens: 300,
	}, false, 200, "", time.Second, "s1")

	got := creditSnapshot("u1", 10, 24)
	if got["cached_tokens"].(int64) != 2000 {
		t.Fatalf("cached_tokens=%v want 2000", got["cached_tokens"])
	}
	if got["cache_creation_tokens"].(int64) != 300 {
		t.Fatalf("cache_creation_tokens=%v want 300", got["cache_creation_tokens"])
	}
	models := got["models"].([]creditModelTotal)
	if len(models) != 1 || models[0].CachedTokens != 2000 || models[0].CacheCreationTokens != 300 {
		t.Fatalf("per-model cache aggregates wrong: %+v", models)
	}
	entries := got["entries"].([]creditEntry)
	if len(entries) != 1 || entries[0].CacheWrite != 300 {
		t.Fatalf("entry should carry cache write: %+v", entries)
	}
}

// CachedTokens (the OpenAI alias) must be used when CacheReadTokens is absent.
func TestRecordCreditUsageFallsBackToCachedTokens(t *testing.T) {
	resetCreditLog(t)
	recordCreditUsage("u1", "a1", "alias", "glm-5.3", time.Now(), usageDetailLite{
		InputTokens:  10,
		CachedTokens: 750,
	}, false, 200, "", time.Second, "")

	got := creditSnapshot("u1", 10, 24)
	if got["cached_tokens"].(int64) != 750 {
		t.Fatalf("cached_tokens=%v want 750", got["cached_tokens"])
	}
}

func TestCreditGlobalSummaryExposesCacheTokens(t *testing.T) {
	resetCreditLog(t)
	at := time.Now()
	recordCreditUsage("u1", "a1", "alias", "glm-5.3", at, usageDetailLite{
		InputTokens: 10, CacheReadTokens: 400, CacheCreationTokens: 60,
	}, false, 200, "", time.Second, "")
	recordCreditUsage("u2", "a2", "alias", "glm-5.3", at, usageDetailLite{
		InputTokens: 10, CacheReadTokens: 100, CacheCreationTokens: 40,
	}, false, 200, "", time.Second, "")

	sum := creditGlobalSummary()
	if sum["cached_tokens"].(int64) != 500 {
		t.Fatalf("cached_tokens=%v want 500", sum["cached_tokens"])
	}
	if sum["cache_creation_tokens"].(int64) != 100 {
		t.Fatalf("cache_creation_tokens=%v want 100", sum["cache_creation_tokens"])
	}
	models := sum["models"].([]creditModelTotal)
	if len(models) != 1 {
		t.Fatalf("want 1 model, got %d", len(models))
	}
	if models[0].CachedTokens != 500 || models[0].CacheCreationTokens != 100 {
		t.Fatalf("global per-model cache wrong: %+v", models[0])
	}
}

// TestRealCacheHitFlowsThroughAccounting replays the live-captured cache-hit
// numbers (4043 cache-read tokens, glm-5.3) through the full accounting path and
// asserts the panel-bound JSON carries them. Complements the parser-level
// regression in usage_detail_test.go.
func TestRealCacheHitFlowsThroughAccounting(t *testing.T) {
	resetCreditLog(t)
	recordCreditUsage("u-live", "idx-live", "glm-5.3", "glm-5.3", time.Now(), usageDetailLite{
		InputTokens:         4443,
		OutputTokens:        4,
		CachedTokens:        4043,
		CacheReadTokens:     4043,
		CacheCreationTokens: 0,
		TotalTokens:         4447,
	}, false, 200, "", time.Second, "")

	snap := creditSnapshot("u-live", 10, 24)
	if got := snap["cached_tokens"].(int64); got != 4043 {
		t.Fatalf("cached_tokens=%v want 4043", got)
	}
	if got := snap["cache_creation_tokens"].(int64); got != 0 {
		t.Fatalf("cache_creation_tokens=%v want 0", got)
	}
	models := snap["models"].([]creditModelTotal)
	if len(models) != 1 || models[0].CachedTokens != 4043 {
		t.Fatalf("per-model cached tokens wrong: %+v", models)
	}
	entries := snap["entries"].([]creditEntry)
	if len(entries) != 1 || entries[0].Cached != 4043 {
		t.Fatalf("entry cached tokens wrong: %+v", entries)
	}
	// The wire payload the panel consumes must expose both columns.
	blob, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(blob, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["cached_tokens"]; !ok {
		t.Error("panel JSON missing cached_tokens")
	}
	if _, ok := wire["cache_creation_tokens"]; !ok {
		t.Error("panel JSON missing cache_creation_tokens")
	}
}
