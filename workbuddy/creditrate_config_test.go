package main

import (
	"encoding/json"
	"testing"
)

// configWith builds the plugin.register-shaped request body configure() reads.
func configWith(yaml string) []byte {
	raw, _ := json.Marshal(struct {
		ConfigYAML []byte `json:"config_yaml"`
	}{ConfigYAML: []byte(yaml)})
	return raw
}

// isolateConfigure keeps configure() off the network. With no usage_report_url
// in config and no env var, resolveUsageReport() probes two candidate URLs with
// a 2s timeout each — that would add ~4s per call to the suite. An explicit env
// URL short-circuits the probe (and nothing is ever sent: the key stays empty).
func isolateConfigure(t *testing.T) {
	t.Helper()
	t.Setenv("USAGE_REPORT_URL", "http://127.0.0.1:1/never-probed")
}

// The credit rate card is configured through config_yaml, so the new keys must
// survive configure()'s line-based parser. A typo there would silently leave the
// built-in table in place while the operator believes their override is live.
func TestConfigure_CreditRateKeys(t *testing.T) {
	// configure() calls ensureScheduler(), which starts a goroutine; it is
	// idempotent, so the first call wins for the whole test binary.
	defer resetCreditRates()
	isolateConfigure(t)

	configure(configWith(`
enabled: true
checkin_auto: true
tokens_per_credit: 2000
credit_rates: "glm-5.3=2.5,kimi-k2.7=0.4"
`))

	if tpc, _ := creditRateScalars(); tpc != 2000 {
		t.Errorf("tokens_per_credit = %v, want 2000", tpc)
	}
	if got := modelCreditFactor("glm-5.3"); got != 2.5 {
		t.Errorf("glm-5.3 factor = %v, want 2.5 from config", got)
	}
	if got := modelCreditFactor("kimi-k2.7"); got != 0.4 {
		t.Errorf("kimi-k2.7 factor = %v, want 0.4 from config", got)
	}
	// A model absent from credit_rates keeps its built-in factor.
	if got := modelCreditFactor("deepseek-v4.1-flash"); got != 0.5 {
		t.Errorf("unlisted model factor = %v, want built-in 0.5", got)
	}
}

// Reconfiguring without the keys must NOT wipe the previous overrides: the host
// re-sends config_yaml on every reload, and a partial reload should not silently
// revert the operator's rates to the built-in table.
func TestConfigure_CreditRatesSurviveUnrelatedReconfigure(t *testing.T) {
	defer resetCreditRates()
	isolateConfigure(t)
	configure(configWith("tokens_per_credit: 3000\ncredit_rates: \"glm-5.3=9.0\"\n"))
	if got := modelCreditFactor("glm-5.3"); got != 9.0 {
		t.Fatalf("setup failed: glm-5.3 factor = %v", got)
	}

	// A later configure that omits the credit keys (e.g. only toggling checkin).
	configure(configWith("checkin_auto: false\n"))

	if got := modelCreditFactor("glm-5.3"); got != 9.0 {
		t.Errorf("override lost on unrelated reconfigure: glm-5.3 factor = %v, want 9.0", got)
	}
	if tpc, _ := creditRateScalars(); tpc != 3000 {
		t.Errorf("tokens_per_credit lost on unrelated reconfigure: %v, want 3000", tpc)
	}
}

// Declaring the key with no models must actually clear the overrides — that is
// how an operator reverts to the built-in table without a restart.
func TestConfigure_EmptyCreditRatesRevertsOverrides(t *testing.T) {
	defer resetCreditRates()
	isolateConfigure(t)
	configure(configWith("credit_rates: \"glm-5.3=9.0\"\n"))
	if got := modelCreditFactor("glm-5.3"); got != 9.0 {
		t.Fatalf("setup failed: glm-5.3 factor = %v", got)
	}

	configure(configWith("credit_rates: \"\"\n"))
	if got := modelCreditFactor("glm-5.3"); got != 1.5 {
		t.Errorf("empty credit_rates must revert to the built-in 1.5, got %v", got)
	}
}

// Quoted and unquoted YAML values are both common; both must parse.
func TestConfigure_CreditRatesQuotingVariants(t *testing.T) {
	defer resetCreditRates()
	isolateConfigure(t)
	configure(configWith("credit_rates: 'hy3=1.25,glm-5.2=0.75'\n"))
	if got := modelCreditFactor("hy3"); got != 1.25 {
		t.Errorf("single-quoted value: hy3 factor = %v, want 1.25", got)
	}
	configure(configWith("credit_rates: \"glm-5v-turbo=1.75\"\n"))
	if got := modelCreditFactor("glm-5v-turbo"); got != 1.75 {
		t.Errorf("double-quoted value: glm-5v-turbo factor = %v, want 1.75", got)
	}
	configure(configWith("credit_rates: minimax-m3=2.25\n"))
	if got := modelCreditFactor("minimax-m3"); got != 2.25 {
		t.Errorf("unquoted value: minimax-m3 factor = %v, want 2.25", got)
	}
}
