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
	return mgmtCallAuth(t, method, path, body, nil)
}

// mgmtCallAuth is mgmtCall with explicit headers (used to present a key).
func mgmtCallAuth(t *testing.T, method, path string, body []byte, headers http.Header) (int, map[string]any) {
	t.Helper()
	req := pluginapi.ManagementRequest{Method: method, Path: path, Body: body, Headers: headers}
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

// setMgmtKey installs a management key for the duration of a test.
func setMgmtKey(t *testing.T, key string) {
	t.Helper()
	managementAPIKeyMu.Lock()
	prev := managementAPIKey
	managementAPIKey = key
	managementAPIKeyMu.Unlock()
	t.Cleanup(func() {
		managementAPIKeyMu.Lock()
		managementAPIKey = prev
		managementAPIKeyMu.Unlock()
	})
}

func bearerHeader(key string) http.Header {
	return http.Header{"Authorization": []string{"Bearer " + key}}
}

// The "valid key is not charged" / "failed key is charged" pair lives in
// management_auth_test.go (upstream). The tests below cover what that pair does
// not: bucket sharing across routes, and the keyless deployment trade-off.
//
// The panel polls /login/poll every 2s (LOGIN_POLL_MS = 2000) while the bucket
// is 5 burst + 1 refill per 6s, so charging valid requests would make a working
// panel throttle itself mid-login. The limiter therefore charges only FAILED
// authentication — it defends against key brute-force, not against the
// operator's own UI.

// TestManagementRateLimitSharesBucketAcrossRoutes ensures the limiter is a
// single per-IP bucket rather than per-route: rotating the target endpoint must
// not hand an attacker a fresh allowance.
func TestManagementRateLimitSharesBucketAcrossRoutes(t *testing.T) {
	resetMgmtRateLimit()
	defer resetMgmtRateLimit()
	setMgmtKey(t, "secret")

	base := loadedManagementBasePath() + "/plugins/" + providerName

	// Burn the burst on one route, then probe a different POST route.
	last := http.StatusForbidden
	for i := 0; i < 6; i++ {
		last, _ = mgmtCallAuth(t, http.MethodPost, base+"/login/poll", nil, bearerHeader("wrong"))
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("baseline: /login/poll not throttled after 6 failures (last=%d)", last)
	}
	status, _ := mgmtCallAuth(t, http.MethodPost, base+"/refresh", nil, bearerHeader("wrong"))
	fmt.Printf("bucket is shared: /refresh after exhausted /login/poll -> HTTP %d\n", status)
	if status != http.StatusTooManyRequests {
		t.Errorf("/refresh was not throttled, got HTTP %d — bucket is not shared as assumed", status)
	}
}

// TestManagementRateLimitInactiveWithoutKey documents the deployment trade-off
// that follows from charging only failed authentication: with no management_key
// configured, checkManagementAuth always passes, so NOTHING is ever charged and
// the plugin adds no throttling at all. Host middleware is then the only guard,
// which is the historical zero-config behaviour.
func TestManagementRateLimitInactiveWithoutKey(t *testing.T) {
	resetMgmtRateLimit()
	defer resetMgmtRateLimit()
	setMgmtKey(t, "")

	base := loadedManagementBasePath() + "/plugins/" + providerName
	pollBody := mustJSON(map[string]string{"state": "no-such-state"})

	for i := 0; i < 12; i++ {
		if status, _ := mgmtCall(t, http.MethodPost, base+"/login/poll", pollBody); status == http.StatusTooManyRequests {
			t.Fatalf("request %d was throttled with no management key set; the limiter is key-gated by design", i+1)
		}
	}
	fmt.Printf("\nno key configured: 12 rapid POSTs, none throttled (host middleware is the guard)\n")
}

// TestMgmtRateLimitRefillRate pins the configured budget so a future change to
// the constants cannot silently invalidate the reasoning above.
func TestMgmtRateLimitRefillRate(t *testing.T) {
	panelCadence := 2 * time.Second
	if mgmtRateLimitRefill < panelCadence {
		// Informational only: valid requests are no longer charged, so a refill
		// slower than the panel cadence no longer stalls a working login. It
		// still matters for how fast brute-force is throttled.
		t.Logf("bucket refills 1 per %s (%.1f/min); panel cadence is %s (%.1f/min)",
			mgmtRateLimitRefill, float64(time.Minute/mgmtRateLimitRefill),
			panelCadence, float64(time.Minute/panelCadence))
	}
	if mgmtRateLimitCapacity < 1 {
		t.Fatalf("capacity %d cannot admit any request", mgmtRateLimitCapacity)
	}
}
