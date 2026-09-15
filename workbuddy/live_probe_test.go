//go:build live

// live_probe_test.go is an OPT-IN, network-touching diagnostic that drives the
// plugin's REAL login code path against the REAL CodeBuddy gateway —
// www.workbuddy.ai for Global, copilot.tencent.com for CN.
//
// The `live` build tag keeps it out of ordinary builds, so `go test ./...`
// stays hermetic and offline (CI relies on that).
//
// Run:
//
//	go test -tags live -run TestLivePanelLoginFlow -v -timeout 20m
//
// Env:
//
//	LIVE_WAIT    how long to wait for the browser login (default 10m), e.g. 45s
//	LIVE_REGION  cn | global (default global); cn is the control
//
// DESIGN NOTE — why this drives handleLoginPoll and not raw polling:
// auth/token is single-use. An earlier version of this probe polled auth/token
// with its own client to observe the success payload, which CONSUMED the
// credential, so the subsequent handleLoginPoll could only report `pending`.
// That experiment proved the success shape is decodable (see findings) but it
// measures the wrong thing for persistence. This version polls exclusively
// through handleLoginPoll — the exact function the panel's /login/poll route
// calls — so the state is consumed once, by the code under test, and the probe
// observes what the panel would actually receive.
//
// Output prints response SHAPE (key names, nesting, value lengths), never token
// material, so it is safe to paste back for analysis.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func liveRegion(t *testing.T) Region {
	t.Helper()
	v := strings.TrimSpace(os.Getenv("LIVE_REGION"))
	if v == "" {
		return RegionGlobal
	}
	return normalizeRegion(v)
}

func liveWait(t *testing.T) time.Duration {
	t.Helper()
	v := strings.TrimSpace(os.Getenv("LIVE_WAIT"))
	if v == "" {
		return 10 * time.Minute
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("LIVE_WAIT=%q: %v", v, err)
	}
	return d
}

// shapeOf renders a JSON value's STRUCTURE — key names, nesting, and value
// lengths — with no token material, so the output can be shared safely.
func shapeOf(v any, depth int) string {
	indent := strings.Repeat("  ", depth)
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("{\n")
		for _, k := range keys {
			b.WriteString(indent + "  " + k + ": " + shapeOf(t[k], depth+1) + ",\n")
		}
		b.WriteString(indent + "}")
		return b.String()
	case []any:
		return fmt.Sprintf("[%d items]", len(t))
	case string:
		return fmt.Sprintf("string(len=%d empty=%v)", len(t), t == "")
	case float64:
		return fmt.Sprintf("number(%v)", t)
	case bool:
		return fmt.Sprintf("bool(%v)", t)
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", t)
	}
}

func shapeOfRaw(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Sprintf("<not JSON: %d bytes>", len(raw))
	}
	return shapeOf(v, 0)
}

