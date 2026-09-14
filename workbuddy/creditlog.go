// creditlog.go implements request-level credit-consumption accounting for the
// workbuddy panel:
//
//   - The host calls UsagePlugin.handleUsage after every request; we record one
//     creditEntry per request (model, auth/uid, tokens, estimated credits,
//     latency, failure info).
//   - Entries live in an in-memory ring buffer per auth (uid), capped in both
//     count and age so a long-running host does not grow without bound.
//   - Aggregations: by model (閰嶉鍒嗗竷), by local hour (鏃舵鍒嗗竷), and per
//     session (session-level credit spend).
//   - Session identity: the OpenAI "session_id" message/system field when the
//     client supplies one (some CLI clients do); otherwise the model is kept
//     in a global "default" session.
//
// Credit estimation: CodeBuddy does not report credits per request; the
// billing API only exposes cycle-level remain/used per package. We therefore
// estimate request cost as
//
//	credits 鈮?round((input + cached_tokens脳cacheFactor + output
//	             + reasoning) / tokensPerCredit 脳 modelFactor)
//
// with per-model factors from the static model table (see creditModelFactor)
// and package-level est_credits derived from the same math. Estimates are
// always labeled as estimates ("鈮?) in the panel; the authoritative numbers
// remain the billing API's remain/used.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// Tunables
// -----------------------------------------------------------------------------

const (
	// creditLogPerAuthCap bounds entries kept per auth/uid. 500 requests is
	// roughly a working day of CLI usage; older entries age out anyway.
	creditLogPerAuthCap = 500
	// creditLogMaxAuths bounds how many distinct auth uids keep buffers.
	creditLogMaxAuths = 128
	// creditLogTTL bounds entry age; the janitor prunes older entries.
	creditLogTTL = 7 * 24 * time.Hour
	// creditLogPruneInterval is the janitor sweep period.
	creditLogPruneInterval = 30 * time.Minute
	// creditSessionsCap bounds session summaries retained globally.
	creditSessionsCap = 256
	// tokensPerCredit is the CodeBuddy coding-plan conversion used for
	// estimates: 1 credit 鈮?1000 tokens at factor 1.0 models.
	tokensPerCredit = 1000
	// cachedTokensFactor discounts prompt-cache hits (cheap to serve).
	cachedTokensFactor = 0.1
	// creditSessionIdle expires a session after this much inactivity.
	creditSessionIdle = 30 * time.Minute
)

// creditSessionKey is the synthetic session bucket for requests without an
// explicit session_id.
const creditSessionKey = "default"

// -----------------------------------------------------------------------------
// Data model
// -----------------------------------------------------------------------------

// creditEntry is one request-level credit-consumption record.
type creditEntry struct {
	At         int64  `json:"at"`                    // unix seconds (request start)
	Model      string `json:"model"`                 // resolved upstream model
	Alias      string `json:"alias,omitempty"`       // client-facing alias (when different)
	AuthIndex  string `json:"auth_index,omitempty"`  // host auth index (when known)
	UID        string `json:"uid"`                   // account uid (buffer key)
	Input      int64  `json:"input_tokens"`          // prompt tokens
	Output     int64  `json:"output_tokens"`         // completion tokens
	Reasoning  int64  `json:"reasoning_tokens"`      // reasoning tokens
	Cached     int64  `json:"cached_tokens"`         // cache-read tokens (prompt cache hits)
	CacheWrite int64  `json:"cache_creation_tokens"` // cache-creation tokens (cache writes)
	Total      int64  `json:"total_tokens"`          // reported total (0 鈫?derived)
	Credits    int64  `json:"credits"`               // estimated credits (rounded, 鈮?)
	Failed     bool   `json:"failed,omitempty"`      // request failed
	StatusCode int    `json:"status_code,omitempty"` // upstream failure status
	Error      string `json:"error,omitempty"`       // redacted failure snippet
	Latency    int64  `json:"latency_ms"`            // total request latency
	Session    string `json:"session,omitempty"`     // session bucket ("" 鈫?default)
}

