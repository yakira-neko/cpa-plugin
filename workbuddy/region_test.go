package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestIsGlobalDomain(t *testing.T) {
	cases := []struct {
		domain string
		want   bool
	}{
		{"www.workbuddy.ai", true},
		{"workbuddy.ai", true},
		{"www.codebuddy.cn", false},
		{"", false},
		{"WORKBUDDY.AI", true},
		{"  www.workbuddy.ai  ", true},
		// A-33: substring match was too loose
		{"evilworkbuddy.ai", false},
		{"workbuddy.ai.evil.com", false},
		{"notworkbuddy.ai", false},
	}
	for _, tc := range cases {
		if got := isGlobalDomain(tc.domain); got != tc.want {
			t.Errorf("isGlobalDomain(%q) = %v, want %v", tc.domain, got, tc.want)
		}
	}
}

func TestAccountRegion(t *testing.T) {
	cases := []struct {
		name   string
		sa     *storedAuth
		region string
	}{
		{"nil", nil, "cn"},
		{"empty domain", &storedAuth{}, "cn"},
		{"CN domain", &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}, "cn"},
		{"Global domain", &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}}, "global"},
	}
	for _, tc := range cases {
		if got := accountRegion(tc.sa); got != tc.region {
			t.Errorf("%s: accountRegion() = %q, want %q", tc.name, got, tc.region)
		}
	}
}

