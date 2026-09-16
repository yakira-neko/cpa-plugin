// creditrate.go owns the per-model credits <-> tokens rate card: the
// conversion in both directions, and the single forward formula the rest of
// the plugin estimates spend with.
//
// CodeBuddy does not report credits per request; the billing API only exposes
// cycle-level remain/used per package. Per-request cost is therefore estimated
// from token counts with a linear rate card over three billable classes:
//
//	credits = (uncached_in + out*model_factor + cached_in*cache_factor)
//	          / tokens_per_credit
//
// Every per-model figure the panel shows ("1 积分 ≈ N token") is derived from
// this one table, so the displayed rate and the estimated spend can never
// drift apart: tokensForCredits is the exact inverse of creditsForTokens.
//
// Two deliberate details:
//
//   - uncached_in is the prompt count MINUS the cache-read tokens. WorkBuddy
//     folds cache hits into prompt_tokens (verified live: 4443 prompt =
//     4043 cached + 400 miss, pinned in usage_detail_test.go), so passing the
//     raw prompt count here would charge every cache hit twice — once at full
//     rate inside input, once again at the discounted cache rate.
//   - The per-model factor scales OUTPUT tokens only. Input is the un-scaled
//     baseline; that is what makes "tokens per credit" differ per model.
//
// The factors are an approximation: CodeBuddy publishes real per-model pricing
// only inside its web app. They are overridable from config (credit_rates) so
// an operator can correct them without a rebuild.
package main

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	// defaultTokensPerCredit is the CodeBuddy coding-plan conversion used when
	// no operator override is configured: 1 credit = 1000 billable tokens at
	// factor 1.0.
	defaultTokensPerCredit = 1000
	// defaultCachedTokensFactor discounts prompt-cache hits (cheap to serve).
	defaultCachedTokensFactor = 0.1
	// defaultOutputShare is the reference completion share of the
	// billable (uncached prompt + completion) token count, used only when a
	// caller asks for a single blended "tokens per credit" figure without
	// supplying its own mix. CLI coding traffic is prompt-heavy.
	defaultOutputShare = 0.25
)

// tokenClass identifies which billable token class a conversion applies to.
type tokenClass string

const (
	tokenClassInput  tokenClass = "input"
	tokenClassOutput tokenClass = "output"
	tokenClassCached tokenClass = "cached"
)

// staticCreditFactors is the built-in per-model cost factor. Factor 1.0 means
// the model is the baseline: its output tokens cost the same as baseline input
// tokens. Kept in sync with the model list in models.go.
var staticCreditFactors = []struct {
	Model  string
	Factor float64
}{
	// Expert tier — 1.5x
	{"glm-5.3", 1.5},
	{"glm-5.2", 1.5},
	{"kimi-k3-1", 1.5},
	{"deepseek-v4-pro", 1.5},
	{"hy4-preview-x", 1.5},
	// Baseline tier — 1.0x
	{"glm-5.1", 1.0},
	{"glm-5v-turbo", 1.0},
	{"minimax-m3", 1.0},
	{"hy4-preview", 1.0},
	{"hy3-x", 1.0},
	{"hy3", 1.0},
	// Discounted tier — 0.8x
	{"kimi-k2.7", 0.8},
	{"kimi-k2.6", 0.8},
	{"hy3-preview", 0.8},
	{"hy3-preview-agent", 0.8},
	// Lightweight tier — 0.5x
	{"glm-5.3-flash", 0.5},
	{"deepseek-v4-flash", 0.5},
	{"deepseek-v4.1-flash", 0.5},
}

// creditRateSettings holds the tunable scalars of the rate card. Guarded by one
// RWMutex: reads happen on every UsagePlugin record, writes only on configure.
var (
	creditRateMu          sync.RWMutex
	creditTokensPerCredit = float64(defaultTokensPerCredit)
	creditCachedFactor    = defaultCachedTokensFactor
	creditFactorOverrides = map[string]float64{}
)

// modelCreditFactor returns the effective cost factor of a model: an operator
// override wins over the built-in table, and unknown models fall back to the
// baseline 1.0.
func modelCreditFactor(model string) float64 {
	m := strings.ToLower(strings.TrimSpace(model))
	creditRateMu.RLock()
	if f, ok := creditFactorOverrides[m]; ok && f > 0 {
		creditRateMu.RUnlock()
		return f
	}
	creditRateMu.RUnlock()
	for _, e := range staticCreditFactors {
		if e.Model == m {
			return e.Factor
		}
	}
	return 1.0
}

// creditRateScalars returns the effective (tokens_per_credit, cache_factor).
func creditRateScalars() (float64, float64) {
	creditRateMu.RLock()
	defer creditRateMu.RUnlock()
	tpc, cf := creditTokensPerCredit, creditCachedFactor
	if tpc <= 0 {
		tpc = defaultTokensPerCredit
	}
	if cf < 0 {
		cf = defaultCachedTokensFactor
	}
	return tpc, cf
}

