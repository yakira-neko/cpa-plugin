// credits_handler.go implements the management API endpoints that mutate or
// read account state: import credential, toggle check-in, claim trial, select
// active auth, and query credits for one account or all.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleImportAuth accepts nested or flat credential JSON and persists via host.auth.save.
func handleImportAuth(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		JSON json.RawMessage `json:"json"`
		Raw  string          `json:"raw"`
	}
	_ = json.Unmarshal(req.Body, &body)
	raw := []byte(strings.TrimSpace(body.Raw))
	if len(body.JSON) > 0 {
		raw = body.JSON
	}
	if len(raw) == 0 {
		return map[string]any{"success": false, "error": "missing json/raw credential payload"}
	}
	sa, err := parseStored(raw)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	// Persist nested storage + top-level type/note/logo/disabled for Auth page.
	fileJSON, err := buildAuthFileJSON(sa, false, displayNote(sa, nil, false), nil)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	auth := toAuthData(sa)
	saveReq := pluginapi.HostAuthSaveRequest{
		Name: auth.FileName,
		JSON: fileJSON,
	}
	saveBody, _ := json.Marshal(saveReq)
	rawResp, err := hostCall(pluginabi.MethodHostAuthSave, saveBody)
	if err != nil {
		return map[string]any{"success": false, "error": "host.auth.save: " + err.Error()}
	}
	var env envelope
	if err := json.Unmarshal(rawResp, &env); err != nil || !env.OK {
		msg := "host.auth.save failed"
		if env.Error != nil && env.Error.Message != "" {
			msg = env.Error.Message
		}
		return map[string]any{"success": false, "error": msg}
	}
	var saveResp pluginapi.HostAuthSaveResponse
	_ = json.Unmarshal(env.Result, &saveResp)
	// Remove legacy workbuddy.json if it exists and differs from the saved name.
	if saveResp.Name != "" && !strings.EqualFold(saveResp.Name, authFileName) {
		legacyPath := strings.TrimSpace(saveResp.Path)
		// Best-effort: if auth dir is known via saveResp.Path parent, try removing sibling workbuddy.json.
		if legacyPath != "" {
			dir := filepath.Dir(legacyPath)
			legacyFile := filepath.Join(dir, authFileName)
			legacyRaw, readErr := os.ReadFile(legacyFile)
			if readErr == nil && shouldDeleteLegacyForUID(legacyRaw, sa.Account.UID) {
				_ = deleteAuthFileInDir(legacyFile, dir)
			}
		}
	}
	return map[string]any{
		"success":  true,
		"name":     saveResp.Name,
		"path":     saveResp.Path,
		"uid":      sa.Account.UID,
		"nickname": sa.Account.Nickname,
		"file":     auth.FileName,
	}
}

// handleLoginStart begins an OAuth login flow for an explicitly chosen region
// and returns the browser login URL plus the state to poll with.
//
// This exists because CPA's own "add auth" card calls auth.login.start without
// a region selector, so a user has no way to reach the Global login page from
// the host UI. The panel owns region choice at click time instead.
//
// body: {"region":"cn"|"global"}
func handleLoginStart(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Region string `json:"region"`
	}
	_ = json.Unmarshal(req.Body, &body)
	region := RegionCN
	if strings.TrimSpace(body.Region) != "" {
		region = normalizeRegion(body.Region)
	} else {
		region = defaultLoginRegion()
	}
	resp, err := startLoginFlow(region)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error(), "region": string(region)}
	}
	return map[string]any{
		"success":     true,
		"region":      string(region),
		"url":         resp.URL,
		"state":       resp.State,
		"expires_at":  resp.ExpiresAt.Format(time.RFC3339),
		"ttl_seconds": int(loginTTL.Seconds()),
	}
}