// sessionKey returns the effective session bucket of the entry.
func (e creditEntry) sessionKey() string {
	if s := strings.TrimSpace(e.Session); s != "" {
		return s
	}
	return creditSessionKey
}

// creditSession is one session-level spend summary.
type creditSession struct {
	ID        string   `json:"session"`    // session id or "default"
	StartedAt int64    `json:"started_at"` // first request (unix s)
	LastAt    int64    `json:"last_at"`    // most recent request (unix s)
	Requests  int64    `json:"requests"`
	Credits   float64  `json:"credits"` // fractional sum for stable ordering
	Models    []string `json:"models,omitempty"`
}

// creditAuthLog is the ring buffer for one auth uid.
type creditAuthLog struct {
	entries []creditEntry
}

// creditLogStore is the global credit-log store. One mutex covers both maps 鈥?// the hot path is a single append and the panel path is a handful of reads
// per refresh; contention is not a concern at panel refresh rates.
type creditLogStore struct {
	mu       sync.Mutex
	byAuth   map[string]*creditAuthLog
	sessions map[string]*creditSession
	started  bool
}

var creditLog = &creditLogStore{byAuth: map[string]*creditAuthLog{}, sessions: map[string]*creditSession{}}
var creditLogPersistMu sync.Mutex
var creditLogLoaded bool

func creditLogPath() string {
	if p := strings.TrimSpace(os.Getenv("WB_CREDIT_LOG_PATH")); p != "" {
		return p
	}
	return filepath.Join(os.TempDir(), "workbuddy-creditlog.jsonl")
}
func loadCreditLogOnce() {
	creditLogPersistMu.Lock()
	defer creditLogPersistMu.Unlock()
	if creditLogLoaded {
		return
	}
	creditLogLoaded = true
	b, err := os.ReadFile(creditLogPath())
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		var e creditEntry
		if json.Unmarshal([]byte(line), &e) != nil || e.Model == "" {
			continue
		}
		creditLog.mu.Lock()
		creditLog.appendLocked(e.UID, e)
		creditLog.foldSessionLocked(e, e.sessionKey(), time.Unix(e.At, 0))
		creditLog.mu.Unlock()
	}
}
func persistCreditEntry(e creditEntry) {
	creditLogPersistMu.Lock()
	defer creditLogPersistMu.Unlock()
	p := creditLogPath()
	_ = os.MkdirAll(filepath.Dir(p), 0700)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(e)
	_, _ = f.Write(append(b, '\n'))
}

// creditNow is overridable in tests.
var creditNow = time.Now

// -----------------------------------------------------------------------------
// Recording (hot path)
// -----------------------------------------------------------------------------

// recordCreditUsage appends one request-level entry and folds it into session
// aggregates. Safe for concurrent use; never blocks the executor hot path.
func recordCreditUsage(authUID, authIndex, alias, model string, started time.Time, detail usageDetailLite, failed bool, statusCode int, errSnippet string, latency time.Duration, session string) {
	loadCreditLogOnce()
	if model == "" {
		model = alias
	}
	if model == "" {
		// Nothing meaningful to bill 鈥?skip (e.g. malformed internal call).
		return
	}
	at := started
	if at.IsZero() {
		at = creditNow()
	}
	in := detail.InputTokens
	out := detail.OutputTokens + detail.ReasoningTokens
	// Cache reads (hits) and cache writes (creation) are distinct counters and
	// the panel shows them as separate columns. CacheReadTokens is the
	// canonical read counter; CachedTokens is the OpenAI-style alias for the
	// same number, while CacheCreationTokens is the write side.
	cacheRead := detail.CacheReadTokens
	if cacheRead == 0 {
		cacheRead = detail.CachedTokens
	}
	cacheWrite := detail.CacheCreationTokens
	total := detail.TotalTokens
	if total == 0 {
		total = in + out + cacheRead + cacheWrite
	}
	est := estimateCredits(model, in, out, cacheRead)
	entry := creditEntry{
		At:         at.Unix(),
		Model:      model,
		Alias:      alias,
		AuthIndex:  authIndex,
		UID:        authUID,
		Input:      in,
		Output:     detail.OutputTokens,
		Reasoning:  detail.ReasoningTokens,
		Cached:     cacheRead,
		CacheWrite: cacheWrite,
		Total:      total,
		Credits:    est,
		Failed:     failed,
		StatusCode: statusCode,
		Error:      truncateRedacted(errSnippet, 160),
		Latency:    latency.Milliseconds(),
		Session:    strings.TrimSpace(session),
	}
	key := entry.sessionKey()

	creditLog.mu.Lock()
	defer creditLog.mu.Unlock()
	creditLog.appendLocked(authUID, entry)
	creditLog.foldSessionLocked(entry, key, at)
	go persistCreditEntry(entry)
}

