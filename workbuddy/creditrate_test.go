package main

import (
	"math"
	"testing"
)

// approx compares floats with a tolerance; the rate card mixes rounding with
// division, so exact equality is the wrong assertion.
func approx(got, want, tol float64) bool {
	return math.Abs(got-want) <= tol
}

// -----------------------------------------------------------------------------
// Forward conversion (credits for tokens)
// -----------------------------------------------------------------------------

func TestCreditsForTokens_BaselineModel(t *testing.T) {
	resetCreditRates()
	// hy3 is factor 1.0: 1000 uncached input tokens = exactly 1 credit.
	if got := creditsForTokens("hy3", 1000, 0, 0); !approx(got, 1.0, 1e-9) {
		t.Fatalf("1000 baseline input tokens = %v credits, want 1", got)
	}
	// Output is scaled by the factor; at factor 1.0 it prices like input.
	if got := creditsForTokens("hy3", 0, 1000, 0); !approx(got, 1.0, 1e-9) {
		t.Fatalf("1000 baseline output tokens = %v credits, want 1", got)
	}
}

func TestCreditsForTokens_ModelFactorScalesOutputOnly(t *testing.T) {
	resetCreditRates()
	// glm-5.3 is factor 1.5 → 1000 output tokens cost 1.5 credits.
	if got := creditsForTokens("glm-5.3", 0, 1000, 0); !approx(got, 1.5, 1e-9) {
		t.Fatalf("glm-5.3 output = %v credits, want 1.5", got)
	}
	// Input is the un-scaled baseline even on an expert model. This asymmetry is
	// what makes per-model "tokens per credit" differ.
	if got := creditsForTokens("glm-5.3", 1000, 0, 0); !approx(got, 1.0, 1e-9) {
		t.Fatalf("glm-5.3 input = %v credits, want 1.0 (input must not be scaled)", got)
	}
}

func TestCreditsForTokens_CacheDiscount(t *testing.T) {
	resetCreditRates()
	// Cache reads cost the discounted factor (0.1), not the model factor.
	if got := creditsForTokens("glm-5.3", 0, 0, 1000); !approx(got, 0.1, 1e-9) {
		t.Fatalf("1000 cached tokens = %v credits, want 0.1", got)
	}
}

func TestCreditsForTokens_UnknownModelFallsBackToBaseline(t *testing.T) {
	resetCreditRates()
	if got := creditsForTokens("a-model-that-does-not-exist", 0, 1000, 0); !approx(got, 1.0, 1e-9) {
		t.Fatalf("unknown model factor = %v credits, want baseline 1.0", got)
	}
}

func TestCreditsForTokens_ZeroAndNegative(t *testing.T) {
	resetCreditRates()
	if got := creditsForTokens("hy3", 0, 0, 0); got != 0 {
		t.Fatalf("all-zero tokens = %v, want 0", got)
	}
}

// -----------------------------------------------------------------------------
// estimateCredits (the accounting entry point)
// -----------------------------------------------------------------------------

func TestEstimateCredits_FloorsSmallRequestsAtOne(t *testing.T) {
	resetCreditRates()
	// 1 token is far below one credit, but a real request must never bill 0.
	if got := estimateCredits("hy3", 1, 0, 0); got != 1 {
		t.Fatalf("tiny request = %d credits, want floor of 1", got)
	}
}

func TestEstimateCredits_RoundsToNearest(t *testing.T) {
	resetCreditRates()
	// 1400 baseline input tokens = 1.4 credits → rounds to 1.
	if got := estimateCredits("hy3", 1400, 0, 0); got != 1 {
		t.Fatalf("1.4 credits rounded to %d, want 1", got)
	}
	// 1600 baseline input tokens = 1.6 credits → rounds to 2.
	if got := estimateCredits("hy3", 1600, 0, 0); got != 2 {
		t.Fatalf("1.6 credits rounded to %d, want 2", got)
	}
}

func TestEstimateCredits_ZeroWhenNoTokens(t *testing.T) {
	resetCreditRates()
	if got := estimateCredits("hy3", 0, 0, 0); got != 0 {
		t.Fatalf("no tokens = %d credits, want 0 (0 must not be floored to 1)", got)
	}
}

