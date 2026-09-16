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

// upstreamError carries the upstream envelope detail that the old code threw
// away. Every caller that used to see only "http_error: upstream 500" or
// "code=11217 msg=..." now gets the status, business code, upstream message and
// request id separately, so a failure can be reported precisely instead of
// being flattened into "still waiting".
type upstreamError struct {
	HTTPStatus int
	Code       int
	Msg        string
	RequestID  string
	// Shape is the content-free structure of the response body (see jsonShape).
	Shape string
	// Body is the redacted, truncated raw body — used only for diagnostics.
	Body string
	// kind is "transport", "http", "redirect", "parse" or "business".
	kind string
}

func (e *upstreamError) Error() string {
	switch e.kind {
	case "transport":
		return fmt.Sprintf("transport error: %s", e.Msg)
	case "http":
		return fmt.Sprintf("http_error: upstream %d", e.HTTPStatus)
	case "redirect":
		return fmt.Sprintf("http_error: upstream redirect %d", e.HTTPStatus)
	case "parse":
		return fmt.Sprintf("parse failed: %s", e.Msg)
	default:
		return fmt.Sprintf("code=%d msg=%s", e.Code, e.Msg)
	}
}

// doJSON sends method to fullURL with the given headers, parses the {code,msg,data}
// envelope, and returns the inner data payload. httpStatus is the upstream code.
//
// On failure the returned error is an *upstreamError carrying the upstream
// status / business code / message / requestId so callers can surface real
// diagnostics. Callers that only need the legacy string can keep using
// err.Error().
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
		return nil, 0, &upstreamError{kind: "transport", Msg: truncateRedacted(err.Error(), 200)}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, &upstreamError{
			kind:       "http",
			HTTPStatus: resp.StatusCode,
			Shape:      jsonShape(raw),
			Body:       truncateRedacted(string(raw), 400),
		}
	}
	if resp.StatusCode >= 300 {
		// Redirects: Go's client follows them for GET, but a 3xx that lands
		// here (e.g. POST 307/308 not re-sent, or a new upstream gateway) would
		// otherwise surface as a misleading JSON "parse failed".
		return nil, resp.StatusCode, &upstreamError{
			kind:       "redirect",
			HTTPStatus: resp.StatusCode,
			Msg:        truncateRedacted(resp.Header.Get("Location"), 200),
			Shape:      jsonShape(raw),
		}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, &upstreamError{
			kind:       "parse",
			HTTPStatus: resp.StatusCode,
			Msg:        truncateRedacted(err.Error(), 160),
			Shape:      jsonShape(raw),
			Body:       truncateRedacted(string(raw), 400),
		}
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, &upstreamError{
			kind:       "business",
			HTTPStatus: resp.StatusCode,
			Code:       env.Code,
			Msg:        truncateRedacted(env.Msg, 200),
			RequestID:  env.RequestID,
			Shape:      jsonShape(raw),
			Body:       truncateRedacted(string(raw), 400),
		}
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

// pendingBusinessCodes lists upstream business codes that genuinely mean "the
// browser has not finished logging in yet". Only these are reported as pending.
//
// 11217 "login ing..." is the only one observed live (verified 2026-09-15 on
// both www.workbuddy.ai and copilot.tencent.com with an unconsumed state).
//
// Any OTHER business code is reported as a hard error to the user — it is a
// terminal upstream refusal, and calling it "waiting" is what previously hid
// real failures until the 5-minute TTL expired. Classification is centralised
// here so the panel, the host card and the tests all agree.
var pendingBusinessCodes = map[int]bool{
	11217: true, // "login ing..." — observed pending on CN + Global
}

// loginUnrecognisedCodeFastFail is how many times an unrecognised business code
// must repeat before it is declared terminal. The first occurrence is tolerated
// as pending-like (an unknown pending variant must not break a working login),
// but a code that keeps coming back is a hard refusal, and the user should be
// told now rather than after the 5-minute TTL.
const loginUnrecognisedCodeFastFail = 3

// loginOutcome classifies one auth/token poll result.
type loginOutcome int

const (
	outcomePending loginOutcome = iota
	outcomeSuccess
	outcomeTerminal
)

// classifyTokenPoll decides what a poll result means, and always explains
// itself in a form worth showing the user.
//
// Rule: pending ONLY for a recognised pending code. Everything else is either a
// decoded credential or a terminal error carrying the upstream detail.
func classifyTokenPoll(raw json.RawMessage, httpStatus int, baseErr error) (loginOutcome, string, *upstreamError) {
	var ue *upstreamError
	if baseErr != nil {
		if e, ok := baseErr.(*upstreamError); ok {
			ue = e
		} else {
			ue = &upstreamError{kind: "other", Msg: truncateRedacted(baseErr.Error(), 200)}
		}
	}

	// Transport failure or 5xx: a real error, not "still waiting".
	if ue != nil && (ue.kind == "transport" || ue.HTTPStatus == 0 || ue.HTTPStatus >= 500) {
		return outcomeTerminal, fmt.Sprintf("上游不可用（%s）", ue.Error()), ue
	}

	// A recognised pending business code: genuinely waiting.
	if ue != nil && ue.kind == "business" && pendingBusinessCodes[ue.Code] {
		return outcomePending, "waiting for login", ue
	}

	// Any other business code, 4xx, redirect or parse failure: terminal.
	if ue != nil {
		detail := ue.Error()
		if ue.RequestID != "" {
			detail += " [requestId=" + ue.RequestID + "]"
		}
		return outcomeTerminal, "上游拒绝本次轮询：" + detail, ue
	}

	// code 0: the payload must contain a usable credential. If it does not,
	// that is a terminal error — NOT "waiting". Reporting pending here was the
	// second way a completed login could look like an endless wait.
	var tok tokenData
	if err := json.Unmarshal(raw, &tok); err != nil {
		return outcomeTerminal,
			"上游返回成功但响应无法解析：" + truncateRedacted(err.Error(), 160),
			&upstreamError{kind: "parse", HTTPStatus: httpStatus, Msg: err.Error(), Shape: jsonShape(raw)}
	}
	if strings.TrimSpace(tok.AccessToken) == "" {
		return outcomeTerminal,
			"上游返回成功但响应中没有 accessToken（响应结构：" + jsonShape(raw) + "）",
			&upstreamError{kind: "parse", HTTPStatus: httpStatus, Msg: "missing accessToken", Shape: jsonShape(raw)}
	}
	return outcomeSuccess, "", nil
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
		// The upstream state may already have been consumed by an earlier
		// successful poll whose credential has not been persisted yet (a failed
		// host.auth.save on the panel path). Replay the cached credential rather
		// than reporting a terminal failure — the login itself did succeed, and
		// discarding it here is what made a save error unrecoverable.
		if sa, hit := loginResultTake(state); hit {
			loginDiagAdd(state, loginDiagEntry{Step: "poll", Outcome: "replayed_success",
				Detail: "credential recovered from the completed login (state already consumed)"})
			return okEnvelope(pluginapi.AuthLoginPollResponse{
				Status: pluginapi.AuthLoginStatusSuccess,
				Auth:   toAuthData(sa),
			})
		}
		loginDiagAdd(state, loginDiagEntry{Step: "poll", Outcome: "unknown_state",
			Detail: "no such login state in this process (restart, TTL expiry, or a second replica)"})
		return nil, fmt.Errorf("poll: unknown state (restart login) — the login session was lost; please re-initiate login")
	}
	lc := v.(*loginCtx)
	if time.Now().After(lc.expires) {
		loginStates.Delete(state)
		loginDiagAdd(state, loginDiagEntry{Step: "poll", Outcome: "expired", Detail: "5 minute login TTL elapsed"})
		// Diagnostic record is intentionally kept: the expiry is exactly when
		// the user needs to see what happened. Bounded by the diag ring buffer
		// and pruned by the janitor alongside the login state.
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

	outcome, msg, ue := classifyTokenPoll(tokRaw, status, errTok)
	entry := loginDiagEntry{Region: string(region), Step: "auth/token", HTTPStatus: status}
	if ue != nil {
		entry.Code = ue.Code
		entry.Msg = ue.Msg
		entry.RequestID = ue.RequestID
		entry.Shape = ue.Shape
		if entry.Detail == "" {
			entry.Detail = ue.Error()
		}
	}

	switch outcome {
	case outcomeTerminal:
		// Fast-fail: an unrecognised non-zero code that REPEATS is a hard
		// refusal, not a slow login. A single occurrence is tolerated as
		// pending-like so an unknown pending variant cannot break a login that
		// would otherwise succeed, but a code that keeps coming back is
		// terminal. This is what turns a 5-minute silent hang into a message.
		if ue != nil && ue.kind == "business" && !pendingBusinessCodes[ue.Code] {
			repeats := loginDiagRepeatCount(state, "auth/token", ue.Code)
			if repeats+1 < loginUnrecognisedCodeFastFail {
				entry.Outcome = "unrecognised_code_pending"
				entry.Detail = fmt.Sprintf("unrecognised code=%d (seen %d time(s)); treating as pending until %d repeats",
					ue.Code, repeats+1, loginUnrecognisedCodeFastFail)
				loginDiagAdd(state, entry)
				return okEnvelope(pluginapi.AuthLoginPollResponse{
					Status:  pluginapi.AuthLoginStatusPending,
					Message: fmt.Sprintf("waiting for login (upstream code=%d)", ue.Code),
				})
			}
			msg = fmt.Sprintf("上游连续 %d 次返回未识别的业务码 code=%d，已判定为失败（原信息：%s）",
				repeats+1, ue.Code, ue.Msg)
			if ue.RequestID != "" {
				msg += " [requestId=" + ue.RequestID + "]"
			}
		}
		entry.Outcome = "terminal"
		loginDiagAdd(state, entry)
		// Single-use state: the credential (if any) lived only in this
		// response, and the poll cannot progress past a terminal answer.
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: %s", msg)

	case outcomePending:
		entry.Outcome = "pending"
		loginDiagAdd(state, entry)
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: msg,
		})
	}

	// outcomeSuccess — tokRaw holds a usable credential.
	var tok tokenData
	if err := json.Unmarshal(tokRaw, &tok); err != nil {
		entry.Outcome = "terminal"
		entry.Detail = "success payload did not decode after passing classification (internal inconsistency)"
		loginDiagAdd(state, entry)
		return nil, fmt.Errorf("poll: credential decode failed: %w", err)
	}
	entry.Outcome = "success"
	entry.Shape = jsonShape(tokRaw)
	entry.Detail = fmt.Sprintf("accessToken len=%d refreshToken len=%d expiresIn=%d domain=%q",
		len(tok.AccessToken), len(tok.RefreshToken), tok.ExpiresIn, tok.Domain)
	loginDiagAdd(state, entry)

	var acct accountData
	acctHeaders := func(r *http.Request) {
		regionHeaders(r, region)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	acctRaw, acctStatus, errAcct := doJSON(lc.client, http.MethodGet, region.loginAcctURL(state), acctHeaders, nil)
	acctEntry := loginDiagEntry{Region: string(region), Step: "login/account", HTTPStatus: acctStatus}
	if errAcct != nil {
		var aue *upstreamError
		if e, isUE := errAcct.(*upstreamError); isUE {
			aue = e
			acctEntry.Code = e.Code
			acctEntry.Msg = e.Msg
			acctEntry.RequestID = e.RequestID
			acctEntry.Shape = e.Shape
		}
		// A failed account lookup is NOT fatal: the login itself succeeded and
		// the token is valid. But it MUST be visible — a silent failure here
		// yields an empty UID, which (via authFileNameFor) degrades the saved
		// file name to the bare "workbuddy.json" that hostAuthList() filters
		// out, making a real credential invisible to the panel.
		acctEntry.Outcome = "failed"
		acctEntry.Detail = "account lookup failed — credential will be saved without uid/nickname"
		_ = aue
		loginDiagAdd(state, acctEntry)
	} else {
		_ = json.Unmarshal(acctRaw, &acct)
		acctEntry.Outcome = "ok"
		acctEntry.Detail = fmt.Sprintf("uid_present=%v nickname_present=%v", acct.UID != "", acct.Nickname != "")
		loginDiagAdd(state, acctEntry)
	}

	// A missing expiresIn would stamp expiresAt=now (a credential that is born
	// expired). preserveExpiry is used on the refresh path for the same reason;
	// the login path must not silently produce a dead credential.
	expiresAt := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	if tok.ExpiresIn <= 0 {
		loginDiagAdd(state, loginDiagEntry{Region: string(region), Step: "login",
			Outcome: "warn_no_expires_in",
			Detail:  "upstream omitted expiresIn — credential would be stamped as already expired"})
	}

	sa := &storedAuth{
		Auth: storedTokens{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			ExpiresAt:    expiresAt,
			Domain:       domainForRegion(tok.Domain, region),
		},
		Account: storedAccount{
			UID:          acct.UID,
			EnterpriseID: acct.EnterpriseID,
			Nickname:     acct.Nickname,
		},
	}
	// The upstream state is single-use, so consume it now — but keep the parsed
	// credential in a bounded cache keyed by the same state. The host-driven
	// path persists this credential itself (it never calls handleLoginPoll), and
	// the panel path may need to retry a failed host.auth.save. Without the
	// cache, a failed save would destroy a completed login irrecoverably
	// because the token existed only in this one upstream response.
	loginStates.Delete(state)
	loginResultStore(state, sa)
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