// appendLocked stores the entry in the per-auth ring buffer, evicting the
// oldest when over cap. Caller holds creditLog.mu.
func (s *creditLogStore) appendLocked(uid string, e creditEntry) {
	if uid == "" {
		uid = "unknown"
	}
	buf, ok := s.byAuth[uid]
	if !ok {
		// Evict the stalest buffer when at capacity (auth churn).
		if len(s.byAuth) >= creditLogMaxAuths {
			oldestKey := ""
			var oldestAt int64 = 1 << 62
			for k, b := range s.byAuth {
				if len(b.entries) == 0 {
					oldestKey = k
					break
				}
				if last := b.entries[len(b.entries)-1].At; last < oldestAt {
					oldestAt = last
					oldestKey = k
				}
			}
			if oldestKey != "" {
				delete(s.byAuth, oldestKey)
			}
		}
		buf = &creditAuthLog{}
		s.byAuth[uid] = buf
	}
	buf.entries = append(buf.entries, e)
	if len(buf.entries) > creditLogPerAuthCap {
		// Drop oldest ~10% in a batch so we do not shift on every request.
		drop := creditLogPerAuthCap / 10
		if drop < 1 {
			drop = 1
		}
		buf.entries = append(buf.entries[:0], buf.entries[drop:]...)
	}
}

// foldSessionLocked merges one entry into the session aggregates. Caller holds
// creditLog.mu.
func (s *creditLogStore) foldSessionLocked(e creditEntry, key string, at time.Time) {
	sess, ok := s.sessions[key]
	if !ok {
		if len(s.sessions) >= creditSessionsCap {
			s.pruneSessionsLocked()
			if len(s.sessions) >= creditSessionsCap {
				// Still full: drop the oldest session outright.
				oldest := ""
				var oldestAt int64 = 1 << 62
				for k, v := range s.sessions {
					if v.LastAt < oldestAt {
						oldestAt = v.LastAt
						oldest = k
					}
				}
				if oldest != "" {
					delete(s.sessions, oldest)
				}
			}
		}
		sess = &creditSession{ID: key}
		s.sessions[key] = sess
	}
	if sess.StartedAt == 0 || e.At < sess.StartedAt {
		sess.StartedAt = e.At
	}
	if e.At > sess.LastAt {
		sess.LastAt = e.At
	}
	sess.Requests++
	sess.Credits += float64(e.Credits)
	if !containsString(sess.Models, e.Model) {
		sess.Models = append(sess.Models, e.Model)
	}
	_ = at
}

// pruneSessionsLocked drops sessions idle beyond creditSessionIdle. Caller
// holds creditLog.mu.
func (s *creditLogStore) pruneSessionsLocked() {
	cutoff := creditNow().Add(-creditSessionIdle).Unix()
	for k, v := range s.sessions {
		if v.LastAt < cutoff {
			delete(s.sessions, k)
		}
	}
}

// containsString is a tiny membership helper (avoids importing slices).
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// usageDetailLite decouples this file from the SDK usage struct so tests can
// build records without the full pluginabi stack.
type usageDetailLite struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

// -----------------------------------------------------------------------------
// Credit estimation
// -----------------------------------------------------------------------------

