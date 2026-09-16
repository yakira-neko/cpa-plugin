// logindiag.go keeps an in-memory, redacted record of every step of an
// in-flight login, so a failure can be SEEN instead of collapsing into
// "waiting for login".
//
// Why this exists: handlePollLogin used to map every non-token outcome onto
// AuthLoginStatusPending, which made three very different situations
// indistinguishable to the user (and to us): a genuinely pending browser
// login, a terminal upstream refusal, and a success payload the plugin could
// not decode. The panel would spin until the 5-minute TTL and then report a
// generic expiry, with no way to tell which had happened.
//
// Design constraints:
//   - Never persisted to disk. The record only needs to outlive one login flow;
//     keeping it in memory avoids volume, permission and rotation concerns.
//   - Never stores token material. jsonShape renders STRUCTURE only (key names,
//     nesting, value lengths) and is deliberately content-free, so a record can
//     be shown in the panel or returned over the management API even when the
//     payload carried accessToken / refreshToken. Free text goes through
//     redactSecrets.
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// loginDiagCapPerState bounds the entries kept for one login flow. A poll
	// every 2s over the 5-minute TTL is ~150 polls, so 24 keeps the tail (which
	// is what matters when diagnosing a failure) without unbounded growth.
	loginDiagCapPerState = 24
	// loginDiagMaxStates bounds how many concurrent flows are remembered.
	loginDiagMaxStates = 16
)

// loginDiagEntry is one observed step of a login flow, shaped for JSON so the
// management endpoint can return it verbatim.
type loginDiagEntry struct {
	Seq        int    `json:"seq"`
	At         string `json:"at"`
	Region     string `json:"region,omitempty"`
	Step       string `json:"step"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Code       int    `json:"code,omitempty"`
	Msg        string `json:"msg,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	Outcome    string `json:"outcome"`
	Detail     string `json:"detail,omitempty"`
	Shape      string `json:"shape,omitempty"`
	// Count is how many consecutive identical polls this entry represents.
	// A normal login polls every 2s for a minute or two, so without collapsing
	// the log is two dozen identical "pending" lines and the one interesting
	// line is buried. Repeats are folded into a single entry instead.
	Count int `json:"count,omitempty"`
}

// loginDiagLog is the per-state ring buffer.
type loginDiagLog struct {
	mu      sync.Mutex
	entries []loginDiagEntry
	seq     int
}

var (
	loginDiagMu    sync.Mutex
	loginDiags     = map[string]*loginDiagLog{}
	loginDiagOrder []string // FIFO of states, for eviction
)

func loginDiagFor(state string) *loginDiagLog {
	loginDiagMu.Lock()
	defer loginDiagMu.Unlock()
	d, ok := loginDiags[state]
	if !ok {
		d = &loginDiagLog{}
		loginDiags[state] = d
		loginDiagOrder = append(loginDiagOrder, state)
		// Evict oldest flows beyond the cap.
		for len(loginDiagOrder) > loginDiagMaxStates {
			oldest := loginDiagOrder[0]
			loginDiagOrder = loginDiagOrder[1:]
			delete(loginDiags, oldest)
		}
	}
	return d
}