// handleLoginPoll performs one poll of a panel-started login flow. On success it
// persists the credential through host.auth.save (same path as credential
// import) and returns the account identity so the panel can select it.
//
// body: {"state":"..."}
func handleLoginPoll(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(req.Body, &body)
	state := strings.TrimSpace(body.State)
	if state == "" {
		return map[string]any{"status": "error", "error": "state is required"}
	}

	// Reuse the AuthProvider poll implementation so the panel and the host
	// share exactly one login code path (and one set of region rules).
	pollReq, _ := json.Marshal(pluginapi.AuthLoginPollRequest{Provider: providerName, State: state})
	rawResp, err := handlePollLogin(pollReq)
	if err != nil {
		// A lost/expired state is terminal for the panel's poll loop; anything
		// else may be transient upstream trouble the user can retry.
		msg := err.Error()
		status := "error"
		if strings.Contains(msg, "unknown state") || strings.Contains(msg, "expired") {
			status = "expired"
		}
		out := map[string]any{"status": status, "error": msg}
		// Surface WHY: the upstream status/code/message and the redacted step
		// log, so the panel can explain the failure instead of just saying
		// "failed". Shape-only payloads keep this safe to display.
		out["diagnostics"] = loginDiagEntries(state)
		if last := lastDiagEntry(state); last != nil {
			if last.Code != 0 {
				out["upstream_code"] = last.Code
			}
			if last.HTTPStatus != 0 {
				out["upstream_http"] = last.HTTPStatus
			}
			if last.RequestID != "" {
				out["request_id"] = last.RequestID
			}
		}
		return out
	}
	var env envelope
	if err := json.Unmarshal(rawResp, &env); err != nil || !env.OK {
		return map[string]any{"status": "error", "error": "poll: bad envelope"}
	}
	var poll pluginapi.AuthLoginPollResponse
	if err := json.Unmarshal(env.Result, &poll); err != nil {
		return map[string]any{"status": "error", "error": "poll: " + err.Error()}
	}
	if poll.Status != pluginapi.AuthLoginStatusSuccess {
		return map[string]any{
			"status":      string(poll.Status),
			"message":     poll.Message,
			"diagnostics": loginDiagSummary(state, 8),
		}
	}

	sa, err := parseStored(poll.Auth.StorageJSON)
	if err != nil {
		return map[string]any{"status": "error", "error": "parse credential: " + err.Error(),
			"diagnostics": loginDiagEntries(state)}
	}

	// Persist exactly like credential import: nested storage + top-level
	// type/note/logo/disabled, named workbuddy-<uid>.json.
	fileJSON, err := buildAuthFileJSON(sa, false, displayNote(sa, nil, false), nil)
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error(),
			"diagnostics": loginDiagEntries(state)}
	}
	auth := toAuthData(sa)
	saveReq := pluginapi.HostAuthSaveRequest{Name: auth.FileName, JSON: fileJSON}
	saveBody, _ := json.Marshal(saveReq)
	rawSave, err := hostCall(pluginabi.MethodHostAuthSave, saveBody)
	if err != nil {
		// The credential is still live in this response but the state has NOT
		// been consumed, so the user can retry rather than restarting the whole
		// browser login.
		loginDiagAdd(state, loginDiagEntry{Step: "host.auth.save", Outcome: "failed",
			Detail: truncateRedacted(err.Error(), 200)})
		return map[string]any{"status": "error", "error": "host.auth.save: " + err.Error(),
			"retryable": true, "diagnostics": loginDiagEntries(state)}
	}
	var saveEnv envelope
	if err := json.Unmarshal(rawSave, &saveEnv); err != nil || !saveEnv.OK {
		msg := "host.auth.save failed"
		if saveEnv.Error != nil && saveEnv.Error.Message != "" {
			msg = saveEnv.Error.Message
		}
		loginDiagAdd(state, loginDiagEntry{Step: "host.auth.save", Outcome: "failed", Detail: msg})
		return map[string]any{"status": "error", "error": msg, "retryable": true,
			"diagnostics": loginDiagEntries(state)}
	}
	var saveResp pluginapi.HostAuthSaveResponse
	_ = json.Unmarshal(saveEnv.Result, &saveResp)

	// Persisted: the cached credential copy is no longer needed for retries.
	loginResultForget(state)
	loginDiagAdd(state, loginDiagEntry{Step: "host.auth.save", Outcome: "ok",
		Detail: fmt.Sprintf("saved as %q", saveResp.Name)})

	// A Global login whose response omitted `domain` still lands on the right
	// side of every downstream branch (see domainForRegion).
	region := accountRegion(sa)
	// Drop a legacy workbuddy.json left beside the canonical uid file, so the
	// host does not end up with two auth records for one credential.
	if saveResp.Name != "" && !strings.EqualFold(saveResp.Name, authFileName) {
		if legacyPath := strings.TrimSpace(saveResp.Path); legacyPath != "" {
			dir := filepath.Dir(legacyPath)
			_ = deleteAuthFileInDir(filepath.Join(dir, authFileName), dir)
		}
	}
	out := map[string]any{
		"status":   "success",
		"region":   region,
		"uid":      sa.Account.UID,
		"nickname": sa.Account.Nickname,
		"domain":   sa.Auth.Domain,
		"name":     saveResp.Name,
		"path":     saveResp.Path,
	}
	// Warn loudly when the credential was saved under a name the plugin's own
	// listing cannot see (empty uid → bare workbuddy.json, filtered by
	// hostAuthList). Without this the panel would report success while the
	// account is invisible everywhere.
	if strings.EqualFold(saveResp.Name, authFileName) || sa.Account.UID == "" {
		out["warning"] = "凭证已保存为 " + authFileName + "（无 uid），插件列表不会显示该账号；请重新登录或检查 login/account 接口"
	}
	out["diagnostics"] = loginDiagSummary(state, 8)
	// The record is deliberately NOT cleared here: on success the user may still
	// want to see what happened (e.g. a login/account warning above), and on
	// failure it is the whole point. Growth is bounded by the per-state ring
	// buffer and the state cap in logindiag.go; the janitor prunes expired
	// flows alongside their login state.
	return out
}