// estimateCredits returns the estimated credits for one request. The result is
// rounded (鈮? for any non-zero spend) so the panel shows integers.
func estimateCredits(model string, input, output, cached int64) int64 {
	weighted := float64(input) +
		float64(output)*creditModelFactor(model) +
		float64(cached)*cachedTokensFactor
	if weighted <= 0 {
		return 0
	}
	credits := weighted / tokensPerCredit
	if credits < 1 {
		// Small requests still consume a chargeable unit in practice; report
		// 鈮? so totals never under-count sparse but real usage.
		return 1
	}
	return int64(credits + 0.5)
}

// creditModelFactor returns the relative cost factor of a model. CodeBuddy
// publishes per-model pricing only inside the web app; the plugin's static
// model table carries a stable approximation (expert-tier models cost more
// than lightweight ones). Unknown models fall back to 1.0.
func creditModelFactor(model string) float64 {
	m := strings.ToLower(strings.TrimSpace(model))
	switch m {
	case "glm-5.3", "glm-5.2", "kimi-k3-1", "deepseek-v4-pro", "hy4-preview-x":
		return 1.5
	case "glm-5.1", "glm-5v-turbo", "minimax-m3", "hy4-preview", "hy3-x", "hy3":
		return 1.0
	case "kimi-k2.7", "kimi-k2.6", "hy3-preview", "hy3-preview-agent":
		return 0.8
	case "glm-5.3-flash", "deepseek-v4-flash", "deepseek-v4.1-flash":
		return 0.5
	default:
		return 1.0
	}
}

// -----------------------------------------------------------------------------
// Queries / aggregation (panel path)
// -----------------------------------------------------------------------------

// creditSnapshot returns the panel-facing credit log for one auth uid:
// entries (newest first), per-model totals, per-hour totals, and sessions.
// hourCount bounds the 鏃舵鍒嗗竷 buckets (0 鈫?default 24).
func creditSnapshot(uid string, limit int, hourCount int) map[string]any {
	if hourCount <= 0 {
		hourCount = 24
	}
	if limit <= 0 || limit > creditLogPerAuthCap {
		limit = creditLogPerAuthCap
	}
	creditLog.mu.Lock()
	buf := creditLog.byAuth[uid]
	all := make([]creditEntry, 0, limit)
	if buf != nil {
		start := 0
		if len(buf.entries) > limit {
			start = len(buf.entries) - limit
		}
		all = append(all, buf.entries[start:]...)
	}
	sessions := make([]creditSession, 0, len(creditLog.sessions))
	for _, v := range creditLog.sessions {
		sessions = append(sessions, *v)
	}
	creditLog.mu.Unlock()

	// Newest first for display.
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	if len(all) > limit {
		all = all[:limit]
	}

	var credits, spentIn, spentOut, spentCached, spentCacheWrite, requests, failedN int64
	modelTotals := map[string]*creditModelTotal{}
	hourTotals := make(map[int]*creditHourTotal, hourCount)
	localZone := time.Local
	for _, e := range all {
		requests++
		credits += e.Credits
		spentIn += e.Input
		spentOut += e.Output + e.Reasoning
		spentCached += e.Cached
		spentCacheWrite += e.CacheWrite
		if e.Failed {
			failedN++
		}
		mt, ok := modelTotals[e.Model]
		if !ok {
			mt = &creditModelTotal{Model: e.Model}
			modelTotals[e.Model] = mt
		}
		mt.Requests++
		mt.Credits += e.Credits
		mt.InputTokens += e.Input
		mt.OutputTokens += e.Output + e.Reasoning
		mt.CachedTokens += e.Cached
		mt.CacheCreationTokens += e.CacheWrite
		if e.Failed {
			mt.Failed++
		}
		localHour := time.Unix(e.At, 0).In(localZone).Hour()
		ht, ok := hourTotals[localHour]
		if !ok {
			ht = &creditHourTotal{Hour: localHour}
			hourTotals[localHour] = ht
		}
		ht.Requests++
		ht.Credits += e.Credits
	}
	models := make([]creditModelTotal, 0, len(modelTotals))
	for _, v := range modelTotals {
		models = append(models, *v)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Credits != models[j].Credits {
			return models[i].Credits > models[j].Credits
		}
		return models[i].Model < models[j].Model
	})
	hours := make([]creditHourTotal, 0, len(hourTotals))
	for _, v := range hourTotals {
		hours = append(hours, *v)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i].Hour < hours[j].Hour })

	// Sort sessions by last activity, newest first.
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].LastAt > sessions[j].LastAt })
	if len(sessions) > creditSessionsCap {
		sessions = sessions[:creditSessionsCap]
	}

	return map[string]any{
		"uid":           uid,
		"requests":      requests,
		"failed":        failedN,
		"credits":       credits,
		"input_tokens":  spentIn,
		"output_tokens": spentOut,
		"cached_tokens": spentCached,
		// cache_creation_tokens is the write side of the prompt cache; kept
		// separate so the panel can show hit vs write columns.
		"cache_creation_tokens": spentCacheWrite,
		"models":                models,
		"hours":                 hours,
		"sessions":              sessions,
		"entries":               all,
		"limits": map[string]any{
			"per_auth_cap": creditLogPerAuthCap,
			"ttl_hours":    int(creditLogTTL / time.Hour),
		},
	}
}