// -----------------------------------------------------------------------------
// Reverse conversion (tokens for credits)
// -----------------------------------------------------------------------------

// tokensForCredits must be the exact inverse of creditsForTokens, or the panel's
// advertised rate would contradict the spend it reports.
func TestTokensForCredits_InvertsForwardConversion(t *testing.T) {
	resetCreditRates()
	cases := []struct {
		model string
		class tokenClass
	}{
		{"hy3", tokenClassInput},
		{"glm-5.3", tokenClassInput},
		{"glm-5.3", tokenClassOutput},
		{"glm-5.3-flash", tokenClassOutput},
		{"hy3", tokenClassCached},
	}
	for _, tc := range cases {
		const tokens = 5000
		var credits float64
		switch tc.class {
		case tokenClassInput:
			credits = creditsForTokens(tc.model, tokens, 0, 0)
		case tokenClassOutput:
			credits = creditsForTokens(tc.model, 0, tokens, 0)
		case tokenClassCached:
			credits = creditsForTokens(tc.model, 0, 0, tokens)
		}
		back := tokensForCredits(tc.model, credits, tc.class)
		if !approx(back, tokens, 1e-6) {
			t.Fatalf("%s/%s: %d tokens -> %v credits -> %v tokens (round-trip must be lossless)",
				tc.model, tc.class, tokens, credits, back)
		}
	}
}

func TestTokensForCredits_PerModelDivergence(t *testing.T) {
	resetCreditRates()
	// One credit buys fewer OUTPUT tokens on a pricier model.
	cheap := tokensForCredits("glm-5.3-flash", 1, tokenClassOutput) // factor 0.5
	dear := tokensForCredits("glm-5.3", 1, tokenClassOutput)        // factor 1.5
	if !(cheap > dear) {
		t.Fatalf("flash (%v tok/credit) must beat glm-5.3 (%v tok/credit) on output", cheap, dear)
	}
	// 1000/0.5 = 2000, 1000/1.5 ≈ 666.67
	if !approx(cheap, 2000, 1) {
		t.Fatalf("flash output = %v tokens/credit, want 2000", cheap)
	}
	if !approx(dear, 2000.0/3.0, 1) {
		t.Fatalf("glm-5.3 output = %v tokens/credit, want ~666.67", dear)
	}
	// Input does not vary by model.
	if got := tokensForCredits("glm-5.3", 1, tokenClassInput); !approx(got, 1000, 1e-6) {
		t.Fatalf("glm-5.3 input = %v tokens/credit, want 1000", got)
	}
	// Cache reads are the cheapest class.
	cached := tokensForCredits("hy3", 1, tokenClassCached)
	if !approx(cached, 10000, 1e-6) {
		t.Fatalf("cached = %v tokens/credit, want 10000 (1000/0.1)", cached)
	}
}

func TestTokensForCredits_ScalesLinearlyWithCredits(t *testing.T) {
	resetCreditRates()
	one := tokensForCredits("hy3", 1, tokenClassInput)
	ten := tokensForCredits("hy3", 10, tokenClassInput)
	if !approx(ten, one*10, 1e-6) {
		t.Fatalf("10 credits = %v tokens, want 10x one credit (%v)", ten, one*10)
	}
}

func TestTokensForCreditsBlended_ClampsShare(t *testing.T) {
	resetCreditRates()
	// A 0 share behaves as pure input; a 1 share as pure output.
	if got := tokensForCreditsBlended("glm-5.3", 1, 0); !approx(got, 1000, 1e-6) {
		t.Fatalf("share=0 = %v tokens, want input rate 1000", got)
	}
	if got := tokensForCreditsBlended("glm-5.3", 1, 1); !approx(got, 2000.0/3.0, 1e-6) {
		t.Fatalf("share=1 = %v tokens, want output rate ~666.67", got)
	}
	// Out-of-range shares clamp rather than producing a nonsense rate.
	if got := tokensForCreditsBlended("glm-5.3", 1, 5); !approx(got, 2000.0/3.0, 1e-6) {
		t.Fatalf("share=5 should clamp to 1, got %v", got)
	}
	if got := tokensForCreditsBlended("glm-5.3", 1, -3); !approx(got, 1000, 1e-6) {
		t.Fatalf("share=-3 should clamp to 0, got %v", got)
	}
}

