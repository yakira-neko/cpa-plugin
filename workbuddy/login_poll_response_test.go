package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestLoginPollResponseCarriesHostSaveName pins the /login/poll response's
// name/path fields, which are taken from the host's auth.save REPLY.
//
// The existing e2e test (login_panel_test.go:99) only asserts the hook's
// captured REQUEST name; it never asserts what the panel actually receives
// back. So a failure to decode the save response would go unnoticed — and the
// panel's success toast and the legacy-file cleanup both depend on it
// (credits_handler.go:188-193 uses saveResp.Name to decide whether to remove a
// stale legacy workbuddy.json).
func TestLoginPollResponseCarriesHostSaveName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"state":"s1","authUrl":"https://www.workbuddy.ai/login?state=s1"}}`))
		case "/v2/plugin/auth/token":
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"accessToken":"at","refreshToken":"rt","expiresIn":3600,"domain":"www.workbuddy.ai"}}`))
		case "/v2/plugin/login/account":
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"uid":"u1","nickname":"N"}}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()
	defer setRegionBase(RegionGlobal, srv.URL)()

	var hookSawName string
	defer setHostCallHook(func(method string, request []byte) ([]byte, error) {
		var req pluginapi.HostAuthSaveRequest
		_ = json.Unmarshal(request, &req)
		hookSawName = req.Name
		res, _ := json.Marshal(pluginapi.HostAuthSaveResponse{Name: req.Name, Path: "/auth/" + req.Name})
		// NOTE: must be json.RawMessage, not []byte. json.Marshal([]byte)
		// base64-encodes the bytes, so okEnvelope would emit result as a JSON
		// STRING and the plugin's json.Unmarshal(env.Result, &saveResp) would
		// silently leave saveResp zero — hiding exactly the fields under test.
		// Every existing stub in this package passes []byte
		// (login_panel_test.go:56,151), so saveResp.Name has never actually
		// been populated in any test.
		return okEnvelope(json.RawMessage(res))
	})()

	startBody, _ := json.Marshal(map[string]string{"region": "global"})
	start := handleLoginStart(pluginapi.ManagementRequest{Body: startBody})
	state, _ := start["state"].(string)

	pollBody, _ := json.Marshal(map[string]string{"state": state})
	out := handleLoginPoll(pluginapi.ManagementRequest{Body: pollBody})

	t.Logf("hook saw request name : %q", hookSawName)
	t.Logf("poll response         : %#v", out)

	if out["status"] != "success" {
		t.Fatalf("status = %v (%v)", out["status"], out["error"])
	}
	if hookSawName != "workbuddy-u1.json" {
		t.Fatalf("hook request name = %q, want workbuddy-u1.json", hookSawName)
	}
	// These are the assertions the existing suite is missing.
	if got := out["name"]; got != hookSawName {
		t.Errorf("response name = %v, want %q (host save reply was not decoded into the response)", got, hookSawName)
	}
	if got := out["path"]; got != "/auth/"+hookSawName {
		t.Errorf("response path = %v, want %q", got, "/auth/"+hookSawName)
	}
}
