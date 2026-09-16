package main

import (
	"math"
	"testing"
)

// The rate card claims both directions describe the same rate. This asserts the
// forward column really is creditsForTokens and the reverse really is
// tokensForCredits — i.e. the card the panel shows is derived from the functions
// the accounting uses, not a parallel formula that could drift.
func TestCreditRateFor_ColumnsMatchTheAccountingFunctions(t *testing.T) {
	resetCreditRates()
	for _, model := range []string{"glm-5.3", "glm-5.3-flash", "hy3", "unknown-model"} {
		r := creditRateFor(model)

		// Forward: 1000 tokens of each class, straight from creditsForTokens.
		if want := round4(creditsForTokens(model, 1000, 0, 0)); r.CreditsPer1KInput != want {
			t.Errorf("%s: credits_per_1k_input=%v, creditsForTokens says %v", model, r.CreditsPer1KInput, want)
		}
		if want := round4(creditsForTokens(model, 0, 1000, 0)); r.CreditsPer1KOutput != want {
			t.Errorf("%s: credits_per_1k_output=%v, creditsForTokens says %v", model, r.CreditsPer1KOutput, want)
		}
		if want := round4(creditsForTokens(model, 0, 0, 1000)); r.CreditsPer1KCached != want {
			t.Errorf("%s: credits_per_1k_cached=%v, creditsForTokens says %v", model, r.CreditsPer1KCached, want)
		}

		// Reverse: the rounded whole-token figure must round-trip within one
		// token of the exact rate (rounding is the only permitted loss).
		for class, got := range map[tokenClass]float64{
			tokenClassInput:  r.TokensPerCreditInput,
			tokenClassOutput: r.TokensPerCreditOutput,
			tokenClassCached: r.TokensPerCreditCached,
		} {
			exact := tokensForCredits(model, 1, class)
			if math.Abs(got-exact) > 0.5 {
				t.Errorf("%s/%s: card says %v tokens/credit, exact rate is %v",
					model, class, got, exact)
			}
		}
	}
}

// The two directions must be mutual inverses: pricing 1000 tokens then asking
// how many tokens those credits buy must return ~1000.
func TestCreditRateFor_DirectionsAreMutuallyInverse(t *testing.T) {
	resetCreditRates()
	for _, model := range []string{"glm-5.3", "kimi-k2.7", "glm-5.3-flash"} {
		r := creditRateFor(model)
		cases := []struct {
			class     tokenClass
			perCredit float64
			credits1K float64
		}{
			{tokenClassInput, r.TokensPerCreditInput, r.CreditsPer1KInput},
			{tokenClassOutput, r.TokensPerCreditOutput, r.CreditsPer1KOutput},
			{tokenClassCached, r.TokensPerCreditCached, r.CreditsPer1KCached},
		}
		for _, c := range cases {
			// 1000 tokens x (tokens/credit) x (credits/token) ~ 1000
			roundTrip := c.perCredit * c.credits1K
			// Tolerance is loose only because both columns are rounded for
			// display; the underlying identities are exact (see the inverse
			// test in creditrate_test.go).
			if math.Abs(roundTrip-1000) > 25 {
				t.Errorf("%s/%s: %v tokens/credit x %v credits/1k tokens = %v, want ~1000",
					model, c.class, c.perCredit, c.credits1K, roundTrip)
			}
		}
	}
}

// A degenerate override must not produce NaN/Inf columns (a divide-by-zero in
// the forward direction would render as "null" in the panel JSON).
func TestCreditRateFor_NeverEmitsNonFiniteColumns(t *testing.T) {
	defer resetCreditRates()
	resetCreditRates()
	// Overrides with non-positive factors are rejected at the door, but the
	// tokens_per_credit scale is a free-form float; push it extreme.
	for _, tpc := range []float64{math.SmallestNonzeroFloat64, 1e9, 0.0001} {
		overrideTokensPerCredit(tpc)
		for _, model := range []string{"glm-5.3", "unknown"} {
			r := creditRateFor(model)
			for name, v := range map[string]float64{
				"tokens_per_credit_input":  r.TokensPerCreditInput,
				"tokens_per_credit_output": r.TokensPerCreditOutput,
				"tokens_per_credit_cached": r.TokensPerCreditCached,
				"credits_per_1k_input":     r.CreditsPer1KInput,
				"credits_per_1k_output":    r.CreditsPer1KOutput,
				"credits_per_1k_cached":    r.CreditsPer1KCached,
			} {
				if math.IsNaN(v) || math.IsInf(v, 0) {
					t.Errorf("tpc=%v model=%s: %s = %v (must be finite)", tpc, model, name, v)
				}
			}
		}
	}
}