// loginDiagAdd appends one step. detail and msg are redacted; shape is expected
// to come from jsonShape (content-free).
//
// Consecutive identical steps are folded together (Count++), so a minute of
// 2-second "pending" polls reads as one line rather than two dozen. Collapsing
// keeps the interesting entry — the one that differs — visible in a short log.
func loginDiagAdd(state string, e loginDiagEntry) loginDiagEntry {
	if strings.TrimSpace(state) == "" {
		return e
	}
	e.At = time.Now().Format(time.RFC3339)
	e.Msg = truncateRedacted(e.Msg, 200)
	e.Detail = truncateRedacted(e.Detail, 400)
	if len(e.Shape) > 2000 {
		e.Shape = e.Shape[:2000] + "\n...(truncated)"
	}
	d := loginDiagFor(state)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq++
	e.Seq = d.seq
	e.Count = 1
	// Fold into the previous entry when nothing meaningful changed. requestId
	// changes on every response, so it is compared separately and kept as the
	// FIRST occurrence's value (the earliest is the most useful for a trace).
	if n := len(d.entries); n > 0 {
		prev := &d.entries[n-1]
		if prev.Step == e.Step && prev.Outcome == e.Outcome && prev.Code == e.Code &&
			prev.HTTPStatus == e.HTTPStatus && prev.Detail == e.Detail && prev.Msg == e.Msg {
			prev.Count++
			prev.At = e.At // keep the latest timestamp
			if prev.RequestID == "" {
				prev.RequestID = e.RequestID
			}
			e.Count = prev.Count
			e.Seq = prev.Seq
			return e
		}
	}
	d.entries = append(d.entries, e)
	if len(d.entries) > loginDiagCapPerState {
		d.entries = d.entries[len(d.entries)-loginDiagCapPerState:]
	}
	return e
}

// loginDiagEntries returns a copy of the recorded steps for one state.
func loginDiagEntries(state string) []loginDiagEntry {
	loginDiagMu.Lock()
	d, ok := loginDiags[state]
	loginDiagMu.Unlock()
	if !ok {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]loginDiagEntry, len(d.entries))
	copy(out, d.entries)
	return out
}

// loginDiagClear drops a flow's record.
func loginDiagClear(state string) {
	loginDiagMu.Lock()
	defer loginDiagMu.Unlock()
	if _, ok := loginDiags[state]; !ok {
		return
	}
	delete(loginDiags, state)
	for i, s := range loginDiagOrder {
		if s == state {
			loginDiagOrder = append(loginDiagOrder[:i], loginDiagOrder[i+1:]...)
			break
		}
	}
}

// loginDiagPrune drops records whose login flow is no longer live, so a
// long-lived process does not accumulate dead diagnostics. Driven by the same
// janitor that sweeps abandoned loginStates.
func loginDiagPrune(live func(state string) bool) {
	loginDiagMu.Lock()
	defer loginDiagMu.Unlock()
	for state := range loginDiags {
		if live(state) {
			continue
		}
		delete(loginDiags, state)
		for i, s := range loginDiagOrder {
			if s == state {
				loginDiagOrder = append(loginDiagOrder[:i], loginDiagOrder[i+1:]...)
				break
			}
		}
	}
}

// loginDiagRepeatCount reports how many consecutive times this exact upstream
// business code has been seen at the given step. It powers the fast-fail rule:
// an unrecognised code is treated as "still pending" at first (so an unknown
// pending variant cannot break a working login) but becomes terminal once it
// repeats, which is what stops a hard refusal from masquerading as a wait.
//
// Counts the collapsed Count of each matching entry, not merely the number of
// entries — identical polls are folded into one entry (see loginDiagAdd), so
// counting entries would under-report and delay the fast-fail.
func loginDiagRepeatCount(state, step string, code int) int {
	n := 0
	entries := loginDiagEntries(state)
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Step != step {
			continue
		}
		if e.Code != code {
			break
		}
		c := e.Count
		if c < 1 {
			c = 1
		}
		n += c
	}
	return n
}

// jsonShape renders a JSON payload's STRUCTURE without any scalar content: key
// names, nesting depth, array lengths and string lengths — never a value.
//
// This is what makes a diagnostic record safe to display even when the payload
// is the token bundle. It is the single reason the login diagnostics can be
// turned on by default rather than behind a "show me the raw body" switch.
func jsonShape(raw []byte) string {
	if len(raw) == 0 {
		return "(empty)"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Sprintf("(not JSON, %d bytes)", len(raw))
	}
	return shapeValue(v, 0)
}