func TestBillingBaseFor(t *testing.T) {
	cases := []struct {
		name string
		sa   *storedAuth
		want string
	}{
		{"nil", nil, billingBase},
		{"empty domain", &storedAuth{}, billingBase},
		{"CN domain", &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}, billingBase},
		{"Global domain", &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}}, billingBaseGlobal},
	}
	for _, tc := range cases {
		if got := billingBaseFor(tc.sa); got != tc.want {
			t.Errorf("%s: billingBaseFor() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestOriginRefererFor(t *testing.T) {
	cases := []struct {
		name string
		sa   *storedAuth
		want string
	}{
		{"nil", nil, originReferer},
		{"empty domain", &storedAuth{}, originReferer},
		{"CN domain", &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}, originReferer},
		{"Global domain", &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}}, "https://www.workbuddy.ai"},
	}
	for _, tc := range cases {
		if got := originRefererFor(tc.sa); got != tc.want {
			t.Errorf("%s: originRefererFor() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// -----------------------------------------------------------------------------
// Global login routing
// -----------------------------------------------------------------------------

func TestNormalizeRegion(t *testing.T) {
	cases := []struct {
		in   string
		want Region
	}{
		{"global", RegionGlobal},
		{"Global", RegionGlobal},
		{"  GLOBAL  ", RegionGlobal},
		{"intl", RegionGlobal},
		{"international", RegionGlobal},
		{"workbuddy.ai", RegionGlobal},
		{"www.workbuddy.ai", RegionGlobal},
		{"overseas", RegionGlobal},
		{"cn", RegionCN},
		{"", RegionCN},
		{"bogus", RegionCN},
		{"codebuddy.cn", RegionCN},
	}
	for _, tc := range cases {
		if got := normalizeRegion(tc.in); got != tc.want {
			t.Errorf("normalizeRegion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRegionEndpoints(t *testing.T) {
	cases := []struct {
		region   Region
		wantBase string
	}{
		{RegionCN, "https://copilot.tencent.com"},
		{RegionGlobal, "https://www.workbuddy.ai"},
	}
	for _, tc := range cases {
		if got := tc.region.baseURL(); got != tc.wantBase {
			t.Errorf("%s baseURL() = %q, want %q", tc.region, got, tc.wantBase)
		}
		if got := tc.region.authStateURL(); got != tc.wantBase+"/v2/plugin/auth/state?platform=CLI" {
			t.Errorf("%s authStateURL() = %q", tc.region, got)
		}
		if got := tc.region.authTokenURL("S"); got != tc.wantBase+"/v2/plugin/auth/token?state=S" {
			t.Errorf("%s authTokenURL() = %q", tc.region, got)
		}
		if got := tc.region.loginAcctURL("S"); got != tc.wantBase+"/v2/plugin/login/account?state=S" {
			t.Errorf("%s loginAcctURL() = %q", tc.region, got)
		}
	}
}

// TestRegionOriginReferer pins the Origin/Referer pair per region — a Global
// login polled with a CN Origin is rejected by the workbuddy.ai gateway.
func TestRegionOriginReferer(t *testing.T) {
	if got := RegionCN.originRefererURL(); got != originReferer {
		t.Errorf("CN origin = %q, want %q", got, originReferer)
	}
	if got := RegionGlobal.originRefererURL(); got != originRefererGlobal {
		t.Errorf("Global origin = %q, want %q", got, originRefererGlobal)
	}
}

// TestDomainForRegion pins the fallback that keeps a Global login on the Global
// side of every downstream branch (billing base, chat base, check-in skip,
// trial eligibility, exhaust-delete policy) when upstream omits `domain`.
func TestDomainForRegion(t *testing.T) {
	cases := []struct {
		name     string
		upstream string
		region   Region
		want     string
	}{
		{"upstream wins (global)", "www.workbuddy.ai", RegionCN, "www.workbuddy.ai"},
		{"upstream wins (cn)", "www.codebuddy.cn", RegionGlobal, "www.codebuddy.cn"},
		{"empty global -> workbuddy.ai", "", RegionGlobal, "www.workbuddy.ai"},
		{"empty cn -> codebuddy.cn", "", RegionCN, "www.codebuddy.cn"},
		{"blank global -> workbuddy.ai", "   ", RegionGlobal, "www.workbuddy.ai"},
	}
	for _, tc := range cases {
		got := domainForRegion(tc.upstream, tc.region)
		if got != tc.want {
			t.Errorf("%s: domainForRegion(%q, %q) = %q, want %q", tc.name, tc.upstream, tc.region, got, tc.want)
		}
	}
	// The fallback (no upstream domain) must land on the region's own side of
	// isGlobalDomain, and an upstream-provided domain must be trusted as-is.
	for _, region := range []Region{RegionCN, RegionGlobal} {
		got := domainForRegion("", region)
		wantGlobal := region == RegionGlobal
		if isGlobalDomain(got) != wantGlobal {
			t.Errorf("region %s: fallback domain %q classified global=%v, want %v",
				region, got, isGlobalDomain(got), wantGlobal)
		}
	}
}

// TestRegionFromStartRequest covers how a region reaches handleStartLogin. The
// CPA host has no region selector on its login card, so absence of any hint
// must keep the historical CN default rather than silently switching to Global.
func TestRegionFromStartRequest(t *testing.T) {
	restore := setDefaultLoginRegion("")
	defer restore()

	// No payload at all -> CN.
	if got := regionFromStartRequest(nil); got != RegionCN {
		t.Errorf("nil payload -> %q, want cn", got)
	}
	// Metadata (host-defined login context).
	meta, _ := json.Marshal(pluginapi.AuthLoginStartRequest{
		Provider: providerName,
		Metadata: map[string]any{"region": "global"},
	})
	if got := regionFromStartRequest(meta); got != RegionGlobal {
		t.Errorf("metadata region=global -> %q, want global", got)
	}
	// Top-level region field (management-style payload).
	flat, _ := json.Marshal(map[string]string{"region": "global"})
	if got := regionFromStartRequest(flat); got != RegionGlobal {
		t.Errorf("flat region=global -> %q, want global", got)
	}
	// Unknown value degrades to CN, never errors.
	bogus, _ := json.Marshal(map[string]string{"region": "nonsense"})
	if got := regionFromStartRequest(bogus); got != RegionCN {
		t.Errorf("bogus region -> %q, want cn", got)
	}
	// Config default applies when the request carries no hint.
	setDefaultLoginRegion("global")
	if got := regionFromStartRequest(nil); got != RegionGlobal {
		t.Errorf("default_region=global, nil payload -> %q, want global", got)
	}
	// ...but an explicit request hint still wins over the default.
	cnHint, _ := json.Marshal(map[string]string{"region": "cn"})
	if got := regionFromStartRequest(cnHint); got != RegionCN {
		t.Errorf("explicit cn should beat default_region=global, got %q", got)
	}
}

// TestStartLoginFlow_GlobalURL verifies a Global start registers the flow and
// returns the workbuddy.ai login URL.
func TestStartLoginFlow_GlobalURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/plugin/auth/state" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("platform"); got != "CLI" {
			t.Errorf("platform = %q, want CLI", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"state":"st-global","authUrl":"https://www.workbuddy.ai/login?platform=CLI&state=st-global"}}`))
	}))
	defer srv.Close()
	defer setRegionBase(RegionGlobal, srv.URL)()

	resp, err := startLoginFlow(RegionGlobal)
	if err != nil {
		t.Fatalf("startLoginFlow(global): %v", err)
	}
	if resp.State != "st-global" {
		t.Errorf("state = %q, want st-global", resp.State)
	}
	if !strings.Contains(resp.URL, "www.workbuddy.ai/login") {
		t.Errorf("url = %q, want workbuddy.ai login", resp.URL)
	}
	if got := resp.Metadata["region"]; got != string(RegionGlobal) {
		t.Errorf("metadata region = %v, want global", got)
	}

	// The flow must be registered for polling, pinned to Global.
	v, ok := loginStates.Load("st-global")
	if !ok {
		t.Fatal("login state not registered")
	}
	lc := v.(*loginCtx)
	if lc.region != RegionGlobal {
		t.Errorf("loginCtx.region = %q, want global", lc.region)
	}
	loginStates.Delete("st-global")
}

// TestStartLoginFlow_CNURL pins the CN path (regression guard: the historic
// behaviour must be unchanged).
func TestStartLoginFlow_CNURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"state":"st-cn","authUrl":"https://copilot.tencent.com/login?platform=CLI&state=st-cn"}}`))
	}))
	defer srv.Close()
	defer setRegionBase(RegionCN, srv.URL)()

	resp, err := startLoginFlow(RegionCN)
	if err != nil {
		t.Fatalf("startLoginFlow(cn): %v", err)
	}
	v, ok := loginStates.Load("st-cn")
	if !ok {
		t.Fatal("login state not registered")
	}
	if lc := v.(*loginCtx); lc.region != RegionCN {
		t.Errorf("loginCtx.region = %q, want cn", lc.region)
	}
	loginStates.Delete("st-cn")
	_ = resp
}

