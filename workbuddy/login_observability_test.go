package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// errTestSave simulates a host.auth.save RPC failure.
var errTestSave = errors.New("simulated host.auth.save failure")

// loginTestGateway serves a scripted auth/token sequence for one login flow.
type loginTestGateway struct {
	tokenBodies []string // consumed in order; last one repeats
	tokenCalls  int
	accountCode int // business code for login/account (0 = ok)
	seenState   string
}

func (g *loginTestGateway) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","requestId":"rid-state","data":{"state":"st-1","authUrl":"https://www.workbuddy.ai/login?state=st-1"}}`))
		case "/v2/plugin/auth/token":
			g.tokenCalls++
			g.seenState = r.URL.Query().Get("state")
			i := g.tokenCalls - 1
			if i >= len(g.tokenBodies) {
				i = len(g.tokenBodies) - 1
			}
			_, _ = w.Write([]byte(g.tokenBodies[i]))
		case "/v2/plugin/login/account":
			if g.accountCode != 0 {
				_, _ = w.Write([]byte(`{"code":` + itoa(g.accountCode) + `,"msg":"account lookup refused","requestId":"rid-acct"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","requestId":"rid-acct","data":{"uid":"u-1","nickname":"N"}}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func startLoginForTest(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	start := handleLoginStart(pluginapi.ManagementRequest{Body: []byte(`{"region":"global"}`)})
	state, _ := start["state"].(string)
	if state == "" {
		t.Fatalf("login/start returned no state: %v", start)
	}
	return state
}

func pollForTest(state string) map[string]any {
	pb, _ := json.Marshal(map[string]string{"state": state})
	return handleLoginPoll(pluginapi.ManagementRequest{Body: pb})
}

func installSaveHook(t *testing.T, capture *string) func() {
	t.Helper()
	return setHostCallHook(func(method string, request []byte) ([]byte, error) {
		var req pluginapi.HostAuthSaveRequest
		_ = json.Unmarshal(request, &req)
		if capture != nil {
			*capture = req.Name
		}
		res, _ := json.Marshal(pluginapi.HostAuthSaveResponse{Name: req.Name, Path: "/auth/" + req.Name})
		return okEnvelope(json.RawMessage(res))
	})
}

const liveSuccessBody = `{"code":0,"msg":"OK","requestId":"rid-tok","data":{"accessToken":"at-1234567890","refreshToken":"rt-1234567890","expiresIn":3600,"domain":"www.workbuddy.ai","scope":"s","sessionState":"ss","tokenType":"Bearer"}}`

// TestPendingCodeIsRecognised pins the ONE code that genuinely means "waiting",
// and that its identity is checked (not just "any non-zero code").
func TestPendingCodeIsRecognised(t *testing.T) {
	g := &loginTestGateway{tokenBodies: []string{`{"code":11217,"msg":"11217:login ing...","requestId":"rid-p"}`}}
	srv := httptest.NewServer(g.handler(t))
	defer srv.Close()
	defer setRegionBase(RegionGlobal, srv.URL)()
	defer installSaveHook(t, nil)()

	state := startLoginForTest(t, srv)
	out := pollForTest(state)

	if out["status"] != "pending" {
		t.Fatalf("status = %v, want pending (%v)", out["status"], out)
	}
	if !strings.Contains(fmtAny(out["message"]), "waiting") {
		t.Errorf("message = %v, want a waiting hint", out["message"])
	}
}

// TestTerminalBusinessCodeIsNotPending is the core regression: an upstream
// refusal that is NOT the known pending code must be reported as an error —
// previously every non-zero code was silently reported as pending, which hid
// real failures until the 5-minute TTL expired.
func TestTerminalBusinessCodeIsNotPending(t *testing.T) {
	// A code that is not the pending code, repeated enough to trip fast-fail.
	g := &loginTestGateway{tokenBodies: []string{`{"code":40001,"msg":"invalid state or unsupported platform","requestId":"rid-x"}`}}
	srv := httptest.NewServer(g.handler(t))
	defer srv.Close()
	defer setRegionBase(RegionGlobal, srv.URL)()
	defer installSaveHook(t, nil)()

	state := startLoginForTest(t, srv)

	// First occurrences are tolerated (an unknown pending variant must not
	// break a working login), so poll until it goes terminal.
	var out map[string]any
	for i := 0; i < loginUnrecognisedCodeFastFail+2; i++ {
		out = pollForTest(state)
		if out["status"] != "pending" {
			break
		}
	}
	if out["status"] == "pending" {
		t.Fatalf("unrecognised code %v still pending after %d polls — a terminal refusal is being hidden as a wait",
			out["status"], loginUnrecognisedCodeFastFail+2)
	}
	if out["status"] != "error" {
		t.Errorf("status = %v, want error", out["status"])
	}
	// The upstream detail must reach the user.
	if !strings.Contains(fmtAny(out["error"]), "40001") {
		t.Errorf("error %q does not mention the upstream code", out["error"])
	}
	if !strings.Contains(fmtAny(out["error"]), "rid-x") {
		t.Errorf("error %q does not carry the requestId (needed to ask upstream for a trace)", out["error"])
	}
}

// TestSuccessWithoutTokenIsErrorNotPending covers the other silent-wait path:
// upstream returns code 0 but the payload has no accessible credential. This
// must be an error, since the login will never progress.
func TestSuccessWithoutTokenIsErrorNotPending(t *testing.T) {
	g := &loginTestGateway{tokenBodies: []string{`{"code":0,"msg":"OK","requestId":"rid-empty","data":{"unexpected":"shape"}}`}}
	srv := httptest.NewServer(g.handler(t))
	defer srv.Close()
	defer setRegionBase(RegionGlobal, srv.URL)()
	defer installSaveHook(t, nil)()

	state := startLoginForTest(t, srv)
	out := pollForTest(state)

	if out["status"] == "pending" {
		t.Fatalf("code 0 without a token was reported as pending — a completed login would hang forever")
	}
	if out["status"] != "error" {
		t.Errorf("status = %v, want error", out["status"])
	}
	// The shape must be included so the mismatch is diagnosable without tokens.
	if !strings.Contains(fmtAny(out["error"]), "unexpected") {
		t.Errorf("error %q should include the response shape", out["error"])
	}
}

// TestExpiresInMissingIsWarned pins that an omitted expiresIn is recorded (the
// credential would otherwise be stamped as born-expired, silently).
func TestExpiresInMissingIsWarned(t *testing.T) {
	g := &loginTestGateway{tokenBodies: []string{`{"code":0,"msg":"OK","requestId":"rid-nx","data":{"accessToken":"at","refreshToken":"rt","domain":"www.workbuddy.ai"}}`}}
	srv := httptest.NewServer(g.handler(t))
	defer srv.Close()
	defer setRegionBase(RegionGlobal, srv.URL)()
	defer installSaveHook(t, nil)()

	state := startLoginForTest(t, srv)
	out := pollForTest(state)
	if out["status"] != "success" {
		t.Fatalf("status = %v, want success (%v)", out["status"], out)
	}

	found := false
	for _, e := range loginDiagEntries(state) {
		if e.Outcome == "warn_no_expires_in" {
			found = true
		}
	}
	if !found {
		t.Errorf("omitted expiresIn was not recorded as a warning: %+v", loginDiagEntries(state))
	}
}

// TestAccountLookupFailureIsVisible pins that a failed login/account call no
// longer disappears. It previously produced an empty UID, which degrades the
// saved file name to the bare workbuddy.json that hostAuthList filters out —
// making a real credential invisible to the panel with no signal at all.
func TestAccountLookupFailureIsVisible(t *testing.T) {
	g := &loginTestGateway{
		tokenBodies: []string{liveSuccessBody},
		accountCode: 40100, // account lookup refused
	}
	srv := httptest.NewServer(g.handler(t))
	defer srv.Close()
	defer setRegionBase(RegionGlobal, srv.URL)()

	var savedName string
	defer installSaveHook(t, &savedName)()

	state := startLoginForTest(t, srv)
	out := pollForTest(state)

	// The login itself succeeded — the token is valid.
	if out["status"] != "success" {
		t.Fatalf("status = %v, want success (%v)", out["status"], out)
	}
	// But the degraded identity must be explicit, and it must be recorded.
	if got := fmtAny(out["warning"]); got == "" {
		t.Errorf("no warning for a credential saved without a uid: %v", out)
	}
	recorded := false
	for _, e := range loginDiagEntries(state) {
		if e.Step == "login/account" && e.Outcome == "failed" {
			recorded = true
		}
	}
	if !recorded {
		t.Errorf("login/account failure was not recorded in diagnostics")
	}
	if !strings.EqualFold(savedName, authFileName) {
		t.Errorf("saved name = %q; expected the bare legacy name to be the degraded case under test", savedName)
	}
}

// TestSaveFailureKeepsStateRetryable pins the credential-safety property: a
// failed host.auth.save must not destroy a completed login. The upstream state
// is single-use and the credential existed only in that one response, so the
// implementation consumes the state but caches the credential for retry. The
// old code deleted the state AND kept no copy, making a save failure
// unrecoverable ("unknown state").
func TestSaveFailureKeepsStateRetryable(t *testing.T) {
	g := &loginTestGateway{tokenBodies: []string{liveSuccessBody}}
	srv := httptest.NewServer(g.handler(t))
	defer srv.Close()
	defer setRegionBase(RegionGlobal, srv.URL)()

	failFirst := true
	defer setHostCallHook(func(method string, request []byte) ([]byte, error) {
		if failFirst {
			failFirst = false
			return nil, errTestSave
		}
		var req pluginapi.HostAuthSaveRequest
		_ = json.Unmarshal(request, &req)
		res, _ := json.Marshal(pluginapi.HostAuthSaveResponse{Name: req.Name, Path: "/auth/" + req.Name})
		return okEnvelope(json.RawMessage(res))
	})()

	state := startLoginForTest(t, srv)

	first := pollForTest(state)
	if first["status"] != "error" {
		t.Fatalf("first poll status = %v, want error", first["status"])
	}
	if first["retryable"] != true {
		t.Errorf("save failure should be marked retryable: %v", first)
	}

	// The credential must survive the save failure so the login is recoverable.
	if _, ok := loginResultTake(state); !ok {
		t.Fatalf("no cached credential after a FAILED save — the completed login is destroyed and retry impossible")
	}

	second := pollForTest(state)
	if second["status"] != "success" {
		t.Fatalf("retry after save failure = %v, want success (err=%v) — a completed login must be recoverable",
			second["status"], second["error"])
	}
	// After a successful save the cached copy is released.
	if _, ok := loginResultTake(state); ok {
		t.Errorf("cached credential should be forgotten after a successful save")
	}
}

// TestRealCapturedSequenceEndToEnd replays the exact sequence observed against
// the live Global gateway (www.workbuddy.ai, 2026-09-15): several short
// "11217:login ing..." polls, then the real success payload, then login/account.
//
// The success body below is the SHAPE captured live — accessToken/refreshToken/
// expiresIn/domain/scope/sessionState/tokenType — so this pins the real
// contract without needing a browser, and covers the state-consumption and
// credential-caching changes on the success path.
func TestRealCapturedSequenceEndToEnd(t *testing.T) {
	// Real success SHAPE captured live from www.workbuddy.ai (2026-09-15):
	// accessToken / refreshToken / expiresIn / domain / scope / sessionState /
	// tokenType. Token values are synthetic but length-accurate.
	realSuccess := `{"code":0,"msg":"OK","requestId":"9aa26504-b4f9-4f79-a2e0-7a9d6af8306b","data":{` +
		`"accessToken":"` + strings.Repeat("a", 1359) + `",` +
		`"refreshToken":"` + strings.Repeat("r", 700) + `",` +
		`"expiresIn":31416534,"domain":"www.workbuddy.ai","scope":"` + strings.Repeat("s", 35) + `",` +
		`"sessionState":"` + strings.Repeat("t", 36) + `","tokenType":"Bearer"}}`

	g := &loginTestGateway{tokenBodies: []string{
		`{"code":11217,"msg":"11217:login ing...","requestId":"rid-p1"}`,
		`{"code":11217,"msg":"11217:login ing...","requestId":"rid-p2"}`,
		realSuccess,
	}}
	srv := httptest.NewServer(g.handler(t))
	defer srv.Close()
	defer setRegionBase(RegionGlobal, srv.URL)()

	var savedName string
	var savedJSON []byte
	defer setHostCallHook(func(method string, request []byte) ([]byte, error) {
		var req pluginapi.HostAuthSaveRequest
		_ = json.Unmarshal(request, &req)
		savedName, savedJSON = req.Name, req.JSON
		res, _ := json.Marshal(pluginapi.HostAuthSaveResponse{Name: req.Name, Path: "/auth/" + req.Name})
		return okEnvelope(json.RawMessage(res))
	})()

	state := startLoginForTest(t, srv)

	// Two real pending polls.
	for i := 0; i < 2; i++ {
		out := pollForTest(state)
		if out["status"] != "pending" {
			t.Fatalf("poll %d status = %v, want pending (%v)", i+1, out["status"], out)
		}
	}

	// Then the real success payload.
	out := pollForTest(state)
	if out["status"] != "success" {
		t.Fatalf("status = %v, want success (err=%v)", out["status"], out["error"])
	}
	if out["region"] != "global" {
		t.Errorf("region = %v, want global", out["region"])
	}
	if out["domain"] != "www.workbuddy.ai" {
		t.Errorf("domain = %v, want www.workbuddy.ai", out["domain"])
	}
	if out["uid"] != "u-1" || out["nickname"] != "N" {
		t.Errorf("identity = %v / %v", out["uid"], out["nickname"])
	}
	if savedName != "workbuddy-u-1.json" {
		t.Errorf("saved name = %q, want workbuddy-u-1.json", savedName)
	}
	sa, err := parseStored(savedJSON)
	if err != nil {
		t.Fatalf("saved credential unparseable: %v", err)
	}
	if !isGlobalDomain(sa.Auth.Domain) || accountRegion(sa) != "global" {
		t.Errorf("saved credential not classified Global: domain=%q region=%q", sa.Auth.Domain, accountRegion(sa))
	}
	if billingBaseFor(sa) != billingBaseGlobal {
		t.Errorf("billing base = %q, want %q", billingBaseFor(sa), billingBaseGlobal)
	}
	if len(sa.Auth.AccessToken) != 1359 || len(sa.Auth.RefreshToken) != 700 {
		t.Errorf("token lengths = %d / %d, want 1359 / 700", len(sa.Auth.AccessToken), len(sa.Auth.RefreshToken))
	}
	// expiresIn was present, so the credential must not be born-expired.
	if sa.Auth.ExpiresAt <= time.Now().Unix() {
		t.Errorf("expiresAt %d is not in the future despite expiresIn=31416534", sa.Auth.ExpiresAt)
	}
	// The state is consumed on success, but the cached copy was released too.
	if _, ok := loginResultTake(state); ok {
		t.Errorf("cached credential should be released after a successful save")
	}
	// A later poll must be terminal (single-use state), not a silent hang.
	after := pollForTest(state)
	if after["status"] == "pending" {
		t.Errorf("poll after success reported pending — must be terminal")
	}

	// Diagnostics must still summarize the flow without token material.
	blob := fmtAny(out["diagnostics"])
	if strings.Contains(blob, strings.Repeat("a", 40)) || strings.Contains(blob, strings.Repeat("r", 40)) {
		t.Errorf("diagnostics leaked token material")
	}
}

// TestHostDrivenPathStillConsumesState pins that the host-driven poll (which
// calls handlePollLogin directly and persists the credential itself) consumes
// the single-use state, while still allowing the credential to be replayed.
func TestHostDrivenPathStillConsumesState(t *testing.T) {
	g := &loginTestGateway{tokenBodies: []string{liveSuccessBody}}
	srv := httptest.NewServer(g.handler(t))
	defer srv.Close()
	defer setRegionBase(RegionGlobal, srv.URL)()
	defer installSaveHook(t, nil)()

	state := startLoginForTest(t, srv)

	body, err := handlePollLogin([]byte(`{"state":"` + state + `"}`))
	if err != nil {
		t.Fatalf("host-driven poll: %v", err)
	}
	if env := decodePoll(t, body); env.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("host-driven poll status = %q, want success", env.Status)
	}
	// The upstream state is single-use: it must no longer be pending.
	if _, ok := loginStates.Load(state); ok {
		t.Errorf("login state was not consumed on success")
	}
	// But the credential must remain recoverable for the host to persist.
	if _, ok := loginResultTake(state); !ok {
		t.Errorf("credential was not cached for the host-driven path")
	}
}

// TestDiagnosticsNeverContainTokens is the safety property for shipping
// diagnostics on by default: the payloads carry token material, so the recorded
// and returned diagnostics must be content-free.
func TestDiagnosticsNeverContainTokens(t *testing.T) {
	const accessToken = "SUPERSECRETACCESSVALUE1234567890"
	const refreshToken = "SUPERSECRETREFRESH9876543210"
	body := `{"code":0,"msg":"OK","requestId":"rid","data":{"accessToken":"` + accessToken +
		`","refreshToken":"` + refreshToken + `","expiresIn":3600,"domain":"www.workbuddy.ai"}}`

	g := &loginTestGateway{tokenBodies: []string{body}}
	srv := httptest.NewServer(g.handler(t))
	defer srv.Close()
	defer setRegionBase(RegionGlobal, srv.URL)()
	defer installSaveHook(t, nil)()

	state := startLoginForTest(t, srv)
	out := pollForTest(state)

	// Everything the panel renders / the diag endpoint returns:
	blob, _ := json.Marshal(out)
	for _, secret := range []string{accessToken, refreshToken} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("login/poll response leaked a token value")
		}
	}
	diag, _ := json.Marshal(loginDiagEntries(state))
	for _, secret := range []string{accessToken, refreshToken} {
		if strings.Contains(string(diag), secret) {
			t.Errorf("diagnostics leaked a token value")
		}
	}
	// And the shape renderer must be content-free even on a token payload.
	shape := jsonShape([]byte(body))
	for _, secret := range []string{accessToken, refreshToken} {
		if strings.Contains(shape, secret) {
			t.Errorf("jsonShape leaked a token value")
		}
	}
	if !strings.Contains(shape, "string(len=") {
		t.Errorf("jsonShape should report lengths, got: %s", shape)
	}
}

// TestLoginDiagEndpoint verifies the machine-readable diagnostic route.
func TestLoginDiagEndpoint(t *testing.T) {
	g := &loginTestGateway{tokenBodies: []string{`{"code":11217,"msg":"login ing","requestId":"rid-p"}`}}
	srv := httptest.NewServer(g.handler(t))
	defer srv.Close()
	defer setRegionBase(RegionGlobal, srv.URL)()
	defer installSaveHook(t, nil)()

	state := startLoginForTest(t, srv)
	_ = pollForTest(state)

	base := loadedManagementBasePath() + "/plugins/" + providerName + "/login/diag"
	status, out, _ := mgmtGetQuery(t, base, map[string]string{"state": state})
	if status != http.StatusOK {
		t.Fatalf("GET login/diag status = %d", status)
	}
	if fmtAny(out["state"]) != state {
		t.Errorf("state = %v, want %v", out["state"], state)
	}
	steps, _ := out["steps"].([]any)
	if len(steps) == 0 {
		t.Fatalf("no diagnostic steps recorded for a polled flow")
	}

	// Listing all flows must also work.
	status, all, _ := mgmtGetQuery(t, base, nil)
	if status != http.StatusOK {
		t.Fatalf("GET login/diag (list) status = %d", status)
	}
	if n, _ := all["count"].(float64); n < 1 {
		t.Errorf("flow list count = %v, want >= 1", all["count"])
	}
}

// mgmtGetQuery drives a GET through handleManagement with query parameters
// supplied the way the host supplies them (ManagementRequest.Query), not
// embedded in the path.
func mgmtGetQuery(t *testing.T, path string, query map[string]string) (int, map[string]any, string) {
	t.Helper()
	req := pluginapi.ManagementRequest{Method: http.MethodGet, Path: path}
	if len(query) > 0 {
		req.Query = url.Values{}
		for k, v := range query {
			req.Query.Set(k, v)
		}
	}
	raw, err := handleManagement(mustJSON(req))
	if err != nil {
		t.Fatalf("handleManagement GET %s: %v", path, err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %+v", env.Error)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var out map[string]any
	_ = json.Unmarshal(resp.Body, &out)
	return resp.StatusCode, out, string(resp.Body)
}

func fmtAny(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// TestDiagnosticsCollapseRepeatedPolls keeps the step log readable: a normal
// login polls every 2s for a minute or two, so without folding the log is two
// dozen identical "pending" lines and the one entry that differs is buried.
func TestDiagnosticsCollapseRepeatedPolls(t *testing.T) {
	const state = "collapse-test-state"
	loginDiagClear(state)
	defer loginDiagClear(state)

	for i := 0; i < 10; i++ {
		loginDiagAdd(state, loginDiagEntry{
			Step: "auth/token", Outcome: "pending", HTTPStatus: 200, Code: 11217,
			Msg: "11217:login ing...",
		})
	}
	entries := loginDiagEntries(state)
	if len(entries) != 1 {
		t.Fatalf("10 identical polls produced %d entries, want 1", len(entries))
	}
	if entries[0].Count != 10 {
		t.Errorf("collapsed count = %d, want 10", entries[0].Count)
	}

	// A differing outcome must start a NEW entry, so the interesting step stays
	// visible rather than being folded into the noise.
	loginDiagAdd(state, loginDiagEntry{Step: "auth/token", Outcome: "success", HTTPStatus: 200})
	entries = loginDiagEntries(state)
	if len(entries) != 2 {
		t.Fatalf("after a differing outcome: %d entries, want 2", len(entries))
	}
	if entries[1].Outcome != "success" {
		t.Errorf("last entry outcome = %q, want success", entries[1].Outcome)
	}

	// Fast-fail must count collapsed OCCURRENCES, not entries: 3 identical
	// unrecognised codes in one collapsed entry must still trip the rule.
	loginDiagClear(state)
	for i := 0; i < 3; i++ {
		loginDiagAdd(state, loginDiagEntry{
			Step: "auth/token", Outcome: "unrecognised_code_pending", Code: 40001,
		})
	}
	if got := loginDiagRepeatCount(state, "auth/token", 40001); got != 3 {
		t.Errorf("loginDiagRepeatCount = %d, want 3 (collapsed occurrences must be counted)", got)
	}
}