// creditModelTotal aggregates one model's spend inside a snapshot window.
type creditModelTotal struct {
	Model        string `json:"model"`
	Requests     int64  `json:"requests"`
	Credits      int64  `json:"credits"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	// CachedTokens = prompt-cache reads (hits); CacheCreationTokens = writes.
	CachedTokens        int64 `json:"cached_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	Failed              int64 `json:"failed"`
}

// creditHourTotal aggregates one local-hour bucket (0-23).
type creditHourTotal struct {
	Hour     int   `json:"hour"`
	Requests int64 `json:"requests"`
	Credits  int64 `json:"credits"`
}

// creditGlobalSummary rolls the whole store up across auths. Used by the
// panel summary card and the /creditlog/summary endpoint.
func creditGlobalSummary() map[string]any {
	creditLog.mu.Lock()
	var requests, credits, failedN, cachedTokens, cacheCreationTokens int64
	modelTotals := map[string]*creditModelTotal{}
	hourTotals := map[int]*creditHourTotal{}
	sessions := make([]creditSession, 0, len(creditLog.sessions))
	auths := make([]map[string]any, 0, len(creditLog.byAuth))
	for uid, buf := range creditLog.byAuth {
		var authCredits int64
		var authRequests int64
		for _, e := range buf.entries {
			requests++
			authRequests++
			credits += e.Credits
			authCredits += e.Credits
			cachedTokens += e.Cached
			cacheCreationTokens += e.CacheWrite
			if e.Failed {
				failedN++
			}
			mt, ok := modelTotals[e.Model]
			if !ok {
				mt = &creditModelTotal{Model: e.Model}
				modelTotals[e.Model] = mt
			}
			mt.Requests++
			mt.Credits += e.Credits
			mt.InputTokens += e.Input
			mt.OutputTokens += e.Output + e.Reasoning
			mt.CachedTokens += e.Cached
			mt.CacheCreationTokens += e.CacheWrite
			if e.Failed {
				mt.Failed++
			}
			localHour := time.Unix(e.At, 0).In(time.Local).Hour()
			ht, ok := hourTotals[localHour]
			if !ok {
				ht = &creditHourTotal{Hour: localHour}
				hourTotals[localHour] = ht
			}
			ht.Requests++
			ht.Credits += e.Credits
		}
		lastAt := int64(0)
		if n := len(buf.entries); n > 0 {
			lastAt = buf.entries[n-1].At
		}
		auths = append(auths, map[string]any{
			"uid":      uid,
			"credits":  authCredits,
			"requests": authRequests,
			"last_at":  lastAt,
		})
	}
	for _, v := range creditLog.sessions {
		sessions = append(sessions, *v)
	}
	creditLog.mu.Unlock()

	models := make([]creditModelTotal, 0, len(modelTotals))
	for _, v := range modelTotals {
		models = append(models, *v)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Credits != models[j].Credits {
			return models[i].Credits > models[j].Credits
		}
		return models[i].Model < models[j].Model
	})
	hours := make([]creditHourTotal, 0, 24)
	for h := 0; h < 24; h++ {
		if ht, ok := hourTotals[h]; ok {
			hours = append(hours, *ht)
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].LastAt > sessions[j].LastAt })
	sort.Slice(auths, func(i, j int) bool {
		ai, _ := auths[i]["credits"].(int64)
		aj, _ := auths[j]["credits"].(int64)
		return ai > aj
	})
	return map[string]any{
		"requests":              requests,
		"failed":                failedN,
		"credits":               credits,
		"cached_tokens":         cachedTokens,
		"cache_creation_tokens": cacheCreationTokens,
		"models":                models,
		"hours":                 hours,
		"sessions":              sessions,
		"auths":                 auths,
	}
}