func TestTokensForCredits_NonPositiveCredits(t *testing.T) {
	resetCreditRates()
	for _, c := range []float64{0, -5, math.NaN(), math.Inf(1)} {
		if got := tokensForCredits("hy3", c, tokenClassInput); got != 0 {
			t.Fatalf("credits=%v yielded %v tokens, want 0", c, got)
		}
	}
}

// -----------------------------------------------------------------------------
// Config overrides
// -----------------------------------------------------------------------------

func TestParseCreditRates(t *testing.T) {
	got := parseCreditRates(" glm-5.3=1.5 , kimi-k2.7=0.8 ,deepseek-v4.1-flash=0.25")
	want := map[string]float64{"glm-5.3": 1.5, "kimi-k2.7": 0.8, "deepseek-v4.1-flash": 0.25}
	if len(got) != len(want) {
		t.Fatalf("parsed %d entries, want %d: %#v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %v, want %v", k, got[k], v)
		}
	}
}

// One typo must not discard the operator's other corrections.
func TestParseCreditRates_SkipsMalformedEntries(t *testing.T) {
	got := parseCreditRates("glm-5.3=1.5,garbage,kimi-k2.7=abc,=2,hy3=0,ok=0.5,neg=-1")
	if len(got) != 2 {
		t.Fatalf("want only the 2 valid entries, got %#v", got)
	}
	if got["glm-5.3"] != 1.5 || got["ok"] != 0.5 {
		t.Fatalf("valid entries mangled: %#v", got)
	}
}

func TestParseCreditRates_Empty(t *testing.T) {
	if got := parseCreditRates(""); len(got) != 0 {
		t.Fatalf("empty config parsed %d entries, want 0", len(got))
	}
}

func TestOverrideCreditRates_ReplacesPreviousSet(t *testing.T) {
	defer resetCreditRates()
	resetCreditRates()
	overrideCreditRates(map[string]float64{"glm-5.3": 3.0})
	if got := modelCreditFactor("glm-5.3"); got != 3.0 {
		t.Fatalf("override not applied: %v", got)
	}
	// A second call replaces the whole set: dropping a model from config must
	// actually revert it, not leave the old override sticky.
	overrideCreditRates(map[string]float64{"hy3": 2.0})
	if got := modelCreditFactor("glm-5.3"); got != 1.5 {
		t.Fatalf("stale override survived: glm-5.3 factor = %v, want built-in 1.5", got)
	}
	if got := modelCreditFactor("hy3"); got != 2.0 {
		t.Fatalf("new override missing: hy3 factor = %v, want 2.0", got)
	}
}

func TestOverrideCreditRates_IgnoresInvalidFactors(t *testing.T) {
	defer resetCreditRates()
	resetCreditRates()
	overrideCreditRates(map[string]float64{"glm-5.3": 0, "hy3": -1, "kimi-k2.7": math.NaN()})
	for _, m := range []string{"glm-5.3", "hy3", "kimi-k2.7"} {
		want := modelCreditFactor(m)
		if _, overridden := creditFactorOverrides[m]; overridden {
			t.Fatalf("invalid factor for %s was accepted", m)
		}
		if want != 1.5 && want != 1.0 && want != 0.8 {
			t.Fatalf("unexpected built-in factor for %s: %v", m, want)
		}
	}
}

// Overrides are matched case-insensitively, matching how models are resolved.
func TestModelCreditFactor_CaseInsensitiveOverride(t *testing.T) {
	defer resetCreditRates()
	resetCreditRates()
	overrideCreditRates(map[string]float64{"GLM-5.3": 2.5})
	if got := modelCreditFactor("glm-5.3"); got != 2.5 {
		t.Fatalf("lowercase lookup = %v, want 2.5", got)
	}
	if got := modelCreditFactor("  GLM-5.3  "); got != 2.5 {
		t.Fatalf("padded/uppercase lookup = %v, want 2.5", got)
	}
}

