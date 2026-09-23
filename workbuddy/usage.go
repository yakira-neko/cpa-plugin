// usage.go implements the UsagePlugin capability: the host calls handleUsage
// after every request with a canonical record, and we forward to CPAMP. The
// legacy publishUsage path is kept for hosts without UsagePlugin wiring.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleUsage is the UsagePlugin entry point. The host calls this after every
// request with the canonical usage record (it also records to its own
// DefaultManager, so we don't need to). Our job is just to forward to CPAMP.
//
// v0.7.0 compliance: this replaces the old pattern where each executor path
// called publishUsage directly (which both skipped the host audit and forced
// every call site to remember to publish).
func handleUsage(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	// Only forward workbuddy's own records; the host will route other plugins'
	// usage to their own UsagePlugin.
	if record.Provider != "" && record.Provider != providerName {
		return okEnvelope(map[string]any{"forwarded": false})
	}
	detail := usage.Detail{
		InputTokens:         record.Detail.InputTokens,
		OutputTokens:        record.Detail.OutputTokens,
		ReasoningTokens:     record.Detail.ReasoningTokens,
		CachedTokens:        record.Detail.CachedTokens,
		CacheReadTokens:     record.Detail.CacheReadTokens,
		CacheCreationTokens: record.Detail.CacheCreationTokens,
		TotalTokens:         record.Detail.TotalTokens,
	}
	started := record.RequestedAt
	if started.IsZero() {
		started = time.Now().Add(-record.Latency)
	}
	forwardUsageToCPAMP(
		record.Alias,
		record.Model,
		record.AuthID,
		started,
		detail,
		record.Failed,
		record.Failure.StatusCode,
		record.Failure.Body,
	)
	recordCreditUsage(record.AuthID, record.AuthID, record.Alias, record.Model, started, usageDetailLite{InputTokens: detail.InputTokens, OutputTokens: detail.OutputTokens, ReasoningTokens: detail.ReasoningTokens, CachedTokens: detail.CachedTokens, CacheReadTokens: detail.CacheReadTokens, CacheCreationTokens: detail.CacheCreationTokens, TotalTokens: detail.TotalTokens}, record.Failed, record.Failure.StatusCode, record.Failure.Body, record.Latency, "")
	return okEnvelope(map[string]any{"forwarded": true})
}

// publishUsage is kept for backward compatibility with existing call sites
// inside the executor. After v0.7.0 the host calls UsagePlugin.HandleUsage
// itself after every request, so executor-side publish is redundant. We
// forward to CPAMP here too so that hosts WITHOUT UsagePlugin wiring (older
// CPA builds) still emit usage; hosts with the wiring will trigger HandleUsage
// separately, but forwardUsageToCPAMP is idempotent at the CPAMP ingestion
// layer (NDJSON import dedups on timestamp+auth+model+total_tokens).
func publishUsage(requestedModel, upstreamModel, authID string, started time.Time, detail usage.Detail, failed bool, statusCode int, errBody string) {
	model := strings.TrimSpace(upstreamModel)
	if model == "" {
		model = strings.TrimSpace(requestedModel)
	}
	alias := strings.TrimSpace(requestedModel)
	if alias == "" {
		alias = model
	}
	recordCreditUsage(authID, authID, alias, model, started, usageDetailLite{InputTokens: detail.InputTokens, OutputTokens: detail.OutputTokens, ReasoningTokens: detail.ReasoningTokens, CachedTokens: detail.CachedTokens, CacheReadTokens: detail.CacheReadTokens, CacheCreationTokens: detail.CacheCreationTokens, TotalTokens: detail.TotalTokens}, failed, statusCode, errBody, time.Since(started), "")
	// Fire-and-forget so the executor hot path never blocks on the CPAMP
	// round-trip. handleUsage (the host-driven path) is synchronous because the
	// host already runs it on its own goroutine after the request completes.
	go forwardUsageToCPAMP(alias, model, authID, started, normalizeUsageDetail(detail), failed, statusCode, errBody)
}

