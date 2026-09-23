#!/usr/bin/env python3
"""Headless check of panel.html's credits<->tokens rate rendering.

Extracts the panel's real <script> block, stubs the minimal DOM/browser surface,
feeds it a payload shaped exactly like the backend's GET /creditlog/rates
response, and drives the new functions. This catches client/server field-name
drift that a JS syntax check cannot: if the backend renames a key, the table
silently renders "-" and this fails.

Usage: python3 scripts/check_panel_rates.py
"""

from __future__ import annotations

import json
import os
import pathlib
import re
import subprocess
import sys
import tempfile

REPO = pathlib.Path(__file__).resolve().parent.parent
PANEL = REPO / "workbuddy" / "panel.html"

# A faithful GET /creditlog/rates?credits=10&output_share=0 response, matching
# creditRatesReport()'s exact JSON keys.
RATES_WITH_CREDITS = {
    "tokens_per_credit": 1000.0,
    "cached_factor": 0.1,
    "output_share": 0.0,
    "credits": 10,
    "converted": True,
    "models": [
        {
            "model": "glm-5.3", "factor": 1.5, "source": "static",
            "tokens_per_credit_input": 1000, "tokens_per_credit_output": 667,
            "tokens_per_credit_cached": 10000,
            "credits_per_1k_input": 1.0, "credits_per_1k_output": 1.5,
            "credits_per_1k_cached": 0.1,
            "tokens": 10000, "input_tokens": 10000, "output_tokens": 6667,
            "cached_tokens": 100000,
        },
        {
            "model": "glm-5.3-flash", "factor": 0.5, "source": "override",
            "tokens_per_credit_input": 1000, "tokens_per_credit_output": 2000,
            "tokens_per_credit_cached": 10000,
            "credits_per_1k_input": 1.0, "credits_per_1k_output": 0.5,
            "credits_per_1k_cached": 0.1,
            "tokens": 10000, "input_tokens": 10000, "output_tokens": 20000,
            "cached_tokens": 100000,
        },
    ],
    "note": "估算值",
}

# Rates-only response (no ?credits=).
RATES_ONLY = dict(RATES_WITH_CREDITS)
RATES_ONLY = {k: v for k, v in RATES_WITH_CREDITS.items()
              if k not in ("credits", "converted")}
RATES_ONLY["models"] = [
    {k: v for k, v in m.items()
     if k not in ("tokens", "input_tokens", "output_tokens", "cached_tokens")}
    for m in RATES_WITH_CREDITS["models"]
]

