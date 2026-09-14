package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestPanelLoginEndToEnd_Global drives the exact routes the panel calls
// (/login/start then /login/poll) against a fake Global gateway, and asserts
// the whole chain: region routing, credential shaping, and the response the
// panel renders.
func TestPanelLoginEndToEnd_Global(t *testing.T) {
	var sawState bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Both endpoints must carry the Global Origin.
		if got := r.Header.Get("Origin"); got != originRefererGlobal {
			t.Errorf("Origin = %q, want %q", got, originRefererGlobal)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"state":"panel-g","authUrl":"https://www.workbuddy.ai/login?platform=CLI&state=panel-g"}}`))
		case "/v2/plugin/auth/token":
			sawState = true
			if got := r.URL.Query().Get("state"); got != "panel-g" {
				t.Errorf("poll state = %q, want panel-g", got)
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"accessToken":"at-g","refreshToken":"rt-g","expiresIn":7200,"domain":"www.workbuddy.ai"}}`))
		case "/v2/plugin/login/account":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer at-g") {
				t.Errorf("account call missing bearer, got %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"uid":"intl-uid","enterpriseId":"ent-9","nickname":"IntlUser"}}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()
	defer setRegionBase(RegionGlobal, srv.URL)()

	var savedName string
	var savedJSON []byte
	defer setHostCallHook(func(method string, request []byte) ([]byte, error) {
		if method != pluginabi.MethodHostAuthSave {
			t.Fatalf("unexpected host method %q", method)
		}
		var req pluginapi.HostAuthSaveRequest
		_ = json.Unmarshal(request, &req)
		savedName, savedJSON = req.Name, req.JSON
		res, _ := json.Marshal(pluginapi.HostAuthSaveResponse{Name: req.Name, Path: "/auth/" + req.Name})
		return okEnvelope(res)
	})()

	// 1) /login/start {region:"global"}
	startBody, _ := json.Marshal(map[string]string{"region": "global"})
	out := handleLoginStart(pluginapi.ManagementRequest{Body: startBody})
	if out["success"] != true {
		t.Fatalf("login/start failed: %v", out["error"])
	}
	if got := out["region"]; got != "global" {
		t.Errorf("region = %v, want global", got)
	}
	url, _ := out["url"].(string)
	if !strings.Contains(url, "www.workbuddy.ai/login") {
		t.Errorf("url = %q, want workbuddy.ai login", url)
	}
	state, _ := out["state"].(string)
	if state != "panel-g" {
		t.Fatalf("state = %q, want panel-g", state)
	}

	// 2) /login/poll {state}
	pollBody, _ := json.Marshal(map[string]string{"state": state})
	res := handleLoginPoll(pluginapi.ManagementRequest{Body: pollBody})
	if res["status"] != "success" {
		t.Fatalf("login/poll status = %v (err=%v)", res["status"], res["error"])
	}
	if res["region"] != "global" {
		t.Errorf("polled region = %v, want global", res["region"])
	}
	if res["nickname"] != "IntlUser" || res["uid"] != "intl-uid" {
		t.Errorf("account identity = %v / %v", res["nickname"], res["uid"])
	}
	if res["domain"] != "www.workbuddy.ai" {
		t.Errorf("domain = %v, want www.workbuddy.ai", res["domain"])
	}
	if !sawState {
		t.Error("token endpoint was never polled")
	}

	// The credential must be persisted under the uid-scoped file name, with the
	// Global domain preserved — that domain is what routes every later request.
	if savedName != "workbuddy-intl-uid.json" {
		t.Errorf("saved name = %q, want workbuddy-intl-uid.json", savedName)
	}
	sa, err := parseStored(savedJSON)
	if err != nil {
		t.Fatalf("saved credential unparseable: %v", err)
	}
	if !isGlobalDomain(sa.Auth.Domain) {
		t.Errorf("saved domain = %q, want Global", sa.Auth.Domain)
	}
	if accountRegion(sa) != "global" {
		t.Errorf("saved account region = %q, want global", accountRegion(sa))
	}
	if sa.Auth.AccessToken != "at-g" || sa.Auth.RefreshToken != "rt-g" {
		t.Errorf("saved tokens = %q / %q", sa.Auth.AccessToken, sa.Auth.RefreshToken)
	}
	if billingBaseFor(sa) != billingBaseGlobal {
		t.Errorf("billing base for saved account = %q, want %q", billingBaseFor(sa), billingBaseGlobal)
	}

	// The state is single-use: a second poll must report a terminal status the
	// panel can act on (not hang).
	again := handleLoginPoll(pluginapi.ManagementRequest{Body: pollBody})
	if again["status"] != "expired" {
		t.Errorf("second poll status = %v, want expired", again["status"])
	}
}

