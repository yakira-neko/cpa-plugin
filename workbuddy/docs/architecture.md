# WorkBuddy Plugin Architecture

Module map and data flow for the workbuddy plugin. The plugin is a single
`package main` compiled as a c-shared `.so`, loaded by CPA at startup and
driven via the `pluginabi` RPC interface.

## Capability surface (declared in `wbRegistration`)

| Capability | Implementation file | What it does |
|---|---|---|
| `ModelProvider` | `models.go` | Static + dynamic model list, alias reverse-resolution, `oauth-excluded-models` filter |
| `AuthProvider` | `oauth.go`, `auth_parse.go` (in `authfile.go` / `main.go`) | OAuth login flow (CN + Global, region-routed), token refresh, auth file parse |
| `Executor` | `executor.go`, `stream.go`, `payload.go` | Chat completions, streaming SSE pump, request body rewriting |
| `Scheduler` | `scheduler.go`, `active_auth.go` | Optional panel-selected account routing (`scheduler_mode: credits`) |
| `ManagementAPI` | `management.go`, `panel.go`, `checkin.go`, `credits_handler.go`, `billing.go`, `usage_config.go`, `host_auth.go` | Dashboard, manual check-in, credits query, import credential, config |
| `UsagePlugin` | `usage.go`, `creditlog.go`, `creditrate.go` | Forward every request's usage record to CPAMP; estimate per-request credits and expose the per-model credits ↔ tokens rate card |

## File map (by responsibility)

```
main.go           C ABI exports + handleMethod dispatch + registration
registration.go   (in main.go) wbRegistration + capabilities + ConfigField
envelope.go       (in main.go) envelope/okEnvelope/errorEnvelope helpers

host_call.go      hostCall + hostBridgeUnwrap (RPC to CPA host)
host_bridge.go    hostHTTPDo/DoStream/Read/Close + hostStreamReader + Direct fallbacks

executor.go       handleExecExecute / handleExecStream
stream.go         streamEmit/Close + pumpUpstreamStream + collectUpstreamStream + aggregate*
payload.go        prepareUpstreamBody + InPlace mutators (forceStream/normalizeTools/
                  rewriteSystem/ensureSystemMessage/rewriteModel) + legacy wrappers

models.go         callModelsAPI + fetchDynamicModels + cacheModelAliases +
                  resolveUpstreamModel + parseModelAliasAttribute + filterExcludedModels

oauth.go          startLoginFlow/handleStartLogin/PollLogin/RefreshAuth +
                  newLoginClient + doJSON
                  Region routing lives in main.go: Region type +
                  normalizeRegion + baseURL/authStateURL/authTokenURL/
                  loginAcctURL + regionHeaders + domainForRegion
auth_parse.go     (in authfile.go / main.go) handleParseAuth + parseStored + toAuthData

usage.go          handleUsage + publishUsage + forwardUsageToCPAMP + sseUsageCollector

management.go     managementRegistration + handleManagement + auth/ratelimit
panel.go          buildDashboardEx + summarizeCredits + servePanel + panelHTML
checkin.go        schedulerLoop + runAutoCheckin + handleManualCheckin + 
                  classifyCheckinTargets/executeCheckinBatch/summarizeCheckinResults
credits_handler.go handleImportAuth/CheckinConfig/ClaimTrial/SelectAuth/CreditsQuery +
                   LoginStart/LoginPoll/LoginConfig (panel-driven CN/Global login)
billing.go        fetchCheckinStatus/fetchUserResource/fetchPaymentType/
                  performCheckinCall/performTrialCall + JSON helpers
usage_config.go   configure + resolveUsageReport + probe* + config vars
host_auth.go      hostAuthList/Get/GetBundle (host auth-store RPC)

lifecycle.go      reconcileOneAccount/AllAccounts/AfterExecutorError/ByUID +
                  applyExhaustedPolicy + lifecycleState
policy.go         lifecycleAction decisions (pure functions) + displayNote + labelForAuth
authfile.go       authFileNameFor/sanitizeUIDForFileName/hostAuthPersist/deleteAuth +
                  path safety checks

scheduler.go      handleSchedulerPick + candidateDisabled + cachedCreditsScore
active_auth.go    activeAuthID sticky state + pickActiveAuth + clearActiveAuthIfMatch

cache.go          accountCache + accountDetailFlight singleflight + prune
creditrate.go     Per-model credits <-> tokens rate card: modelCreditFactor +
                  creditsForTokens (forward, single source of truth for spend) +
                  tokensForCredits/Blended (exact inverse) + overrideTokensPerCredit
                  + parseCreditRates + creditRatesReport
creditlog.go      recordCreditUsage + ring buffer + creditSnapshot/creditGlobalSummary
                  + handleCreditLogQuery/Summary/Rates + janitor
redact.go         redactSecrets + 4 regex + truncateRedacted + truncate
headers.go        (in main.go / oauth.go) commonHeaders/backendHeaders/billingHeaders
stored.go         (in main.go / models.go) storedAuth/storedTokens/storedAccount
```