STUBS = r"""
// --- minimal browser surface -------------------------------------------------
// The panel's <script> ends with an auto-init block (keyInput listener +
// load()/showAuth()), so the stub must tolerate real DOM calls rather than
// just the handful the rate functions use. Element lookups return a permissive
// fake: every property read yields something usable, every method is a no-op.
function fakeEl() {
  const el = {
    style: {}, dataset: {}, value: "", textContent: "", innerHTML: "",
    disabled: false, checked: false, children: [], firstChild: null,
    classList: { add() {}, remove() {}, contains: () => false, toggle() {} },
    addEventListener() {}, removeEventListener() {}, focus() {}, blur() {},
    appendChild() {}, removeChild() {}, remove() {}, replaceWith() {},
    querySelector: () => null, querySelectorAll: () => [], closest: () => null,
    setAttribute() {}, removeAttribute() {}, getAttribute: () => null,
    insertBefore() {}, scrollIntoView() {}, contains: () => false,
  };
  return el;
}
const fakeStorage = {
  getItem: () => null, setItem() {}, removeItem() {}, clear() {},
};
globalThis.document = {
  getElementById: () => fakeEl(),
  querySelector: () => null,
  querySelectorAll: () => [],
  createElement: () => fakeEl(),
  addEventListener() {}, removeEventListener() {},
  documentElement: fakeEl(),
  body: fakeEl(),
  cookie: "",
};
globalThis.window = globalThis;
globalThis.localStorage = fakeStorage;
globalThis.sessionStorage = fakeStorage;
// Node >=21 defines navigator (and location in some builds) as getter-only
// globals; redefine rather than assign so the stub always wins.
function defineGlobal(name, value) {
  try {
    Object.defineProperty(globalThis, name, {
      value, writable: true, configurable: true, enumerable: false,
    });
  } catch (e) {
    /* leave the host's own global in place */
  }
}
// The panel's <script> opens with `const MANAGEMENT_BASE_PATH=__WB_MANAGEMENT_BASE_PATH_JSON__;`
// — a placeholder the Go side substitutes at serve time (panel.go). Supply a
// literal here so the extracted script parses as-is.
defineGlobal("__WB_MANAGEMENT_BASE_PATH_JSON__", "/v0/management");
defineGlobal("navigator", { userAgent: "node-check", language: "zh-CN" });
// captureUrlKey() reads window.location.href to pick up a ?key= bootstrap
// query, so href must be a parseable absolute URL (new URL() rejects undefined).
defineGlobal("location", {
  href: "http://127.0.0.1:43120/panel",
  host: "127.0.0.1:43120",
  pathname: "/panel",
  search: "",
});
defineGlobal("history", { replaceState() {} });
globalThis.CSS = { escape: (s) => String(s) };
globalThis.matchMedia = () => ({ matches: false, addEventListener() {}, addListener() {} });
globalThis.requestAnimationFrame = (fn) => setTimeout(fn, 0);
globalThis.MutationObserver = function () { this.observe = () => {}; };
globalThis.fetch = async () => ({ status: 200, ok: true, json: async () => ({}) });
globalThis.TextEncoder = TextEncoder;
globalThis.TextDecoder = TextDecoder;
globalThis.toast = () => {};
globalThis.fmtTok = function (n) {
  n = Number(n) || 0;
  if (n < 0) return "0";
  if (n < 999500) {
    if (n < 1000) return String(Math.round(n));
    return (n / 1000).toFixed(1).replace(/\.0$/, "") + "k";
  }
  return (n / 1000000).toFixed(2).replace(/\.?0+$/, "") + "M";
};
globalThis.esc = function (s) {
  return (s || "").replace(/[&<>"']/g, function (c) {
    return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
  });
};
globalThis.api = async () => ({});
globalThis.busy = () => {};
"""