// creditLogJanitor periodically prunes aged entries and idle sessions. Started
// once from ensureCreditLogJanitor; safe to call multiple times.
func ensureCreditLogJanitor() {
	creditLog.mu.Lock()
	defer creditLog.mu.Unlock()
	if creditLog.started {
		return
	}
	creditLog.started = true
	go func() {
		ticker := time.NewTicker(creditLogPruneInterval)
		defer ticker.Stop()
		for range ticker.C {
			creditLog.pruneOnce()
		}
	}()
}

// pruneOnce drops entries older than creditLogTTL and sessions idle beyond
// creditSessionIdle.
func (s *creditLogStore) pruneOnce() {
	cutoff := creditNow().Add(-creditLogTTL).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, buf := range s.byAuth {
		kept := buf.entries[:0]
		for _, e := range buf.entries {
			if e.At >= cutoff {
				kept = append(kept, e)
			}
		}
		buf.entries = kept
	}
	s.pruneSessionsLocked()
}

// -----------------------------------------------------------------------------
// Management handlers
// -----------------------------------------------------------------------------

// handleCreditLogQuery serves GET /creditlog?uid=&limit=&hours=.
// uid defaults to every auth's uid 鈫?returns per-auth snapshots. With uid set,
// returns the single snapshot plus full entry list.
func handleCreditLogQuery(req mgmtRequestLite) map[string]any {
	loadCreditLogOnce()
	uid := strings.TrimSpace(req.Query("uid"))
	limit := atoiDefault(req.Query("limit"), 100)
	hours := atoiDefault(req.Query("hours"), 24)
	if uid != "" {
		return creditSnapshot(uid, limit, hours)
	}
	// No uid: aggregate everything, plus a compact per-auth breakdown.
	sum := creditGlobalSummary()
	return sum
}

// mgmtRequestLite decouples handlers from pluginapi for testability.
type mgmtRequestLite struct {
	Raw   []byte
	Query func(key string) string
}

// atoiDefault parses s as int, returning def when empty/invalid.
func atoiDefault(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
		if n > 1_000_000 {
			return def
		}
	}
	return n
}

// handleCreditLogSummary serves GET /creditlog/summary (global rollup).
func handleCreditLogSummary() map[string]any {
	return creditGlobalSummary()
}

// parseSessionIDFromRequest best-effort extracts a session identifier from the
// client request body. Accepted shapes:
//   - {"session_id": "..."} top level
//   - messages[].content containing "session_id": "..." (string or array parts)
//
// Returns "" when absent. Kept tolerant 鈥?session attribution is a nice-to-
// have, never a failure path.
func parseSessionIDFromRequest(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.SessionID != "" {
		return strings.TrimSpace(probe.SessionID)
	}
	// Fall back to a shallow scan of message objects for session_id fields.
	var msgProbe struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &msgProbe); err != nil {
		return ""
	}
	for _, m := range msgProbe.Messages {
		var inner struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(m["content"], &inner); err == nil && inner.SessionID != "" {
			return strings.TrimSpace(inner.SessionID)
		}
		var parts []json.RawMessage
		if json.Unmarshal(m["content"], &parts) == nil {
			for _, p := range parts {
				var pObj struct {
					SessionID string `json:"session_id"`
				}
				if err := json.Unmarshal(p, &pObj); err == nil && pObj.SessionID != "" {
					return strings.TrimSpace(pObj.SessionID)
				}
			}
		}
	}
	return ""
}