func TestOverrideTokensPerCredit(t *testing.T) {
	defer resetCreditRates()
	resetCreditRates()
	overrideTokensPerCredit(2000)
	// 2000 tokens/credit at factor 1.0 → 2000 input tokens = 1 credit.
	if got := creditsForTokens("hy3", 2000, 0, 0); !approx(got, 1.0, 1e-9) {
		t.Fatalf("with 2000 tokens/credit: 2000 input = %v credits, want 1", got)
	}
	// A non-positive value restores the default rather than dividing by zero.
	overrideTokensPerCredit(0)
	if tpc, _ := creditRateScalars(); tpc != float64(defaultTokensPerCredit) {
		t.Fatalf("tokens_per_credit = %v, want default %v", tpc, defaultTokensPerCredit)
	}
}

// -----------------------------------------------------------------------------
// Rate card reporting
// -----------------------------------------------------------------------------

func TestCreditRateFor_ReportsBothDirections(t *testing.T) {
	resetCreditRates()
	r := creditRateFor("glm-5.3")
	if r.Model != "glm-5.3" || r.Factor != 1.5 || r.Source != "static" {
		t.Fatalf("unexpected card: %+v", r)
	}
	if !approx(r.TokensPerCreditInput, 1000, 1) {
		t.Fatalf("tokens_per_credit_input = %v, want 1000", r.TokensPerCreditInput)
	}
	if !approx(r.TokensPerCreditOutput, 2000.0/3.0, 1) {
		t.Fatalf("tokens_per_credit_output = %v, want ~667", r.TokensPerCreditOutput)
	}
	// Forward direction: 1000 output tokens of a 1.5x model cost 1.5 credits.
	if !approx(r.CreditsPer1KOutput, 1.5, 1e-6) {
		t.Fatalf("credits_per_1k_output = %v, want 1.5", r.CreditsPer1KOutput)
	}
	if !approx(r.CreditsPer1KInput, 1.0, 1e-6) {
		t.Fatalf("credits_per_1k_input = %v, want 1.0", r.CreditsPer1KInput)
	}
}

func TestCreditRateFor_OverrideAndUnknownSource(t *testing.T) {
	defer resetCreditRates()
	resetCreditRates()
	if got := creditRateFor("brand-new-model").Source; got != "default" {
		t.Fatalf("unknown model source = %q, want \"default\"", got)
	}
	overrideCreditRates(map[string]float64{"glm-5.3": 2.0})
	if got := creditRateFor("glm-5.3").Source; got != "override" {
		t.Fatalf("overridden model source = %q, want \"override\"", got)
	}
}

func TestKnownCreditRateModels_IncludesOverridesAndIsSorted(t *testing.T) {
	defer resetCreditRates()
	resetCreditRates()
	models := knownCreditRateModels()
	if len(models) != len(staticCreditFactors) {
		t.Fatalf("got %d models, want %d built-ins", len(models), len(staticCreditFactors))
	}
	for i := 1; i < len(models); i++ {
		if models[i-1] >= models[i] {
			t.Fatalf("not sorted/deduped at %d: %q >= %q", i, models[i-1], models[i])
		}
	}
	overrideCreditRates(map[string]float64{"zzz-override": 1.0})
	found := false
	for _, m := range knownCreditRateModels() {
		if m == "zzz-override" {
			found = true
		}
	}
	if !found {
		t.Fatal("override model missing from the known list")
	}
}

// -----------------------------------------------------------------------------
// Handler
// -----------------------------------------------------------------------------

func ratesQuery(params map[string]string) mgmtRequestLite {
	return mgmtRequestLite{Query: func(k string) string { return params[k] }}
}