DRIVER = r"""
// --- drive the new rate-rendering code ---------------------------------------
const out = {};
const checks = (name, val) => { out[name] = !!val; };
// The panel's own source, for assertions about call sites (not just behaviour).
const panelSource = __PANEL_SOURCE__;

rateData = __RATES_WITH__;
checks("rateFor_exact", rateFor("glm-5.3"));
checks("rateFor_case_insensitive", rateFor("GLM-5.3"));
checks("rateFor_unknown_is_null", rateFor("no-such-model") === null);

checks("fmtTokRate_whole", fmtTokRate(1000) === "1k");
// >= 100 renders as a whole number: the backend already rounds
// tokens-per-credit to integers, and extra precision on an approximation is
// noise. Below 100 one decimal is kept, so cheap models stay distinguishable.
checks("fmtTokRate_rounds_large_to_whole", fmtTokRate(666.67) === "667");
checks("fmtTokRate_keeps_decimal_below_100", fmtTokRate(66.67) === "66.7");
checks("fmtTokRate_zero_is_dash", fmtTokRate(0) === "-");
checks("fmtTokRate_garbage_is_dash", fmtTokRate(NaN) === "-");

const t0 = rateTableHTML(0);
checks("table_rates_only_caption", t0.includes("1 积分 ≈"));
checks("table_rates_only_has_model", t0.includes("glm-5.3-flash"));
checks("table_rates_only_marks_override", t0.includes("\u2731"));
checks("table_rates_only_shows_factor", t0.includes("1.5") && t0.includes("0.5"));
checks("table_rates_only_shows_per_credit_output",
       t0.includes(fmtTokRate(667)) && t0.includes(fmtTokRate(2000)));

const t10 = rateTableHTML(10);
checks("table_with_credits_caption", t10.includes("10 积分 ≈"));
checks("table_with_credits_uses_token_columns",
       t10.includes(fmtTok(20000)) && t10.includes(fmtTok(6667)));

checks("calc_has_inputs", rateCalcHTML(0, null).includes("rcCredits"));
checks("calc_has_share_input", rateCalcHTML(0, null).includes("rcShare"));
checks("calc_prefills_credits", rateCalcHTML(25, null).includes('value="25"'));

// Rates-only payload must not crash the table either.
rateData = __RATES_ONLY__;
checks("table_survives_rates_only_payload", rateTableHTML(0).includes("1 积分 ≈"));

// An empty/absent rate card must degrade to nothing, not throw.
rateData = null;
checks("table_empty_when_no_rates", rateTableHTML(0) === "");
checks("rateFor_null_safe", rateFor("glm-5.3") === null);

// --- cache-hit rate ----------------------------------------------------------
// cacheRate(reads, input). The upstream's prompt count is cache-INCLUSIVE, so
// reads are a subset of the denominator and the write counter must stay out of
// it. Regression guard: the rate was once computed over
// (input + reads + writes), which double-counted the reads and read low
// whenever a cache write occurred.
checks("cacheRate_live_fixture", Math.abs(cacheRate(4043, 4443) - 91.0) < 0.15);
checks("cacheRate_no_reads_is_zero", cacheRate(0, 1000) === 0);
checks("cacheRate_half", cacheRate(500, 1000) === 50);
checks("cacheRate_zero_input_is_zero", cacheRate(10, 0) === 0);
checks("cacheRate_all_cached", cacheRate(1000, 1000) === 100);
// The write counter is no longer a parameter — pin the arity and the call site
// so a stale three-argument call cannot creep back in.
checks("cacheRate_arity_is_two", cacheRate.length === 2);
checks("cacheRate_call_site_two_args", /cacheRate\(read,\s*input\)/.test(panelSource));
checks("cacheRate_call_site_no_write_arg", !/cacheRate\(read,\s*write/.test(panelSource));

console.log(JSON.stringify(out));
"""


def main() -> int:
    html = PANEL.read_text(encoding="utf-8")
    blocks = re.findall(r"<script>(.*?)</script>", html, re.DOTALL)
    if not blocks:
        raise SystemExit("no <script> block found in panel.html")
    panel_js = blocks[-1]

    driver = (
        DRIVER.replace("__RATES_WITH__", json.dumps(RATES_WITH_CREDITS))
        .replace("__RATES_ONLY__", json.dumps(RATES_ONLY))
        # panelSource lets the driver assert on call sites (e.g. that cacheRate
        # is invoked with two arguments), which runtime probing alone cannot see.
        .replace("__PANEL_SOURCE__", json.dumps(panel_js))
    )
    harness = "\n".join([STUBS, panel_js, driver])

    with tempfile.NamedTemporaryFile("w", suffix=".mjs", delete=False,
                                     encoding="utf-8") as fh:
        fh.write(harness)
        path = fh.name
    try:
        proc = subprocess.run(["node", path], capture_output=True, text=True,
                              encoding="utf-8")
    finally:
        os.unlink(path)

    if proc.returncode != 0:
        print("node failed:")
        print(proc.stderr or proc.stdout)
        return 1

    results = json.loads(proc.stdout.strip().splitlines()[-1])
    failed = [k for k, v in results.items() if not v]
    for k, v in results.items():
        print(f"  {'ok  ' if v else 'FAIL'} {k}")
    if failed:
        print(f"\n{len(failed)} check(s) failed: {', '.join(failed)}")
        return 1
    print(f"\nall {len(results)} panel rate checks passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