// overrideCreditRates installs per-model factor overrides, replacing any
// previous set. Entries with a non-positive factor are ignored.
func overrideCreditRates(rates map[string]float64) {
	next := make(map[string]float64, len(rates))
	for model, f := range rates {
		m := strings.ToLower(strings.TrimSpace(model))
		if m == "" || f <= 0 || math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		next[m] = f
	}
	creditRateMu.Lock()
	creditFactorOverrides = next
	creditRateMu.Unlock()
}

// overrideTokensPerCredit sets the billable-tokens-per-credit scale. Values
// <= 0 restore the default.
func overrideTokensPerCredit(v float64) {
	if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		v = defaultTokensPerCredit
	}
	creditRateMu.Lock()
	creditTokensPerCredit = v
	creditRateMu.Unlock()
}

// resetCreditRates restores every tunable to its built-in default. Test helper.
func resetCreditRates() {
	creditRateMu.Lock()
	creditTokensPerCredit = float64(defaultTokensPerCredit)
	creditCachedFactor = defaultCachedTokensFactor
	creditFactorOverrides = map[string]float64{}
	creditRateMu.Unlock()
}

// parseCreditRates decodes the `credit_rates` config value: comma-separated
// "model=factor" pairs (e.g. "glm-5.3=1.5, kimi-k2.7=0.8"). Malformed entries
// are skipped rather than failing the whole configure pass — a typo in one
// model must not silently discard the operator's other corrections.
func parseCreditRates(raw string) map[string]float64 {
	out := map[string]float64{}
	for _, pair := range strings.Split(raw, ",") {
		kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(kv) != 2 {
			continue
		}
		model := strings.ToLower(strings.TrimSpace(strings.Trim(kv[0], "\"'")))
		if model == "" {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(kv[1]), 64)
		if err != nil || f <= 0 || math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		out[model] = f
	}
	return out
}

// -----------------------------------------------------------------------------
// Conversions
// -----------------------------------------------------------------------------

// creditsForTokens is the forward conversion, unrounded and without the
// one-credit floor. input must be the UNCACHED prompt token count (see the
// file comment). This is the single formula the accounting estimates with.
func creditsForTokens(model string, input, output, cached int64) float64 {
	tpc, cacheFactor := creditRateScalars()
	if tpc <= 0 {
		return 0
	}
	weighted := float64(input) +
		float64(output)*modelCreditFactor(model) +
		float64(cached)*cacheFactor
	if weighted <= 0 {
		return 0
	}
	return weighted / tpc
}

// estimateCredits returns the estimated (rounded) credits for one request.
// A non-zero request never reports 0: sparse-but-real usage still consumes a
// chargeable unit.
//
// input must be the UNCACHED prompt token count.
func estimateCredits(model string, input, output, cached int64) int64 {
	credits := creditsForTokens(model, input, output, cached)
	if credits <= 0 {
		return 0
	}
	if credits < 1 {
		return 1
	}
	return int64(credits + 0.5)
}

// tokensForCredits converts credits back into a token count for one billable
// class. It is the exact inverse of creditsForTokens, so the panel's
// "1 积分 ≈ N token" figure always matches the credits it reports.
//
// The forward path rounds up to a 1-credit floor for tiny requests, so this is
// the linear (asymptotic) rate — the right figure for "how many tokens do N
// credits buy" at any meaningful N.
func tokensForCredits(model string, credits float64, class tokenClass) float64 {
	if credits <= 0 || math.IsNaN(credits) || math.IsInf(credits, 0) {
		return 0
	}
	tpc, cacheFactor := creditRateScalars()
	if tpc <= 0 {
		return 0
	}
	var perToken float64
	switch class {
	case tokenClassOutput:
		perToken = modelCreditFactor(model)
	case tokenClassCached:
		perToken = cacheFactor
	default:
		perToken = 1.0
	}
	if perToken <= 0 {
		return 0
	}
	return credits * tpc / perToken
}

// tokensForCreditsBlended converts credits into a token count for a request
// mix. outputShare is the completion share of the billable token count
// (uncached prompt + completion); cache reads are a separate class and are not
// part of the mix. Values outside [0,1] are clamped.
func tokensForCreditsBlended(model string, credits, outputShare float64) float64 {
	if outputShare < 0 {
		outputShare = 0
	}
	if outputShare > 1 {
		outputShare = 1
	}
	tpc, _ := creditRateScalars()
	if credits <= 0 || tpc <= 0 || math.IsNaN(credits) || math.IsInf(credits, 0) {
		return 0
	}
	perToken := (1-outputShare)*1.0 + outputShare*modelCreditFactor(model)
	if perToken <= 0 {
		return 0
	}
	return credits * tpc / perToken
}

// -----------------------------------------------------------------------------
// Rate card reporting
// -----------------------------------------------------------------------------

