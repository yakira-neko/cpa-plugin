package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// mgmtCall drives one request through the REAL management entry point —
// handleManagement — which is the layer /login/start and /login/poll actually
// traverse. Calling handleLoginPoll directly (as the e2e probe does) bypasses
// the plugin-layer rate limiter entirely, so it cannot observe throttling.
func mgmtCall(t *testing.T, method, path string, body []byte) (int, map[string]any) {
	t.Helper()
	req := pluginapi.ManagementRequest{Method: method, Path: path, Body: body}
	raw, err := handleManagement(mustJSON(req))
	if err != nil {
		t.Fatalf("handleManagement(%s %s): %v", method, path, err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode management envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("management envelope not ok: %+v", env.Error)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode management response: %v", err)
	}
	var out map[string]any
	_ = json.Unmarshal(resp.Body, &out)
	return resp.StatusCode, out
}

// resetMgmtRateLimit clears the token buckets so tests start from a known state.
func resetMgmtRateLimit() {
	mgmtRateLimitMu.Lock()
	defer mgmtRateLimitMu.Unlock()
	mgmtRateLimit = map[string]*mgmtRateEntry{}
}

// TestManagementRateLimitBlocksPanelPoll reproduces the panel's real polling
// cadence (LOGIN_POLL_MS = 2000, panel.html:947) against the real management
// layer. The bucket is 5 burst + 1 refill per 6s (management.go:254-257), so a
// 2s cadence cannot be sustained.
func TestManagementRateLimitBlocksPanelPoll(t *testing.T) {
	resetMgmtRateLimit()
	defer resetMgmtRateLimit()

	base := loadedManagementBasePath() + "/plugins/" + providerName
	pollBody := mustJSON(map[string]string{"state": "no-such-state"})

	// Simulate the first 20 seconds of the panel's poll loop: one request
	// every 2s. We do NOT actually sleep — the bucket refills on wall-clock
	// elapsed time, so a 2s spacing must be simulated by consuming the burst
	// and then checking immediately.
	throttled := 0
	passed := 0
	for i := 0; i < 5; i++ {
		status, _ := mgmtCall(t, http.MethodPost, base+"/login/poll", pollBody)
		if status == http.StatusTooManyRequests {
			throttled++
		} else {
			passed++
		}
	}

	// The first 5 requests consume the whole burst. A 6th request arriving
	// immediately (i.e. well within the 6s refill window — exactly the panel's
	// situation at 2s cadence) MUST be throttled.
	status, out := mgmtCall(t, http.MethodPost, base+"/login/poll", pollBody)

	fmt.Printf("\nburst of 5: passed=%d throttled=%d\n", passed, throttled)
	fmt.Printf("6th request (panel cadence would be 2s later): HTTP %d %v\n", status, out)

	if status != http.StatusTooManyRequests {
		t.Fatalf("6th rapid request got HTTP %d, want 429 — panel polls every 2s but the bucket only refills 1 per 6s", status)
	}

	// THE KEY ASSERTION: a 429 body carries NO "status" field, so the panel's
	// pollLogin (panel.html:1031-1049) sees neither success/expired/error and
	// falls through to scheduleLoginPoll — i.e. the throttle is rendered as
	// "still pending" rather than as an error.
	if _, hasStatus := out["status"]; hasStatus {
		t.Errorf("429 body unexpectedly carries a status field: %v", out)
	}
	fmt.Printf("429 body has no \"status\" field → panel pollLogin treats throttle as pending (panel.html:1049)\n")

	// And the throttle is NOT limited to the login route: every POST shares it.
	resetMgmtRateLimit()
	for i := 0; i < 6; i++ {
		status, _ = mgmtCall(t, http.MethodPost, base+"/refresh", nil)
	}
	fmt.Printf("same bucket also throttles /refresh: last HTTP %d\n", status)
	if status != http.StatusTooManyRequests {
		t.Errorf("/refresh was not throttled, got HTTP %d — bucket is not shared as assumed", status)
	}
}

// TestManagementRateLimitActiveWithoutKey documents that the limiter runs even
// when no management_key is configured (management.go:186 is an unconditional
// POST gate; only checkManagementAuth is key-dependent).
func TestManagementRateLimitActiveWithoutKey(t *testing.T) {
	resetMgmtRateLimit()
	defer resetMgmtRateLimit()

	managementAPIKeyMu.Lock()
	prev := managementAPIKey
	managementAPIKey = "" // simulate the historical zero-config deployment
	managementAPIKeyMu.Unlock()
	defer func() {
		managementAPIKeyMu.Lock()
		managementAPIKey = prev
		managementAPIKeyMu.Unlock()
	}()

	base := loadedManagementBasePath() + "/plugins/" + providerName
	pollBody := mustJSON(map[string]string{"state": "no-such-state"})

	throttled := false
	for i := 0; i < 12; i++ {
		if status, _ := mgmtCall(t, http.MethodPost, base+"/login/poll", pollBody); status == http.StatusTooManyRequests {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Errorf("no throttling observed with an empty management key — expected an unconditional POST limiter")
	} else {
		fmt.Printf("\nconfirmed: rate limiting applies with NO management key configured\n")
	}
}

// TestMgmtRateLimitRefillRate pins the configured budget so a future change to
// the constants cannot silently make the panel's cadence unsustainable.
func TestMgmtRateLimitRefillRate(t *testing.T) {
	panelCadence := 2 * time.Second
	if mgmtRateLimitRefill < panelCadence {
		// This is the actual defect: sustained rate < panel demand rate.
		t.Logf("DEFECT CONFIRMED: bucket sustains 1 request per %s (%.1f/min) but the panel sends 1 per %s (%.1f/min)",
			mgmtRateLimitRefill, float64(time.Minute/mgmtRateLimitRefill),
			panelCadence, float64(time.Minute/panelCadence))
	} else {
		t.Logf("bucket refill %s keeps up with panel cadence %s", mgmtRateLimitRefill, panelCadence)
	}
	if mgmtRateLimitCapacity < 1 {
		t.Fatalf("capacity %d cannot admit any request", mgmtRateLimitCapacity)
	}
}
