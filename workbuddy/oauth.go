// oauth.go implements the AuthProvider login flow: browser-driven OAuth via
// CodeBuddy's login endpoints (CN and Global), login state polling, and token
// refresh. Each login flow gets an isolated cookie jar so multi-account flows
// never cross-contaminate session state.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// newLoginClient builds an isolated client with its own cookie jar so that the
// browser login for one state can never leak into another.
func newLoginClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: sharedHTTPClient().Transport,
		Jar:       jar,
	}
}

// doJSON sends method to fullURL with the given headers, parses the {code,msg,data}
// envelope, and returns the inner data payload. httpStatus is the upstream code.
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		// Redirects: Go's client follows them for GET, but a 3xx that lands
		// here (e.g. POST 307/308 not re-sent, or a new upstream gateway) would
		// otherwise surface as a misleading JSON "parse failed".
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d (location: %s)", resp.StatusCode, resp.Header.Get("Location"))
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, truncateRedacted(env.Msg, 120))
	}
	return env.Data, resp.StatusCode, nil
}

// regionFromStartRequest extracts the desired login region from the host's
// auth.login.start payload. The CPA host does not surface a region selector on
// its login card, so the plugin-level config default (default_region) is the
// effective signal there; Metadata is honoured when a host does pass it
// through. Absent any hint we keep the historical CN behaviour.
func regionFromStartRequest(raw []byte) Region {
	if len(raw) > 0 {
		var req pluginapi.AuthLoginStartRequest
		if err := json.Unmarshal(raw, &req); err == nil && req.Metadata != nil {
			if v, ok := req.Metadata["region"].(string); ok && strings.TrimSpace(v) != "" {
				return normalizeRegion(v)
			}
		}
		var probe struct {
			Region string `json:"region"`
		}
		if err := json.Unmarshal(raw, &probe); err == nil && strings.TrimSpace(probe.Region) != "" {
			return normalizeRegion(probe.Region)
		}
	}
	return defaultLoginRegion()
}

// startLoginFlow issues a login state on the given region's gateway, registers
// it for polling, and returns the browser-facing login URL. Shared by the host
// AuthProvider RPC (handleStartLogin) and the panel-driven management route
// (/login/start), which is how a user picks Global at runtime.
func startLoginFlow(region Region) (pluginapi.AuthLoginStartResponse, error) {
	client := newLoginClient()
	headers := func(r *http.Request) { regionHeaders(r, region) }
	data, _, err := doJSON(client, http.MethodPost, region.authStateURL(), headers, bytes.NewReader([]byte("{}")))
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("%s auth state failed: %w", region, err)
	}
	var st authStateData
	_ = json.Unmarshal(data, &st)
	if st.State == "" || st.AuthURL == "" {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("auth state: missing state or authUrl — please restart the login flow")
	}
	expires := time.Now().Add(loginTTL)
	loginStates.Store(st.State, &loginCtx{client: client, region: region, expires: expires})
	return pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       st.AuthURL,
		State:     st.State,
		ExpiresAt: expires.UTC(),
		Metadata: map[string]any{
			"logo":   pluginLogoURL,
			"region": string(region),
		},
	}, nil
}

func handleStartLogin(raw []byte) ([]byte, error) {
	resp, err := startLoginFlow(regionFromStartRequest(raw))
	if err != nil {
		return nil, err
	}
	return okEnvelope(resp)
}