// TestStartLoginFlow_RegionAndProfileAreIndependent pins the coupling the
// region-routing and OAuth-profile features share: they are orthogonal axes,
// and the merge that combined them must not let either one win.
//
// Region picks the GATEWAY HOST; the profile picks the PLATFORM query and the
// header set. A desktop-profile login against Global must therefore talk to the
// Global host while still carrying platform=workbuddy and the app's headers —
// the two features were developed on separate branches, so each one's own tests
// pass while this combination is broken. That is exactly the shape of bug a
// merge introduces, so it gets its own test.
func TestStartLoginFlow_RegionAndProfileAreIndependent(t *testing.T) {
	oldFeatures := featureRuntime.Load()
	desktop, err := parseFeatureRuntime([]byte("oauth_client_mode: workbuddy\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(desktop)
	t.Cleanup(func() { featureRuntime.Store(oldFeatures) })

	var gotPath, gotPlatform, gotUA, gotOrigin string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotPlatform = r.URL.Query().Get("platform")
		gotUA = r.Header.Get("User-Agent")
		gotOrigin = r.Header.Get("Origin")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"state":"st-global-desktop","authUrl":"https://www.workbuddy.ai/login?state=st-global-desktop"}}`))
	}))
	defer srv.Close()
	defer setRegionBase(RegionGlobal, srv.URL)()

	resp, err := startLoginFlow(RegionGlobal)
	if err != nil {
		t.Fatalf("startLoginFlow(global, desktop profile): %v", err)
	}
	if gotPath != "/v2/plugin/auth/state" {
		t.Errorf("path = %q, want the auth-state endpoint", gotPath)
	}
	// The profile axis wins on the query and headers...
	if gotPlatform != platformDesktop {
		t.Errorf("platform = %q, want %q (the profile must survive region routing)", gotPlatform, platformDesktop)
	}
	if gotUA != "WorkBuddy/5.3.14 WorkBuddy/5.3.14 CLI/2.115.0" {
		t.Errorf("User-Agent = %q, want the desktop profile's", gotUA)
	}
	if gotOrigin != "https://www.workbuddy.cn" {
		t.Errorf("Origin = %q, want the desktop profile's", gotOrigin)
	}
	// ...and the region axis still decides whose gateway was contacted.
	if !strings.Contains(srv.URL, "127.0.0.1") {
		t.Fatalf("test server URL = %q, expected a local override", srv.URL)
	}
	v, ok := loginStates.Load("st-global-desktop")
	if !ok {
		t.Fatal("login state not registered")
	}
	lc := v.(*loginCtx)
	if lc.region != RegionGlobal {
		t.Errorf("loginCtx.region = %q, want global", lc.region)
	}
	if lc.profile.mode != oauthClientModeWorkBuddy {
		t.Errorf("loginCtx.profile.mode = %q, want the desktop profile", lc.profile.mode)
	}
	if lc.loginSessionID == "" {
		t.Error("desktop profile must stamp a loginSessionId onto the auth URL")
	}
	if !strings.Contains(resp.URL, "loginSessionId=") {
		t.Errorf("browser URL = %q, want the desktop session decoration", resp.URL)
	}
	loginStates.Delete("st-global-desktop")
}

// TestStartLoginFlow_MissingStateIsError guards the "restart login" error path.
func TestStartLoginFlow_MissingStateIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"state":"","authUrl":""}}`))
	}))
	defer srv.Close()
	defer setRegionBase(RegionGlobal, srv.URL)()

	if _, err := startLoginFlow(RegionGlobal); err == nil {
		t.Fatal("expected error for missing state/authUrl")
	}
}