// creditRate describes one model's conversion rate, both directions.
type creditRate struct {
	Model  string  `json:"model"`
	Factor float64 `json:"factor"`
	// Source is "override" when the factor came from config, "static" when it
	// came from the built-in table, "default" for an unknown model.
	Source string `json:"source"`
	// TokensPerCredit* answer "how many tokens of this class does 1 credit buy".
	TokensPerCreditInput  float64 `json:"tokens_per_credit_input"`
	TokensPerCreditOutput float64 `json:"tokens_per_credit_output"`
	TokensPerCreditCached float64 `json:"tokens_per_credit_cached"`
	// CreditsPer1K* is the same rate in the forward direction, per 1000 tokens.
	CreditsPer1KInput  float64 `json:"credits_per_1k_input"`
	CreditsPer1KOutput float64 `json:"credits_per_1k_output"`
	CreditsPer1KCached float64 `json:"credits_per_1k_cached"`
}

// creditRateFor builds the rate card for one model. The model need not be in
// the built-in table — unknown models resolve to the 1.0 baseline.
//
// Both directions come from the same two functions the accounting uses
// (tokensForCredits / creditsForTokens), so the reported card cannot drift from
// the numbers it explains.
func creditRateFor(model string) creditRate {
	m := strings.ToLower(strings.TrimSpace(model))
	source := "default"
	creditRateMu.RLock()
	_, overridden := creditFactorOverrides[m]
	creditRateMu.RUnlock()
	if overridden {
		source = "override"
	} else {
		for _, e := range staticCreditFactors {
			if e.Model == m {
				source = "static"
				break
			}
		}
	}
	return creditRate{
		Model:  m,
		Factor: modelCreditFactor(m),
		Source: source,
		// Reverse direction: tokens one credit buys in each class. Rounded to
		// whole tokens — sub-token precision is noise for an approximation.
		TokensPerCreditInput:  math.Round(tokensForCredits(m, 1, tokenClassInput)),
		TokensPerCreditOutput: math.Round(tokensForCredits(m, 1, tokenClassOutput)),
		TokensPerCreditCached: math.Round(tokensForCredits(m, 1, tokenClassCached)),
		// Forward direction: credits 1000 tokens of each class cost.
		CreditsPer1KInput:  round4(creditsForTokens(m, 1000, 0, 0)),
		CreditsPer1KOutput: round4(creditsForTokens(m, 0, 1000, 0)),
		CreditsPer1KCached: round4(creditsForTokens(m, 0, 0, 1000)),
	}
}

// round4 keeps the forward direction readable (4 decimals ≈ sub-cent
// precision on a credit).
func round4(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*10000) / 10000
}

// knownCreditRateModels returns every model the rate card can describe: the
// built-in table plus any operator override, de-duplicated and sorted so API
// output is stable across calls.
func knownCreditRateModels() []string {
	creditRateMu.RLock()
	seen := make(map[string]struct{}, len(staticCreditFactors)+len(creditFactorOverrides))
	for _, e := range staticCreditFactors {
		seen[e.Model] = struct{}{}
	}
	for m := range creditFactorOverrides {
		seen[m] = struct{}{}
	}
	creditRateMu.RUnlock()
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// creditRatesReport builds the /creditlog/rates payload: the model rate card,
// plus per-model token equivalents when the caller asked for a conversion.
//
// withCredits=false reports rates only (the panel's rate reference); =true adds
// "N credits buys M tokens" per model in the three billable classes.
func creditRatesReport(credits, outputShare float64, withCredits bool) map[string]any {
	tpc, cacheFactor := creditRateScalars()
	models := knownCreditRateModels()
	rows := make([]map[string]any, 0, len(models))
	for _, m := range models {
		r := creditRateFor(m)
		row := map[string]any{
			"model":                    r.Model,
			"factor":                   r.Factor,
			"source":                   r.Source,
			"tokens_per_credit_input":  r.TokensPerCreditInput,
			"tokens_per_credit_output": r.TokensPerCreditOutput,
			"tokens_per_credit_cached": r.TokensPerCreditCached,
			"credits_per_1k_input":     r.CreditsPer1KInput,
			"credits_per_1k_output":    r.CreditsPer1KOutput,
			"credits_per_1k_cached":    r.CreditsPer1KCached,
		}
		if withCredits {
			row["tokens"] = math.Round(tokensForCreditsBlended(m, credits, outputShare))
			row["input_tokens"] = math.Round(tokensForCredits(m, credits, tokenClassInput))
			row["output_tokens"] = math.Round(tokensForCredits(m, credits, tokenClassOutput))
			row["cached_tokens"] = math.Round(tokensForCredits(m, credits, tokenClassCached))
		}
		rows = append(rows, row)
	}
	out := map[string]any{
		"tokens_per_credit": tpc,
		"cached_factor":     cacheFactor,
		"output_share":      outputShare,
		"models":            rows,
		"note": "估算值：CodeBuddy 不上报单请求积分，费率由模型系数推导；" +
			"可用 credit_rates 配置覆盖每个模型的系数。",
	}
	if withCredits {
		out["credits"] = credits
		out["converted"] = true
	}
	return out
}
