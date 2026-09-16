package main

import (
	"encoding/json"
	"testing"
)

// The panel renders GET /creditlog/rates by field name; a rename on the Go side
// would silently show "-" for every rate instead of failing loudly. These tests
// pin the exact JSON contract the panel's rateFor()/rateTableHTML() consume.
func TestCreditRatesJSONContract(t *testing.T) {
	resetCreditRates()
	// Mirrors the panel's call: 10 credits, pure-input mix.
	out := handleCreditRates(ratesQuery(map[string]string{
		"credits": "10", "output_share": "0",
	}))
	blob, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		TokensPerCredit float64 `json:"tokens_per_credit"`
		CachedFactor    float64 `json:"cached_factor"`
		OutputShare     float64 `json:"output_share"`
		Credits         float64 `json:"credits"`
		Converted       bool    `json:"converted"`
		Note            string  `json:"note"`
		Models          []struct {
			Model                 string  `json:"model"`
			Factor                float64 `json:"factor"`
			Source                string  `json:"source"`
			TokensPerCreditInput  float64 `json:"tokens_per_credit_input"`
			TokensPerCreditOutput float64 `json:"tokens_per_credit_output"`
			TokensPerCreditCached float64 `json:"tokens_per_credit_cached"`
			CreditsPer1KInput     float64 `json:"credits_per_1k_input"`
			CreditsPer1KOutput    float64 `json:"credits_per_1k_output"`
			CreditsPer1KCached    float64 `json:"credits_per_1k_cached"`
			Tokens                float64 `json:"tokens"`
			InputTokens           float64 `json:"input_tokens"`
			OutputTokens          float64 `json:"output_tokens"`
			CachedTokens          float64 `json:"cached_tokens"`
		} `json:"models"`
	}
	if err := json.Unmarshal(blob, &wire); err != nil {
		t.Fatalf("panel-shaped decode failed (field rename?): %v", err)
	}
	if wire.TokensPerCredit != float64(defaultTokensPerCredit) {
		t.Errorf("tokens_per_credit = %v, want %d", wire.TokensPerCredit, defaultTokensPerCredit)
	}
	if !wire.Converted || wire.Credits != 10 {
		t.Errorf("converted=%v credits=%v, want true/10", wire.Converted, wire.Credits)
	}
	if wire.Note == "" {
		t.Error("note missing: the panel prints it under the rate table")
	}
	if len(wire.Models) == 0 {
		t.Fatal("models empty")
	}
	glm := wire.Models[0]
	// Every per-model key the panel reads must be populated (not just present).
	if glm.Model == "" || glm.Source == "" || glm.Factor <= 0 {
		t.Errorf("model identity fields empty: %+v", glm)
	}
	if glm.TokensPerCreditInput <= 0 || glm.TokensPerCreditOutput <= 0 || glm.TokensPerCreditCached <= 0 {
		t.Errorf("tokens-per-credit columns must all be positive: %+v", glm)
	}
	if glm.CreditsPer1KInput <= 0 || glm.CreditsPer1KOutput <= 0 || glm.CreditsPer1KCached <= 0 {
		t.Errorf("credits-per-1k columns must all be positive: %+v", glm)
	}
	// share=0 → pure input, so 10 credits = 10000 tokens on every model.
	if glm.InputTokens != 10000 {
		t.Errorf("input_tokens = %v, want 10000", glm.InputTokens)
	}
	if glm.Tokens != 10000 {
		t.Errorf("blended tokens = %v, want 10000 at share 0", glm.Tokens)
	}
}

// Without ?credits= the conversion columns must be ABSENT, not zero — the panel
// switches captions on their presence.
func TestCreditRatesJSONContract_RatesOnly(t *testing.T) {
	resetCreditRates()
	blob, err := json.Marshal(handleCreditRates(ratesQuery(nil)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		Credits   *float64         `json:"credits"`
		Converted bool             `json:"converted"`
		Models    []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(blob, &wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if wire.Credits != nil || wire.Converted {
		t.Errorf("credits/converted present without ?credits=: %+v", wire)
	}
	for _, m := range wire.Models {
		for _, k := range []string{"tokens", "input_tokens", "output_tokens", "cached_tokens"} {
			if _, present := m[k]; present {
				t.Errorf("model %v carries %q without ?credits=", m["model"], k)
			}
		}
		// The rate columns must still be there.
		if _, present := m["tokens_per_credit_input"]; !present {
			t.Errorf("model %v missing tokens_per_credit_input", m["model"])
		}
	}
}