// TestPollLogin_FollowsLoginRegion is the core routing test: a Global login
// must be polled on the Global gateway, and a CN login on the CN gateway —
// never crossed, since only the issuing region is guaranteed to serve the state.
func TestPollLogin_FollowsLoginRegion(t *testing.T) {
	var cnHits, globalHits int
	tok := func(state string) string {
		return `{"code":0,"msg":"OK","data":{"accessToken":"at-` + state + `","refreshToken":"rt","expiresIn":3600,"domain":"` + tokDomain(state) + `"}}`
	}
	newSrv := func(region Region, hits *int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*hits++
			// Origin must match the region's gateway.
			if got, want := r.Header.Get("Origin"), region.originRefererURL(); got != want {
				t.Errorf("%s poll Origin = %q, want %q", region, got, want)
			}
			switch r.URL.Path {
			case "/v2/plugin/auth/token":
				state := r.URL.Query().Get("state")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tok(state)))
			case "/v2/plugin/login/account":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"uid":"u1","enterpriseId":"e1","nickname":"Nick"}}`))
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
			}
		}))
	}
	cnSrv := newSrv(RegionCN, &cnHits)
	defer cnSrv.Close()
	glSrv := newSrv(RegionGlobal, &globalHits)
	defer glSrv.Close()
	defer setRegionBase(RegionCN, cnSrv.URL)()
	defer setRegionBase(RegionGlobal, glSrv.URL)()

	// Global login -> only the Global gateway is contacted.
	loginStates.Store("g1", loginCtxIn(t, RegionGlobal))
	body, err := handlePollLogin([]byte(`{"state":"g1"}`))
	if err != nil {
		t.Fatalf("poll global: %v", err)
	}
	env := decodePoll(t, body)
	if env.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("global poll status = %q, want success", env.Status)
	}
	sa, err := parseStored(env.Auth.StorageJSON)
	if err != nil {
		t.Fatalf("parse stored: %v", err)
	}
	if !isGlobalDomain(sa.Auth.Domain) {
		t.Errorf("global login stored domain = %q, want a Global domain", sa.Auth.Domain)
	}
	if accountRegion(sa) != "global" {
		t.Errorf("global login accountRegion = %q, want global", accountRegion(sa))
	}
	if globalHits == 0 {
		t.Error("global gateway was never contacted")
	}
	if cnHits != 0 {
		t.Errorf("CN gateway contacted %d times for a Global login, want 0", cnHits)
	}

	// CN login -> only the CN gateway is contacted.
	loginStates.Store("c1", loginCtxIn(t, RegionCN))
	cnHits, globalHits = 0, 0
	body, err = handlePollLogin([]byte(`{"state":"c1"}`))
	if err != nil {
		t.Fatalf("poll cn: %v", err)
	}
	env = decodePoll(t, body)
	sa, err = parseStored(env.Auth.StorageJSON)
	if err != nil {
		t.Fatalf("parse stored: %v", err)
	}
	if isGlobalDomain(sa.Auth.Domain) {
		t.Errorf("CN login stored domain = %q, want CN", sa.Auth.Domain)
	}
	if cnHits == 0 {
		t.Error("CN gateway was never contacted")
	}
	if globalHits != 0 {
		t.Errorf("Global gateway contacted %d times for a CN login, want 0", globalHits)
	}
}

// TestPollLogin_ZeroRegionFallsBackToCN covers loginCtx values created before
// the region field existed (or a zero value): historical CN behaviour.
func TestPollLogin_ZeroRegionFallsBackToCN(t *testing.T) {
	var cnHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cnHits++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/plugin/auth/token" {
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"accessToken":"at","expiresIn":3600,"domain":"www.codebuddy.cn"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"uid":"u","nickname":"n"}}`))
	}))
	defer srv.Close()
	defer setRegionBase(RegionCN, srv.URL)()

	// Zero region: a loginCtx with no region set (pre-Global value).
	loginStates.Store("z1", &loginCtx{client: mustLoginClient(t), expires: time.Now().Add(loginTTL)})
	body, err := handlePollLogin([]byte(`{"state":"z1"}`))
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got := decodePoll(t, body).Status; got != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, want success", got)
	}
	if cnHits == 0 {
		t.Error("CN gateway was not contacted for a zero-region login")
	}
}