// TestPanelLoginEndToEnd_PendingThenSuccess covers the panel poll loop: a
// pending response must be non-terminal so the UI keeps polling.
func TestPanelLoginEndToEnd_PendingThenSuccess(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"state":"p1","authUrl":"https://www.workbuddy.ai/login?state=p1"}}`))
		case "/v2/plugin/auth/token":
			attempts++
			if attempts < 2 {
				// "login ing" — business code, still pending.
				_, _ = w.Write([]byte(`{"code":11217,"msg":"11217:login ing...","data":null}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"accessToken":"at","expiresIn":3600,"domain":"www.workbuddy.ai"}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"uid":"u","nickname":"n"}}`))
		}
	}))
	defer srv.Close()
	defer setRegionBase(RegionGlobal, srv.URL)()
	defer setHostCallHook(func(method string, request []byte) ([]byte, error) {
		res, _ := json.Marshal(pluginapi.HostAuthSaveResponse{Name: "workbuddy-u.json"})
		return okEnvelope(res)
	})()

	start := handleLoginStart(pluginapi.ManagementRequest{
		Body: []byte(`{"region":"global"}`),
	})
	state, _ := start["state"].(string)
	pb, _ := json.Marshal(map[string]string{"state": state})

	first := handleLoginPoll(pluginapi.ManagementRequest{Body: pb})
	if first["status"] != "pending" {
		t.Fatalf("first poll status = %v, want pending", first["status"])
	}
	second := handleLoginPoll(pluginapi.ManagementRequest{Body: pb})
	if second["status"] != "success" {
		t.Fatalf("second poll status = %v, want success (err=%v)", second["status"], second["error"])
	}
}

// TestPanelLoginStart_DefaultsToConfigRegion pins that omitting a region in the
// panel request honours default_region rather than silently forcing CN.
func TestPanelLoginStart_DefaultsToConfigRegion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"state":"d1","authUrl":"https://x/login"}}`))
	}))
	defer srv.Close()
	defer setRegionBase(RegionGlobal, srv.URL)()
	defer setRegionBase(RegionCN, srv.URL)()
	defer setDefaultLoginRegion("global")()

	out := handleLoginStart(pluginapi.ManagementRequest{Body: []byte(`{}`)})
	if out["region"] != "global" {
		t.Errorf("region = %v, want global (from default_region)", out["region"])
	}
}

// TestLoginConfig_RoundTrip covers the config endpoint the panel uses.
func TestLoginConfig_RoundTrip(t *testing.T) {
	defer setDefaultLoginRegion("")()

	got := handleLoginConfig(pluginapi.ManagementRequest{Body: []byte(`{}`)})
	if got["default_region"] != "cn" {
		t.Errorf("default = %v, want cn", got["default_region"])
	}
	set := handleLoginConfig(pluginapi.ManagementRequest{Body: []byte(`{"default_region":"intl"}`)})
	if set["default_region"] != "global" {
		t.Errorf("after set = %v, want global", set["default_region"])
	}
	if got := defaultLoginRegion(); got != RegionGlobal {
		t.Errorf("defaultLoginRegion() = %q, want global", got)
	}
}

// TestRegionHeaders_PerRegion pins the header set, since the Global gateway
// rejects a CN Origin.
func TestRegionHeaders_PerRegion(t *testing.T) {
	for _, region := range []Region{RegionCN, RegionGlobal} {
		req, _ := http.NewRequest(http.MethodGet, region.baseURL()+"/x", nil)
		regionHeaders(req, region)
		want := region.originRefererURL()
		if got := req.Header.Get("Origin"); got != want {
			t.Errorf("%s Origin = %q, want %q", region, got, want)
		}
		if got := req.Header.Get("Referer"); got != want+"/" {
			t.Errorf("%s Referer = %q, want %q", region, got, want+"/")
		}
		if got := req.Header.Get("User-Agent"); got != clientUA {
			t.Errorf("%s User-Agent = %q, want %q", region, got, clientUA)
		}
	}
}