## Data flow

### Chat completion (streaming)

```
client → CPA → plugin.handleExecStream
  → parseStored(auth file)
  → resolveUpstreamModel(alias → upstream id)
  → prepareUpstreamBody (single JSON pass: forceStream + normalizeTools +
                          rewriteSystem + ensureSystemMessage + rewriteModel)
  → hostHTTPDoStream (via CPA host bridge → request-log captured)
  → pumpUpstreamStream (goroutine)
      → hostStreamReader → bufio.Scanner → SSE lines
      → cleanChunkJSON per line
      → streamEmit → CPA → client
      → sseUsageCollector collects terminal usage object
  → publishUsage → forwardUsageToCPAMP (async, via host bridge)
  → invalidateAccountCredits (async)
  → host calls UsagePlugin.HandleUsage → handleUsage → forwardUsageToCPAMP (sync)
```

### Daily check-in (CN, 09:00 / 21:00)

```
schedulerLoop → runAutoCheckin (sem=4 concurrent)
  → processAutoCheckinAccount per account
      → fetchCheckinStatus → performCheckinCall if needed
      → update accountCache (merge, not wipe)
      → reconcileOneAccount → applyExhaustedPolicy
          → policy.go decides: disable (CN) / delete (Global) / reenable (CN)
          → authfile.go applies: hostAuthPersist / deleteAuth
```

### Login (CN / Global)

Two entry points, one implementation:

```
panel.html "登录账号" (region chosen per click)
  → POST /v0/management/plugins/workbuddy/login/start {region}
      → handleLoginStart → startLoginFlow(region)
          → POST <region base>/v2/plugin/auth/state?platform=CLI
          → loginStates[state] = {client, region, expires}
      ← {url, state, expires_at}
  → browser completes login on that edition's page
  → POST .../login/poll {state}   (panel loops every 2s)
      → handleLoginPoll → handlePollLogin  (follows the stored region)
          → GET <region base>/v2/plugin/auth/token?state=…   (pending until done)
          → GET <region base>/v2/plugin/login/account?state=… (needs bearer)
          → domainForRegion(tok.domain, region) → storedAuth
      → host.auth.save "workbuddy-<uid>.json"
      ← {status: success, region, uid, nickname, domain}

CPA built-in "add auth" card (no region selector)
  → auth.login.start → handleStartLogin → regionFromStartRequest
      = request region hint ?? default_region config ?? cn
```

### Dashboard load

```
panel.html → /v0/management/plugins/workbuddy/accounts
  → handleManagement (auth + ratelimit)
  → buildDashboardEx (concurrent cachedAccountDetails per account, sem=4)
      → accountDetailFlight singleflight dedups concurrent fetches
      → accountCache hit → return cached
      → miss → 3 concurrent billing API calls (plan/checkin/credits)
  → summarizeCredits
```

## Key design decisions

1. **Host HTTP bridge for all upstream calls.** Every HTTP request to
   CodeBuddy / CPAMP goes through `host.http.do` / `host.http.do_stream` so
   CPA's request-log captures outbound traffic and host transport policy
   (proxy, timeout) applies. The plugin's own `sharedHTTPClient` is a
   fallback used only when the bridge is unavailable (unit tests, hosts
   older than v7.2.x).

2. **Single-flight per account for billing API.** `cachedAccountDetails`
   uses a `sync.Map` of in-flight calls so concurrent dashboard refreshes
   and reconcile ticks for the same account share one upstream fetch
   instead of stampeding the billing API.

3. **Cache merge, never wipe.** All cache writes merge with the previous
   entry (credits + plan + checkin) instead of replacing it. The "early
   already checked in" fast path used to wipe credits/plan; v0.6.31 fixed
   that by always merging.