// TestPollLogin_UnknownStateIsTerminal makes sure a lost state reports a
// restartable error rather than hanging the panel's poll loop.
func TestPollLogin_UnknownStateIsTerminal(t *testing.T) {
	if _, err := handlePollLogin([]byte(`{"state":"does-not-exist"}`)); err == nil {
		t.Fatal("expected error for unknown state")
	}
}

// --- helpers ---

func tokDomain(state string) string {
	if strings.HasPrefix(state, "g") {
		return "www.workbuddy.ai"
	}
	return "www.codebuddy.cn"
}

// mustLoginClient builds a login client or fails the test. newLoginClient
// returns an error so a blocked/explicit proxy config fails closed instead of
// silently using the shared transport.
func mustLoginClient(t *testing.T) *http.Client {
	t.Helper()
	client, err := newLoginClient()
	if err != nil {
		t.Fatalf("newLoginClient: %v", err)
	}
	return client
}

// loginCtxIn builds an unexpired loginCtx for the given region.
func loginCtxIn(t *testing.T, region Region) *loginCtx {
	return &loginCtx{client: mustLoginClient(t), region: region, expires: time.Now().Add(loginTTL)}
}

func decodePoll(t *testing.T, body []byte) pluginapi.AuthLoginPollResponse {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", string(body))
	}
	var out pluginapi.AuthLoginPollResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("poll response: %v", err)
	}
	return out
}