// lastDiagEntry returns the most recent recorded step, or nil when the state has
// no diagnostics (e.g. it was already cleared).
func lastDiagEntry(state string) *loginDiagEntry {
	entries := loginDiagEntries(state)
	if len(entries) == 0 {
		return nil
	}
	last := entries[len(entries)-1]
	return &last
}

// handleLoginDiag returns the recorded steps of one login flow (or the list of
// known flows when no state is given). Exists so a failure can be captured from
// a deployed instance and pasted back for analysis without re-running the
// login — the panel shows the same data, this makes it machine-readable.
//
// Query: ?state=<state>  (optional; omit to list known flows)
// The payload is redaction-safe by construction: shapes, statuses, codes and
// request ids only — never token material.
func handleLoginDiag(req pluginapi.ManagementRequest) map[string]any {
	state := ""
	if v := req.Query["state"]; len(v) > 0 {
		state = strings.TrimSpace(v[0])
	}
	if state == "" {
		// List flows that still have a record. NOTE: never call
		// loginDiagEntries while holding loginDiagMu — it takes the same
		// non-reentrant mutex and deadlocks (caught by
		// TestLoginDiagEndpoint). loginDiagStates copies the keys under the
		// lock and releases it before we read each flow.
		states := make([]map[string]any, 0)
		for _, s := range loginDiagStates() {
			entries := loginDiagEntries(s)
			last := ""
			if len(entries) > 0 {
				last = entries[len(entries)-1].Outcome
			}
			states = append(states, map[string]any{
				"state": s, "steps": len(entries), "last_outcome": last,
			})
		}
		return map[string]any{"flows": states, "count": len(states)}
	}
	entries := loginDiagEntries(state)
	return map[string]any{
		"state":   state,
		"steps":   entries,
		"summary": loginDiagSummary(state, 0),
	}
}

// handleLoginConfig reports the region host-driven logins will use, and lets
// the panel set it. Runtime-only like checkin/config: the CPA host exposes no
// plugin-config write callback, so config_yaml wins again on restart.
func handleLoginConfig(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		DefaultRegion string `json:"default_region"`
	}
	_ = json.Unmarshal(req.Body, &body)
	if v := strings.TrimSpace(body.DefaultRegion); v != "" {
		setDefaultLoginRegion(v)
	}
	return map[string]any{
		"default_region": string(defaultLoginRegion()),
		"persistent":     false,
	}
}

func handleCheckinConfig(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	_ = json.Unmarshal(req.Body, &body)
	checkinAutoMu.Lock()
	if body.Enabled != nil {
		// Runtime-only toggle: the CPA host exposes no plugin-config write
		// callback, so persisting would mean editing the host's config.yaml
		// from inside the plugin (fragile under docker volume mounts). The
		// value from config_yaml wins again on CPA restart.
		checkinAuto = *body.Enabled
	}
	cur := checkinAuto
	checkinAutoMu.Unlock()
	return map[string]any{"checkin_auto": cur, "persistent": false}
}

// handleClaimTrial claims the expert trial pack for one Global account.
// CN accounts are rejected — the trial endpoint is Global-only.
func handleClaimTrial(req pluginapi.ManagementRequest) map[string]any {
	return handleClaimTrialWithCallback(req, "")
}

func handleClaimTrialWithCallback(req pluginapi.ManagementRequest, callbackID string) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required"}
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	for _, f := range files {
		if f.AuthIndex != authIndex {
			continue
		}
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			return map[string]any{"auth_index": authIndex, "error": err.Error()}
		}
		if !isGlobalDomain(sa.Auth.Domain) {
			return map[string]any{"auth_index": authIndex, "error": "专家加油包仅适用于国际版账号"}
		}
		res, err := performTrialCallWithCallback(sa, callbackID)
		out := map[string]any{"auth_index": authIndex, "nickname": sa.Account.Nickname}
		if err != nil {
			out["error"] = err.Error()
		} else {
			for k, v := range res {
				out[k] = v
			}
		}
		// Invalidate credits cache (copy entry, set credits=nil, keep plan/checkin).
		if v, ok := accountCache.Load(f.ID); ok {
			if e, ok2 := v.(*accountCacheEntry); ok2 {
				fresh := *e
				fresh.credits = nil
				fresh.fetched = time.Now()
				accountCache.Store(f.ID, &fresh)
			}
		}
		if lifecycleEnabled() {
			_, _ = reconcileOneAccountWithCallback(authIndex, f.ID, true, callbackID)
		}
		return out
	}
	return map[string]any{"error": "account not found"}
}