func shapeValue(v any, depth int) string {
	if depth > 6 {
		return "..."
	}
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
			b.WriteString(indent + "  " + k + ": " + shapeValue(t[k], depth+1) + ",\n")
		}
		b.WriteString(indent + "}")
		return b.String()
	case []any:
		return fmt.Sprintf("[%d items]", len(t))
	case string:
		// Content-free by construction: only the length is reported.
		return fmt.Sprintf("string(len=%d)", len(t))
	case float64:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", t)
	}
}

// loginResultCache holds a completed login's credential briefly, keyed by the
// login state, so a credential that was successfully obtained is never lost.
//
// Why: the upstream auth/token state is single-use, and the credential exists
// only in that one response. Two callers need it after the state is consumed:
//   - the host-driven path (auth.login.poll), which persists the credential
//     itself and never calls handleLoginPoll; and
//   - the panel path, whose host.auth.save can fail and be retried.
//
// Without this, a failed save destroyed a completed login irrecoverably (the
// retry answered "unknown state"). Entries are short-lived and bounded; the
// janitor prunes them on the same tick as the login states.
type loginResultEntry struct {
	sa      *storedAuth
	expires time.Time
}

var (
	loginResultMu sync.Mutex
	loginResults  = map[string]*loginResultEntry{}
)

// loginResultTTL bounds how long a completed-but-unpersisted credential is
// recoverable. Deliberately short: it only has to outlive a save retry.
const loginResultTTL = 10 * time.Minute

func loginResultStore(state string, sa *storedAuth) {
	if strings.TrimSpace(state) == "" || sa == nil {
		return
	}
	loginResultMu.Lock()
	defer loginResultMu.Unlock()
	loginResults[state] = &loginResultEntry{sa: sa, expires: time.Now().Add(loginResultTTL)}
}

// loginResultTake returns a cached credential without consuming it, so the
// panel can retry a failed save and the host path can still read it. The entry
// expires on its own; successful persistence does not need to invalidate it.
func loginResultTake(state string) (*storedAuth, bool) {
	loginResultMu.Lock()
	defer loginResultMu.Unlock()
	e, ok := loginResults[state]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expires) {
		delete(loginResults, state)
		return nil, false
	}
	return e.sa, true
}

// loginResultForget drops a cached credential (called once it is persisted).
func loginResultForget(state string) {
	loginResultMu.Lock()
	defer loginResultMu.Unlock()
	delete(loginResults, state)
}

// loginResultPrune drops expired cached credentials.
func loginResultPrune() {
	loginResultMu.Lock()
	defer loginResultMu.Unlock()
	now := time.Now()
	for k, v := range loginResults {
		if now.After(v.expires) {
			delete(loginResults, k)
		}
	}
}

// loginDiagStates returns the known flow states. Safe to call while holding no
// lock; it takes loginDiagMu only for the duration of the copy. Callers must
// NOT already hold loginDiagMu (it is a plain sync.Mutex, not reentrant).
func loginDiagStates() []string {
	loginDiagMu.Lock()
	defer loginDiagMu.Unlock()
	out := make([]string, 0, len(loginDiags))
	for s := range loginDiags {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// loginDiagSummary renders the last few steps as short human-readable lines for
// the panel. Redacted by construction (see loginDiagAdd).
func loginDiagSummary(state string, maxLines int) []string {
	entries := loginDiagEntries(state)
	if maxLines > 0 && len(entries) > maxLines {
		entries = entries[len(entries)-maxLines:]
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		line := fmt.Sprintf("#%d %s %s", e.Seq, e.Step, e.Outcome)
		if e.Count > 1 {
			line += fmt.Sprintf(" ×%d", e.Count)
		}
		if e.HTTPStatus > 0 {
			line += fmt.Sprintf(" http=%d", e.HTTPStatus)
		}
		if e.Code != 0 {
			line += fmt.Sprintf(" code=%d", e.Code)
		}
		if e.Detail != "" {
			line += " — " + e.Detail
		}
		out = append(out, line)
	}
	return out
}