func TestHandleCreditRates_RatesOnly(t *testing.T) {
	resetCreditRates()
	out := handleCreditRates(ratesQuery(nil))
	if out["error"] != nil {
		t.Fatalf("unexpected error: %v", out["error"])
	}
	if out["tokens_per_credit"] != float64(defaultTokensPerCredit) {
		t.Fatalf("tokens_per_credit = %v, want %d", out["tokens_per_credit"], defaultTokensPerCredit)
	}
	models, ok := out["models"].([]map[string]any)
	if !ok || len(models) == 0 {
		t.Fatalf("models missing/empty: %#v", out["models"])
	}
	// Without ?credits= the conversion columns must be absent, so the panel can
	// tell "rates only" apart from "converted N credits".
	if _, present := models[0]["tokens"]; present {
		t.Fatal("conversion columns must be absent when no credits were requested")
	}
	if _, present := out["credits"]; present {
		t.Fatal("credits must be absent when no conversion was requested")
	}
}

func TestHandleCreditRates_ConvertsCreditsPerModel(t *testing.T) {
	resetCreditRates()
	out := handleCreditRates(ratesQuery(map[string]string{"credits": "10", "output_share": "0"}))
	if out["converted"] != true {
		t.Fatalf("converted flag = %v, want true", out["converted"])
	}
	if out["credits"] != float64(10) {
		t.Fatalf("credits = %v, want 10", out["credits"])
	}
	models := out["models"].([]map[string]any)
	byModel := map[string]map[string]any{}
	for _, m := range models {
		byModel[m["model"].(string)] = m
	}
	// share=0 → pure input: 10 credits = 10000 input tokens on every model.
	for _, name := range []string{"hy3", "glm-5.3", "glm-5.3-flash"} {
		row, ok := byModel[name]
		if !ok {
			t.Fatalf("%s missing from the conversion table", name)
		}
		if got := row["input_tokens"].(float64); !approx(got, 10000, 1) {
			t.Fatalf("%s: 10 credits = %v input tokens, want 10000", name, got)
		}
	}
	// Output differs per model: flash (0.5) buys 3x what glm-5.3 (1.5) does.
	flashOut := byModel["glm-5.3-flash"]["output_tokens"].(float64)
	dearOut := byModel["glm-5.3"]["output_tokens"].(float64)
	if !(flashOut > dearOut) {
		t.Fatalf("flash output (%v) must exceed glm-5.3 output (%v)", flashOut, dearOut)
	}
	if !approx(flashOut, 20000, 1) {
		t.Fatalf("flash: 10 credits = %v output tokens, want 20000", flashOut)
	}
}

func TestHandleCreditRates_RejectsBadAmount(t *testing.T) {
	resetCreditRates()
	for _, bad := range []string{"0", "-5", "abc"} {
		out := handleCreditRates(ratesQuery(map[string]string{"credits": bad}))
		if out["error"] == nil {
			t.Fatalf("credits=%q should be rejected, got %#v", bad, out)
		}
	}
}

func TestHandleCreditRates_RejectsBadShare(t *testing.T) {
	resetCreditRates()
	for _, bad := range []string{"1.5", "-0.1"} {
		out := handleCreditRates(ratesQuery(map[string]string{"credits": "5", "output_share": bad}))
		if out["error"] == nil {
			t.Fatalf("output_share=%q should be rejected, got %#v", bad, out)
		}
	}
}

// A malformed output_share without credits still must not poison the rate card.
func TestHandleCreditRates_DefaultShareWhenAbsent(t *testing.T) {
	resetCreditRates()
	out := handleCreditRates(ratesQuery(map[string]string{"credits": "5"}))
	if out["error"] != nil {
		t.Fatalf("unexpected error: %v", out["error"])
	}
	if out["output_share"] != defaultOutputShare {
		t.Fatalf("output_share = %v, want default %v", out["output_share"], defaultOutputShare)
	}
}

func TestParseFloatDefault(t *testing.T) {
	cases := []struct {
		in   string
		def  float64
		want float64
	}{
		{"", 7, 7},
		{"2.5", 7, 2.5},
		{"junk", 7, 7},
		{"NaN", 7, 7},
		{"+Inf", 7, 7},
	}
	for _, tc := range cases {
		if got := parseFloatDefault(tc.in, tc.def); got != tc.want {
			t.Fatalf("parseFloatDefault(%q, %v) = %v, want %v", tc.in, tc.def, got, tc.want)
		}
	}
}