// mgmtPoll drives one request through the REAL management entry point
// (handleManagement) and returns the HTTP status plus the decoded body. This is
// what the panel's fetch() actually reaches, so it includes the plugin-layer
// rate limiter that calling handleLoginPoll directly would skip.
func mgmtPoll(t *testing.T, path string, body []byte) (int, map[string]any, string) {
	t.Helper()
	req := pluginapi.ManagementRequest{Method: http.MethodPost, Path: path, Body: body}
	raw, err := handleManagement(mustJSON(req))
	if err != nil {
		t.Fatalf("handleManagement(%s): %v", path, err)
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
	return resp.StatusCode, out, string(resp.Body)
}

// TestLivePanelLoginFlow drives startLoginFlow + handleLoginPoll in a loop —
// the same two functions /login/start and /login/poll call — against the real
// gateway, with the host.auth.save seam installed from the start so the exact
// persisted payload is observable.
func TestLivePanelLoginFlow(t *testing.T) {
	region := liveRegion(t)
	wait := liveWait(t)

	fmt.Printf("\n############ LIVE PANEL-PATH PROBE: region=%s wait=%s ############\n", region, wait)

	// ---- host.auth.save seam: capture what would be persisted -----------
	var savedName string
	var savedJSON []byte
	saveCalls := 0
	var saveErr error
	restore := setHostCallHook(func(method string, request []byte) ([]byte, error) {
		if method != pluginabi.MethodHostAuthSave {
			return nil, fmt.Errorf("probe does not emulate host method %q", method)
		}
		saveCalls++
		var r pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(request, &r); err != nil {
			saveErr = err
			return nil, err
		}
		savedName, savedJSON = r.Name, r.JSON
		res, _ := json.Marshal(pluginapi.HostAuthSaveResponse{Name: r.Name, Path: "/probe-auth/" + r.Name})
		return okEnvelope(res)
	})
	defer restore()

	// ---- STEP 1: /login/start equivalent --------------------------------
	start, err := startLoginFlow(region)
	if err != nil {
		t.Fatalf("startLoginFlow(%s): %v", region, err)
	}
	fmt.Printf("\n--- STEP 1: auth/state (plugin startLoginFlow) ---\n")
	fmt.Printf("state     : %s\n", start.State)
	fmt.Printf("expiresAt : %s\n", start.ExpiresAt.UTC().Format(time.RFC3339))
	fmt.Printf("authUrl   : %s\n", start.URL)
	fmt.Printf("metadata  : %v\n", start.Metadata)

	v, ok := loginStates.Load(start.State)
	if !ok {
		t.Fatalf("state %q not registered in loginStates", start.State)
	}
	lc, ok := v.(*loginCtx)
	if !ok {
		t.Fatalf("loginStates held %T, want *loginCtx", v)
	}
	if lc.region != region {
		t.Errorf("loginCtx.region=%q, want %q", lc.region, region)
	}

	// Cookie affinity: the plugin's design assumes one jar carries session
	// state from auth/state through the polls.
	if originURL, uErr := url.Parse(region.baseURL()); uErr == nil {
		n := len(lc.client.Jar.Cookies(originURL))
		fmt.Printf("cookies issued by auth/state: %d", n)
		if n == 0 {
			fmt.Printf("  (none — auth/token must be keyed by the state value alone)")
		}
		fmt.Printf("\n")
	}

	fmt.Printf("\n>>> OPEN THIS URL IN A BROWSER AND LOG IN:\n%s\n\n", start.URL)

	// ---- STEP 2: the panel's poll loop, through the MANAGEMENT layer ---
	// This is the layer /login/poll actually traverses. Driving
	// handleLoginPoll directly (as an earlier version of this probe did)
	// bypasses the plugin-layer rate limiter in handleManagement, which is
	// exactly where the panel's 2s cadence collides with the 1-per-6s bucket.
	deadline := time.Now().Add(wait)
	pollBody, _ := json.Marshal(map[string]string{"state": start.State})
	mgmtPath := loadedManagementBasePath() + "/plugins/" + providerName + "/login/poll"
	rateLimited := 0
	attempt := 0
	var final map[string]any
	started := time.Now()
	for time.Now().Before(deadline) {
		attempt++
		time.Sleep(2 * time.Second) // LOGIN_POLL_MS (panel.html:947)

		status, out, rawOut := mgmtPoll(t, mgmtPath, pollBody)
		final = out
		if status == http.StatusTooManyRequests {
			rateLimited++
			if rateLimited == 1 {
				fmt.Printf("poll %3d: HTTP 429 (rate limited) — body: %s\n", attempt, rawOut)
			}
			continue
		}
		st := fmt.Sprint(out["status"])
		switch st {
		case "pending":
			if attempt%15 == 1 {
				fmt.Printf("poll %3d: HTTP %d pending\n", attempt, status)
			}
		case "success":
			fmt.Printf("poll %3d: HTTP %d SUCCESS after %s\n", attempt, status, time.Since(started).Round(time.Second))
		default:
			fmt.Printf("poll %3d: HTTP %d %s -- %v\n", attempt, status, st, out["error"])
		}
		if st != "pending" {
			break
		}
	}
	fmt.Printf("\npolls=%d  rate-limited(429)=%d  (%.0f%% of polls wasted)\n",
		attempt, rateLimited, 100*float64(rateLimited)/float64(max(attempt, 1)))
	if rateLimited > 0 {
		fmt.Printf("*** The panel's 2s cadence exceeds the 5-burst/1-per-6s bucket, so most\n")
		fmt.Printf("*** polls are rejected. A 429 body has no \"status\" field, so panel.html:1049\n")
		fmt.Printf("*** treats it as pending and keeps polling — the login looks stuck.\n")
	}

	// ---- STEP 3: what the panel got -------------------------------------
	fmt.Printf("\n--- STEP 3: final /login/poll response (what the panel renders) ---\n")
	if final == nil {
		fmt.Printf("(no non-429 response received)\n")
	} else {
		keys := make([]string, 0, len(final))
		for k := range final {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%-9s: %v\n", k, final[k])
		}
	}

	// ---- STEP 4: what would actually be persisted ------------------------
	fmt.Printf("\n--- STEP 4: persisted credential ---------------------------\n")
	fmt.Printf("host.auth.save calls : %d\n", saveCalls)
	fmt.Printf("save error           : %v\n", saveErr)
	fmt.Printf("saved file name      : %q\n", savedName)
	if len(savedJSON) > 0 {
		fmt.Printf("saved JSON shape:\n%s\n", shapeOfRaw(savedJSON))
		if sa, perr := parseStored(savedJSON); perr == nil {
			fmt.Printf("\naccountRegion   : %q\n", accountRegion(sa))
			fmt.Printf("isGlobalDomain  : %v\n", isGlobalDomain(sa.Auth.Domain))
			fmt.Printf("billingBase     : %q\n", billingBaseFor(sa))
			fmt.Printf("authFileNameFor : %q\n", authFileNameFor(sa))
			fmt.Printf("uid non-empty   : %v\n", sa.Account.UID != "")
			fmt.Printf("nickname        : %q\n", sa.Account.Nickname)
			fmt.Printf("expiresAt       : %d\n", sa.Auth.ExpiresAt)
			if sa.Auth.ExpiresAt > 0 {
				fmt.Printf("expiresIn ~     : %s\n", time.Until(time.Unix(sa.Auth.ExpiresAt, 0)).Round(time.Minute))
			}
			if sa.Account.UID == "" {
				fmt.Printf("\n*** UID EMPTY → name falls back to %q, which hostAuthList() filters out\n", authFileName)
				fmt.Printf("*** (host_auth.go:52-54) — the account would be INVISIBLE to the panel.\n")
			}
		} else {
			t.Errorf("persisted credential unparseable: %v", perr)
		}
	}

	if got := fmt.Sprint(final["status"]); got != "success" {
		t.Errorf("panel login ended %q, want success (attempts=%d, rateLimited=%d)", got, attempt, rateLimited)
	}
	if strings.EqualFold(savedName, authFileName) {
		t.Errorf("credential saved as bare %q (no UID) — hostAuthList filters that name out, so the account would be invisible", savedName)
	}
}