// handleSelectAuth sets the panel-selected account used for chat routing.
// Region (CN/Global) is read from that account's stored domain on each request.
func handleSelectAuth(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required", "active_auth": getActiveAuthID()}
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	for _, f := range files {
		if f.AuthIndex != authIndex {
			continue
		}
		if f.Disabled {
			return map[string]any{"error": "账号已禁用，无法选中", "auth_index": authIndex}
		}
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			return map[string]any{"error": err.Error(), "auth_index": authIndex}
		}
		setActiveAuthID(f.ID)
		return map[string]any{
			"ok":          true,
			"active_auth": f.ID,
			"region":      accountRegion(sa),
			"nickname":    sa.Account.Nickname,
			"uid":         sa.Account.UID,
		}
	}
	return map[string]any{"error": "account not found", "auth_index": authIndex}
}

// handleCreditsQuery returns real-time credits for one or all accounts.
// Pass ?auth_index=<idx> to query a single account; omit for all.
// Single-account mode returns full account info (nickname, region, credits,
// exhausted, trial_claimed) so the panel can update one card without
// reloading the entire dashboard.
func handleCreditsQuery(req pluginapi.ManagementRequest) map[string]any {
	return handleCreditsQueryWithCallback(req, "")
}

func handleCreditsQueryWithCallback(req pluginapi.ManagementRequest, callbackID string) map[string]any {
	authIndex := ""
	if vals := req.Query["auth_index"]; len(vals) > 0 {
		authIndex = strings.TrimSpace(vals[0])
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	// Single-account: return one full account row (like dashboard entry).
	if authIndex != "" {
		for _, f := range files {
			if f.AuthIndex != authIndex {
				continue
			}
			sa, err := hostAuthGet(f.AuthIndex)
			if err != nil {
				return map[string]any{"accounts": []map[string]any{{
					"auth_index": authIndex, "error": "load auth: " + err.Error(),
				}}}
			}
			cr, err := fetchUserResourceWithCallback(sa, callbackID)
			acct := map[string]any{
				"auth_index": authIndex,
				"nickname":   sa.Account.Nickname,
				"uid":        sa.Account.UID,
				"region":     accountRegion(sa),
				"name":       f.Name,
				"label":      f.Label,
				"disabled":   f.Disabled,
				"selected":   getActiveAuthID() == f.ID,
			}
			if err != nil {
				acct["error"] = err.Error()
			} else {
				acct["credits"] = cr
				acct["exhausted"] = isCreditsExhausted(cr)
				if isGlobalDomain(sa.Auth.Domain) {
					acct["trial_claimed"] = hasTrialPack(cr)
				}
				// Also fetch plan so the badge updates on lazy load.
				acct["plan"] = fetchPaymentTypeWithCallback(sa, callbackID)
				// Update cache so subsequent dashboard loads see fresh data.
				now := time.Now()
				if cr != nil {
					cr.FetchedAt = now.UTC().Format(time.RFC3339)
				}
				// Merge into existing cache entry (keep checkin if present).
				var prev *accountCacheEntry
				if v, ok := accountCache.Load(f.ID); ok {
					prev, _ = v.(*accountCacheEntry)
				}
				var ci *checkinSummary
				if prev != nil {
					ci = prev.checkin
				}
				plan, _ := acct["plan"].(string)
				accountCache.Store(f.ID, &accountCacheEntry{
					checkin: ci, credits: cr, plan: plan, fetched: now,
				})
			}
			return map[string]any{"accounts": []map[string]any{acct}}
		}
		return map[string]any{"error": "account not found"}
	}
	// All accounts: return simplified list.
	type acctCredits struct {
		AuthIndex string          `json:"auth_index"`
		Nickname  string          `json:"nickname"`
		UID       string          `json:"uid"`
		Credits   *creditsSummary `json:"credits,omitempty"`
		Error     string          `json:"error,omitempty"`
	}
	var out []acctCredits
	for _, f := range files {
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			out = append(out, acctCredits{AuthIndex: f.AuthIndex, Error: "load auth: " + err.Error()})
			continue
		}
		cr, err := fetchUserResourceWithCallback(sa, callbackID)
		ac := acctCredits{AuthIndex: f.AuthIndex, Nickname: sa.Account.Nickname, UID: sa.Account.UID}
		if err != nil {
			ac.Error = err.Error()
		} else {
			ac.Credits = cr
		}
		out = append(out, ac)
	}
	return map[string]any{"accounts": out}
}