func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return nil, fmt.Errorf("poll: empty state")
	}
	v, ok := loginStates.Load(state)
	if !ok {
		return nil, fmt.Errorf("poll: unknown state (restart login) — the login session was lost; please re-initiate login")
	}
	lc := v.(*loginCtx)
	if time.Now().After(lc.expires) {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: login expired (5 min timeout) — please re-initiate login and complete within 5 minutes")
	}
	// Poll the gateway that issued this state. Live testing showed both
	// backends accept each other's state, but following the issuing region is
	// the documented-protocol behaviour and survives upstream isolation.
	region := lc.region
	if region == "" {
		region = RegionCN // pre-Global loginCtx (or zero value) → historical default
	}

	// Single-shot poll per RPC: the host drives the polling cadence.
	// auth/token is the authoritative login-status endpoint: the application
	// layer returns a non-zero code ("login ing") while pending, and code 0
	// with the token bundle once complete. login/account sits behind the
	// openresty gateway and is rejected (401) until login finishes, so probe
	// token first and only fetch account once we hold a bearer.
	//
	// Headers must follow the region: doJSON's fallback is CN, and the
	// workbuddy.ai gateway rejects a CN Origin.
	tokHeaders := func(r *http.Request) { regionHeaders(r, region) }
	tokRaw, status, errTok := doJSON(lc.client, http.MethodGet, region.authTokenURL(state), tokHeaders, nil)
	if errTok != nil {
		// Transport-level failures and 5xx are real errors, not "still waiting":
		// surface them so the user sees a failure instead of polling until TTL.
		if status == 0 || status >= 500 {
			loginStates.Delete(state)
			return nil, fmt.Errorf("poll: token endpoint error: %w — upstream may be temporarily unavailable; retry in a few minutes", errTok)
		}
		// 4xx / business-code responses mean the login is still pending.
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for login",
		})
	}
	var tok tokenData
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for login",
		})
	}

	var acct accountData
	acctHeaders := func(r *http.Request) {
		regionHeaders(r, region)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := doJSON(lc.client, http.MethodGet, region.loginAcctURL(state), acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}

	sa := &storedAuth{
		Auth: storedTokens{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
			Domain:       domainForRegion(tok.Domain, region),
		},
		Account: storedAccount{
			UID:          acct.UID,
			EnterpriseID: acct.EnterpriseID,
			Nickname:     acct.Nickname,
		},
	}
	loginStates.Delete(state)
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   toAuthData(sa),
	})
}

// domainForRegion guarantees the stored domain reflects the gateway the user
// actually logged into. Every downstream region decision (billing base, chat
// base, Origin/Referer, check-in skip, trial eligibility, exhaust policy) keys
// off storedTokens.Domain via isGlobalDomain, so a Global login must never
// persist an empty or CN domain. Upstream normally returns the right value; we
// only fill in when it is missing, and never override a conflicting one (that
// would mask an upstream change we would rather see).
func domainForRegion(upstreamDomain string, region Region) string {
	if d := strings.TrimSpace(upstreamDomain); d != "" {
		return d
	}
	if region == RegionGlobal {
		return "www.workbuddy.ai"
	}
	return "www.codebuddy.cn"
}

func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	// Route via host.http.do so request-log captures the refresh call (H2
	// compliance: was doJSON(sharedHTTPClient()) — bypassed host transport
	// policy + logging for the X-Refresh-Token endpoint).
	data, raw2, status, err := refreshCall(sa)
	if err != nil {
		if status >= 400 {
			return nil, fmt.Errorf("refresh rejected (HTTP %d)", status)
		}
		return nil, fmt.Errorf("refresh: %w", err)
	}
	_ = raw2
	var tok tokenData
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("refresh_failed: no accessToken in response — the refresh token may be expired; re-login required")
	}
	sa.Auth.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		sa.Auth.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		sa.Auth.Domain = tok.Domain
	}
	sa.Auth.ExpiresAt = preserveExpiry(
		time.Now().Add(time.Duration(tok.ExpiresIn)*time.Second).Unix(),
		sa.Auth.ExpiresAt,
	)
	// No explicit host.auth.save here: the host's auth Manager persists the
	// refreshed credential itself after Refresh returns (conductor.go
	// refreshAuth → m.Update → persist). Writing from the plugin too would
	// double-write the file.
	return okEnvelope(pluginapi.AuthRefreshResponse{Auth: toAuthDataForRefresh(sa)})
}

// preserveExpiry reuses the previous token's expiresAt when the refresh
// response omits expiresIn (some CodeBuddy deployments return only the token
// pair). Zero would tell the host the credential is permanently expired and
// trigger a refresh storm on every request.
func preserveExpiry(newExpiry, oldExpiry int64) int64 {
	if newExpiry > 0 {
		return newExpiry
	}
	return oldExpiry
}

// toAuthDataForRefresh returns AuthData with FileName left EMPTY so the CPA
// host backfills the original auth.FileName (auth_provider.go:371).
//
// CPA uses FileName (relative to auth dir) as auth ID. If we set it to
// "workbuddy-<uid>.json" while the original file was "workbuddy.json"
// (legacy single-account name), the host treats it as a rename, writes a
// NEW file, and the old one stays → duplicate auth records.
//
// Returning empty FileName = "keep what you had" → no rename, no dup.
func toAuthDataForRefresh(sa *storedAuth) pluginapi.AuthData {
	ad := toAuthDataOpts(sa, nil, false)
	ad.FileName = "" // let host backfill original
	ad.ID = ""       // let host compute from path (prevents ID mismatch dupes)
	return ad
}
