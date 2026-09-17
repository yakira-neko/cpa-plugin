package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// captureUsageImport stands up a CPAMP-shaped /v0/management/usage/import
// endpoint, points the forwarder at it, runs call(), and returns the decoded
// NDJSON payload that actually crossed the wire.
//
// This is the tight feedback loop for the forwarded-token contract: it drives
// the real forwardUsageToCPAMP → hostHTTPDo → HTTP path with no stubs, so a
// wrong field value is observed exactly as CPAMP would ingest it.
func captureUsageImport(t *testing.T, call func()) map[string]any {
	t.Helper()
	got := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode forwarded payload: %v", err)
		}
		select {
		case got <- payload:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	restore := setUsageReportURLKey(srv.URL, "test-cpamp-key")
	defer restore()

	call()

	select {
	case payload := <-got:
		return payload
	case <-time.After(3 * time.Second):
		t.Fatal("no usage payload reached CPAMP")
		return nil
	}
}

// tokenInt reads one numeric field out of the forwarded "tokens" object.
func tokenInt(t *testing.T, payload map[string]any, key string) int64 {
	t.Helper()
	tokens, ok := payload["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("payload has no tokens object: %#v", payload)
	}
	raw, ok := tokens[key]
	if !ok {
		t.Fatalf("tokens.%s missing: %#v", key, tokens)
	}
	num, ok := raw.(float64)
	if !ok {
		t.Fatalf("tokens.%s is not numeric: %#v", key, raw)
	}
	return int64(num)
}

// TestForwardUsageToCPAMP_TotalNeverBelowInput pins the live bug (observed
// 2026-09 against copilot.tencent.com, glm-5.3):
//
// CodeBuddy reports prompt_tokens as cache-INCLUSIVE (150.6K, of which 72.4K
// were cache reads) but total_tokens as cache-EXCLUSIVE (84.5K = 150.6K -
// 72.4K + 6.3K). Forwarding both verbatim put CPAMP's 总量 BELOW its 输入 in the
// request-monitoring tooltip, which no consumer can reconcile: a
// cache-inclusive input can never exceed the request total.
//
// The forwarded total must therefore be cache-inclusive too.
func TestForwardUsageToCPAMP_TotalNeverBelowInput(t *testing.T) {
	const (
		in        = 150600 // prompt_tokens, cache-inclusive
		out       = 6300
		reasoning = 3640
		cacheRead = 72400
		// What the upstream reported: uncached prompt + completion.
		upstreamTotal = (in - cacheRead) + out
	)
	if upstreamTotal != 84500 {
		t.Fatalf("fixture drift: upstream total=%d want 84500 (the live 总量)", upstreamTotal)
	}

	payload := captureUsageImport(t, func() {
		forwardUsageToCPAMP("glm-5.3", "glm-5.3", "auth-1", time.Now(), usage.Detail{
			InputTokens:     in,
			OutputTokens:    out,
			ReasoningTokens: reasoning,
			CachedTokens:    cacheRead,
			CacheReadTokens: cacheRead,
			TotalTokens:     upstreamTotal,
		}, false, 200, "")
	})

	gotIn := tokenInt(t, payload, "input_tokens")
	gotTotal := tokenInt(t, payload, "total_tokens")
	if gotTotal < gotIn {
		t.Fatalf("total_tokens=%d < input_tokens=%d — CPAMP renders 总量 below 输入", gotTotal, gotIn)
	}
	if want := int64(in + out + reasoning); gotTotal != want {
		t.Fatalf("total_tokens=%d want %d (cache-inclusive input + output + reasoning)", gotTotal, want)
	}
	if gotIn != in {
		t.Fatalf("input_tokens=%d want %d (raw cache-inclusive prompt volume)", gotIn, in)
	}
	// CPAMP defaults an unclassified executor to separate_from_input, which
	// re-adds the cache buckets to the input and would report 223.0K. Declaring
	// the real convention keeps 输入 at the upstream's 150.6K.
	tokens := payload["tokens"].(map[string]any)
	if got := tokens["cache_input_mode"]; got != "included_in_input" {
		t.Fatalf("tokens.cache_input_mode=%v want included_in_input", got)
	}
}

// TestForwardUsageToCPAMP_KeepsCoherentUpstreamTotal guards the other direction:
// when the upstream's own total already exceeds the cache-inclusive input (the
// pinned live cache-hit fixture: prompt 4443 / completion 4 / total 4447) it is
// authoritative and must pass through untouched — no re-derivation, no
// reasoning double-count.
func TestForwardUsageToCPAMP_KeepsCoherentUpstreamTotal(t *testing.T) {
	cases := []struct {
		name   string
		detail usage.Detail
	}{
		{
			name: "pinned live cache-hit fixture",
			// prompt_tokens=4443 (4043 cached + 400 miss), completion 4, total 4447.
			detail: usage.Detail{
				InputTokens: 4443, OutputTokens: 4, TotalTokens: 4447,
				CachedTokens: 4043, CacheReadTokens: 4043,
			},
		},
		{
			// prompt 14 / completion 16 (reasoning 15 is already inside the
			// completion count) / total 30: re-deriving would wrongly yield 45.
			name: "cold response with nested reasoning",
			detail: usage.Detail{
				InputTokens: 14, OutputTokens: 16, ReasoningTokens: 15, TotalTokens: 30,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := captureUsageImport(t, func() {
				forwardUsageToCPAMP("glm-5.3", "glm-5.3", "auth-1", time.Now(), tc.detail, false, 200, "")
			})
			if got, want := tokenInt(t, payload, "total_tokens"), tc.detail.TotalTokens; got != want {
				t.Fatalf("total_tokens=%d want %d (coherent upstream total preserved)", got, want)
			}
		})
	}
}

// TestForwardUsageToCPAMP_NoUpstreamTotalStillCoherent covers the upstream that
// omits total_tokens entirely: the derived total must be cache-inclusive, not
// the sum of the raw prompt count plus the cache buckets (which double-counts
// every cache hit).
func TestForwardUsageToCPAMP_NoUpstreamTotalStillCoherent(t *testing.T) {
	payload := captureUsageImport(t, func() {
		forwardUsageToCPAMP("glm-5.3", "glm-5.3", "auth-1", time.Now(), usage.Detail{
			InputTokens: 1000, OutputTokens: 50, CacheReadTokens: 800,
		}, false, 200, "")
	})
	if got, want := tokenInt(t, payload, "total_tokens"), int64(1050); got != want {
		t.Fatalf("total_tokens=%d want %d (cache-inclusive input + output, cache not re-added)", got, want)
	}
	if got := tokenInt(t, payload, "input_tokens"); got != 1000 {
		t.Fatalf("input_tokens=%d want 1000", got)
	}
}