// forwardUsageToCPAMP POSTs one NDJSON line to CPAMP usage/import.
// Silent on misconfig / network errors — never blocks chat.
//
// v0.7.0: routed via host.http.do so the call is captured by request-log and
// uses host transport policy (proxy, timeout). Was: raw http.Client.
func forwardUsageToCPAMP(alias, model, authID string, started time.Time, detail usage.Detail, failed bool, statusCode int, errBody string) {
	usageReportMu.RLock()
	url := strings.TrimSpace(usageReportURL)
	key := strings.TrimSpace(usageReportKey)
	usageReportMu.RUnlock()
	if url == "" || key == "" {
		return
	}
	ts := started
	if ts.IsZero() {
		ts = time.Now()
	}
	latencyMs := int64(0)
	if !started.IsZero() {
		latencyMs = time.Since(started).Milliseconds()
		if latencyMs < 0 {
			latencyMs = 0
		}
	}
	total := cacheInclusiveTotal(detail)
	failBody := ""
	failCode := 200
	if failed {
		failCode = statusCode
		if failCode <= 0 {
			failCode = 502
		}
		failBody = truncate(redactSecrets(errBody), 512)
	}
	payload := map[string]any{
		"timestamp":     ts.UTC().Format(time.RFC3339Nano),
		"latency_ms":    latencyMs,
		"source":        "workbuddy",
		"auth_index":    strings.TrimSpace(authID),
		"provider":      providerName,
		"model":         model,
		"alias":         alias,
		"endpoint":      "POST /v1/chat/completions",
		"auth_type":     "oauth",
		"executor_type": "workbuddy",
		"generate":      true,
		"failed":        failed,
		"tokens": map[string]any{
			"input_tokens":          detail.InputTokens,
			"output_tokens":         detail.OutputTokens,
			"reasoning_tokens":      detail.ReasoningTokens,
			"cached_tokens":         detail.CachedTokens,
			"cache_read_tokens":     detail.CacheReadTokens,
			"cache_creation_tokens": detail.CacheCreationTokens,
			"total_tokens":          total,
			// CodeBuddy reports prompt_tokens cache-INCLUSIVE while its
			// total_tokens is cache-EXCLUSIVE (live: prompt 150.6K incl. 72.4K
			// cache reads, total 84.5K). Declaring the convention stops CPAMP
			// from guessing: its NormalizeCacheAccounting defaults an
			// unclassified executor to separate_from_input, which would re-add
			// the cache buckets to an already-inclusive input and report 输入 as
			// 223.0K instead of 150.6K.
			"cache_input_mode": "included_in_input",
		},
		"fail": map[string]any{
			"status_code": failCode,
			"body":        failBody,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	body = append(body, '\n')
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := hostHTTPDo(req)
	if err != nil {
		return
	}
	_ = resp.Body
}

// cacheInclusiveTotal returns the total token count to forward to CPAMP.
//
// WorkBuddy's upstream mixes two conventions in one usage block: prompt_tokens
// is cache-INCLUSIVE (a cache read is still part of the prompt) while
// total_tokens is cache-EXCLUSIVE. Live CN sample (pinned 2026-09):
// prompt_tokens=150600 of which cache_read=72400, completion=6300,
// total_tokens=84500 — i.e. exactly (150600-72400)+6300. Forwarding both
// verbatim makes CPAMP's monitoring tooltip show 总量 (84.5K) BELOW 输入
// (150.6K), which is unreconcilable for any cost/token consumer.
//
// An upstream total is kept only when it is coherent with the cache-inclusive
// input (>= input), so the pinned live cache-hit fixture
// (prompt=4443/4043 cached, completion=4, total=4447) passes through untouched.
// Otherwise the total is derived cache-inclusively, which also avoids
// double-counting cache reads the way input+output+cacheRead would.
func cacheInclusiveTotal(d usage.Detail) int64 {
	if d.TotalTokens > 0 && d.TotalTokens >= d.InputTokens {
		return d.TotalTokens
	}
	return d.InputTokens + d.OutputTokens + d.ReasoningTokens
}

// normalizeUsageDetail returns d with TotalTokens filled in when absent.
func normalizeUsageDetail(d usage.Detail) usage.Detail {
	if d.TotalTokens == 0 {
		d.TotalTokens = cacheInclusiveTotal(d)
	}
	return d
}

// usageDetailFromMap converts an OpenAI-style "usage" JSON object into a
// usage.Detail, tolerating both snake_case naming and numeric jitter.
func usageDetailFromMap(m map[string]any) usage.Detail {
	if len(m) == 0 {
		return usage.Detail{}
	}
	num := func(keys ...string) int64 {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				switch n := v.(type) {
				case float64:
					return int64(n)
				case int64:
					return n
				case json.Number:
					i, _ := n.Int64()
					return i
				}
			}
		}
		return 0
	}
	// nested fetches a numeric field from a sub-object, e.g.
	// nested("prompt_tokens_details", "cached_tokens"). Upstreams report cache
	// counters either flat or nested depending on the API flavour, and the
	// flat-only form silently reported zero for OpenAI-style responses.
	nested := func(obj string, keys ...string) (int64, bool) {
		sub, ok := m[obj].(map[string]any)
		if !ok {
			return 0, false
		}
		for _, k := range keys {
			switch n := sub[k].(type) {
			case float64:
				return int64(n), true
			case int64:
				return n, true
			case json.Number:
				i, _ := n.Int64()
				return i, true
			}
		}
		return 0, false
	}
	d := usage.Detail{
		InputTokens:     num("prompt_tokens", "input_tokens"),
		OutputTokens:    num("completion_tokens", "output_tokens"),
		TotalTokens:     num("total_tokens"),
		CachedTokens:    num("cached_tokens"),
		CacheReadTokens: num("cache_read_input_tokens"),
		// Anthropic-flavoured cache-creation counter (flat).
		CacheCreationTokens: num("cache_creation_input_tokens"),
	}
	// OpenAI-style nested counters: prefer these when the flat forms were
	// absent, matching how the host parses its own upstream responses
	// (helps/usage_helpers.go parseOpenAIStyleUsageNode).
	if v, ok := nested("prompt_tokens_details", "cached_tokens"); ok && d.CachedTokens == 0 {
		d.CachedTokens = v
	}
	if v, ok := nested("input_tokens_details", "cached_tokens"); ok && d.CachedTokens == 0 {
		d.CachedTokens = v
	}
	if v, ok := nested("prompt_tokens_details", "cache_creation_tokens"); ok && d.CacheCreationTokens == 0 {
		d.CacheCreationTokens = v
	}
	if v, ok := nested("completion_tokens_details", "reasoning_tokens"); ok {
		d.ReasoningTokens = v
	} else if v, ok := nested("output_tokens_details", "reasoning_tokens"); ok {
		d.ReasoningTokens = v
	}
	// Cache reads are reported under different names per flavour; two of the
	// three are the same counter. Keep CacheReadTokens populated so the
	// panel's read/write split always has a value.
	if d.CacheReadTokens == 0 {
		d.CacheReadTokens = d.CachedTokens
	}
	return d
}

// usageDetailFromCompletion extracts the usage block from an aggregated
// non-streaming chat.completion payload.
func usageDetailFromCompletion(payload []byte) usage.Detail {
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return usage.Detail{}
	}
	m, _ := obj["usage"].(map[string]any)
	return usageDetailFromMap(m)
}

// sseUsageCollector scans upstream SSE chunks and keeps the last "usage"
// object seen (CodeBuddy emits it on the terminal chunk).
type sseUsageCollector struct {
	last map[string]any
}

func (c *sseUsageCollector) feed(rawJSON string) {
	var chunk map[string]any
	if json.Unmarshal([]byte(rawJSON), &chunk) != nil {
		return
	}
	if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
		c.last = u
	}
}

func (c *sseUsageCollector) detail() usage.Detail {
	return usageDetailFromMap(c.last)
}
