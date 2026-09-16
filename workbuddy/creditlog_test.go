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
	dir := t.TempDir()
	// Drain any write still in flight from an earlier test so it cannot land in
	// this test's TempDir after cleanup.
	waitCreditPersist()
	t.Cleanup(waitCreditPersist)
	t.Setenv("WB_CREDIT_LOG_PATH", filepath.Join(dir, "usage.jsonl"))
	creditLog = &creditLogStore{byAuth: map[string]*creditAuthLog{}, sessions: map[string]*creditSession{}}
	creditLogLoaded = false
	// The rate card is process-wide too: a test that overrides a factor would
	// otherwise leak into every later credit assertion.
	resetCreditRates()
	t.Cleanup(resetCreditRates)
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

// The upstream folds cache reads INTO prompt_tokens (live fixture:
// prompt_tokens=4443 = 4043 cached + 400 miss). Pricing the raw prompt count
// charged every cache hit twice — once at full input rate, then again at the
// discounted cache rate — so the uncached remainder must be priced instead.
//
// Both requests below carry the same 4443 prompt tokens and the same 4043 cache
// reads; the second adds 4043 genuinely-uncached tokens. Only the second may
// cost more.
func TestRecordCreditUsage_DoesNotDoubleCountCacheReads(t *testing.T) {
	resetCreditLog(t)
	at := time.Now()

	// Cache-heavy: 400 uncached + 4043 cached.
	recordCreditUsage("u-cache", "a1", "glm-5.3", "glm-5.3", at, usageDetailLite{
		InputTokens:     4443,
		OutputTokens:    10,
		CacheReadTokens: 4043,
	}, false, 200, "", time.Second, "")

	// Same totals, but nothing cached: all 4443 prompt tokens are uncached.
	recordCreditUsage("u-nocache", "a2", "glm-5.3", "glm-5.3", at, usageDetailLite{
		InputTokens:     4443,
		OutputTokens:    10,
		CacheReadTokens: 0,
	}, false, 200, "", time.Second, "")

	cached := creditSnapshot("u-cache", 10, 24)["credits"].(int64)
	uncached := creditSnapshot("u-nocache", 10, 24)["credits"].(int64)

	if cached >= uncached {
		t.Fatalf("cache-heavy request cost %d credits vs %d uncached: a cache hit must be strictly cheaper", cached, uncached)
	}
	// The input tokens STAYED raw for the panel's volume columns, even though
	// only the uncached remainder was priced.
	if got := creditSnapshot("u-cache", 10, 24)["input_tokens"].(int64); got != 4443 {
		t.Fatalf("input_tokens=%d want 4443 (stored raw, not net of cache)", got)
	}
}

// A cached count larger than the prompt count must not produce a negative
// input charge (upstreams occasionally report the nested counter oddly).
func TestRecordCreditUsage_CacheReadsExceedingInputClampToZero(t *testing.T) {
	resetCreditLog(t)
	recordCreditUsage("u-clamp", "a1", "glm-5.3", "glm-5.3", time.Now(), usageDetailLite{
		InputTokens:     100,
		OutputTokens:    50,
		CacheReadTokens: 5000,
	}, false, 200, "", time.Second, "")

	got := creditSnapshot("u-clamp", 10, 24)["credits"].(int64)
	// Output (50 x 1.5 = 75 weighted) + 5000 cached x 0.1 = 575 → 0.575 → 1.
	// A negative input term would instead have wiped the output charge out.
	if got != 1 {
		t.Fatalf("credits=%d, want 1 (uncached input must clamp at 0, output still charged)", got)
	}
}