4. **UID whitelist for auth file names.** `sanitizeUIDForFileName` strips
   any character outside `[a-zA-Z0-9_-]` and caps length at 64, preventing
   path traversal when importing credentials with attacker-controlled UIDs.

5. **Plugin-layer management auth is opt-in.** When `management_key` is
   unset the plugin defers entirely to CPA's management middleware
   (historical default). When set, mutating endpoints require a constant-time
   Bearer match plus a per-IP token bucket.

6. **Scheduler defers by default.** `scheduler_mode: off` (default) makes
   `handleSchedulerPick` always return `Handled: false` so CPA's built-in
   scheduler picks accounts. The plugin only routes when the operator
   explicitly opts in with `scheduler_mode: credits`.

7. **No goroutine leaks across hot-reload.** The scheduler loop uses a
   `schedulerStop` channel and is idempotent. The plugin's `Shutdown` is a
   deliberate no-op because c-shared runtime teardown races with Go sync
   primitives (SIGSEGV) — `dlclose` cleans up the whole runtime anyway.

8. **Region is a first-class concept, resolved at login time.** The panel picks
   CN or Global per sign-in; a poll always targets the gateway that issued the
   state; and the resulting credential's `domain` is what every later request
   keys off (`upstreamBaseFor`, `billingBaseFor`, `originRefererFor`,
   `accountRegion`). Three details matter:
   - `regionHeaders` must be used for every region-scoped call. `doJSON` falls
     back to CN headers, and the Global gateway rejects a `codebuddy.cn` Origin
     exactly like it rejects a CN JWT.
   - `domainForRegion` stamps a Global credential when upstream omits `domain`,
     so an empty field can never silently produce a CN-classified Global
     account (which would misroute chat/billing and change the exhaust policy
     from *delete* to *disable*).
   - The plugin does **not** rely on the two backends sharing login-state
     storage (measured, but undocumented) — see decision 9.

9. **The panel owns region choice, not the host card.** CPA's auth card calls
   `auth.login.start` without a region selector and the SDK offers no
   "hide/annotate card" hook (`AuthLoginStartRequest` carries only
   Provider/BaseURL/Host/HTTPClient/Metadata). Rather than fork host behaviour,
   the plugin exposes its own `/login/start` + `/login/poll` routes that the
   panel drives, and honours `default_region` for the host card.

10. **One rate card answers both directions.** CodeBuddy never reports credits
    per request, so spend is estimated from tokens. Rather than let the estimate
    and the panel's advertised "1 credit ≈ N tokens" drift apart as two separate
    formulas, both derive from `creditrate.go`:

    ```
    credits = (uncached_input + output×factor + cached×cache_factor) / tokens_per_credit
    ```

    `tokensForCredits` is the algebraic inverse, and a round-trip test asserts it
    (`TestTokensForCredits_InvertsForwardConversion`). Consequences worth noting:

    - **Input is net of cache reads.** The upstream folds cache hits into
      `prompt_tokens` (live fixture: 4443 = 4043 cached + 400 miss), so pricing
      the raw count charged every hit twice — once at full input rate, once at
      the discounted cache rate. A cache-heavy request used to cost *more* than
      the same volume uncached.
    - **The factor scales output only.** Input is the un-scaled baseline; that
      asymmetry is what makes per-model token rates differ at all.
    - The factors are an approximation (real pricing lives in CodeBuddy's web
      app), so `credit_rates` overrides them per model from `config.yaml`
      without a rebuild, and overrides replace the whole set each configure so
      deleting a line genuinely reverts it.

## Integration points with CPA

- **Auth store**: `host.auth.list` / `host.auth.get` / `host.auth.save` —
  plugin never writes auth files directly to disk, always via host RPC.
- **Model registration**: `model.static` / `model.for_auth` RPC, plus
  `oauth-model-alias` / `oauth-excluded-models` from host config.
- **Streaming**: `host.stream.emit` / `host.stream.close` — async SSE
  chunks pushed to the client without blocking the executor return.
- **Usage**: `usage.handle` RPC — host calls `UsagePlugin.HandleUsage`
  after every request with a canonical `pluginapi.UsageRecord`.
- **Management**: `management.register` returns routes under
  `/v0/management/plugins/workbuddy/*` and a panel resource under
  `/v0/resource/plugins/workbuddy/panel`.
- **Scheduler**: `scheduler.pick` RPC — plugin returns `Handled: true` with
  an `AuthID` only when `scheduler_mode: credits` and a valid candidate
  exists; otherwise defers.
