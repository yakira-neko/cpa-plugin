# Changelog

## 0.9.1

### Login failures are now visible (diagnosability)

The Global login flow could complete in the browser while the panel spun on
"请在浏览器完成登录" until the 5-minute TTL, then reported a generic expiry —
with no way to tell apart three very different situations. Root-caused by
driving the real code path against the live gateway (`go test -tags live`), and
fixed by making every outcome report itself.

**Verified live before/after** (real `www.workbuddy.ai`, 2026-09-15): a real
Global login returns `{code:0, data:{accessToken, refreshToken, expiresIn,
domain:"www.workbuddy.ai", scope, sessionState, tokenType}}`, `tokenData`
decodes it correctly, `login/account` returns `200 code=0`, and the credential
persists as `workbuddy-<uid>.json` with `accountRegion=global`. The token data
path was already correct — what was broken was the *reporting* around it.

- `logindiag.go` — new in-memory, **redaction-safe** step log per login flow.
  Records status / business code / upstream msg / requestId per step, plus a
  content-free rendering of the response shape (`jsonShape` reports key names,
  nesting and string **lengths**, never values). Consecutive identical polls are
  folded together (`×N`), so a minute of 2-second polls reads as one line
  instead of burying the interesting entry. Bounded by a per-flow ring buffer
  and a flow cap; pruned by the existing janitor. Never written to disk.
- `oauth.go` — `doJSON` no longer flattens upstream detail into an opaque
  string: failures now carry HTTP status, business code, msg and requestId
  (`upstreamError`). `apiEnvelope` captures the previously-dropped `requestId`,
  which is what upstream needs to trace a failed login.
- `oauth.go` — **`pending` now means pending.** Poll outcomes are classified
  instead of defaulting to `AuthLoginStatusPending`:
  - only the recognised waiting code (`11217`) reports pending;
  - any other business code, 4xx, redirect, parse failure, or missing
    `accessToken` on a `code:0` response is a **terminal error** carrying the
    upstream detail — these previously all looked like "still waiting";
  - an *unrecognised* code is tolerated as pending for
    `loginUnrecognisedCodeFastFail` (3) consecutive occurrences, so an unknown
    pending variant cannot break a working login, then goes terminal — turning
    a silent 5-minute hang into an immediate message.
- `oauth.go` — a failed `login/account` lookup is no longer swallowed. It still
  does not fail the login (the token is valid), but it is recorded, and the
  panel warns when the credential would be saved under the bare
  `workbuddy.json` name that `hostAuthList()` filters out — previously that
  produced a **successful login with an account invisible to the panel**.
- `oauth.go` — an omitted `expiresIn` is recorded as a warning. It previously
  stamped `expiresAt = now`, silently creating a born-expired credential (the
  same bug class already fixed on the refresh path via `preserveExpiry`).
- `oauth.go` + `logindiag.go` — **a completed login is no longer lost when
  persistence fails.** The upstream `auth/token` state is single-use and the
  credential exists only in that response, so the successful poll now consumes
  the state but caches the parsed credential (`loginResultStore`, 10-minute TTL,
  bounded and janitor-pruned). A retried poll replays it instead of answering
  "unknown state", and the host-driven path (which persists the credential
  itself and never calls `handleLoginPoll`) still gets its credential. A failed
  `host.auth.save` now returns `retryable: true` and the retry succeeds.
- `credits_handler.go` — `/login/poll` responses carry diagnostics
  (`diagnostics`, `upstream_code`, `upstream_http`, `request_id`), and a
  `warning` when the saved file name would be invisible to the plugin.
- `credits_handler.go` + `management.go` — new
  `GET /login/diag[?state=]` returns the redacted step log for one flow (or the
  list of flows), so a failure can be captured from a deployed instance without
  re-running the login.
- `panel.html` — **HTTP 429 is handled explicitly.** The panel polls every 2s
  but the management limiter is 5 burst + 1 per 6s, so most polls were rejected;
  a 429 body has no `status` field and previously fell through `pollLogin` into
  the generic pending path — the throttle was displayed as "still waiting".
  The panel now backs off for `Retry-After` (default 6s) and says so.
- `panel.html` — the login dialog renders the redacted step log for pending,
  error and success, so the user sees what is actually happening.
- **Tests** — `login_observability_test.go` (pending whitelist, terminal
  classification, fast-fail, `code:0`-without-token is an error, missing
  `expiresIn` warning, visible `login/account` failure, save-failure
  retryability, and a **no-token-material-leak** assertion over both the poll
  response and the recorded diagnostics), `management_ratelimit_test.go`
  (proves the 2s cadence cannot be sustained and that a 429 body carries no
  `status` field), `login_poll_response_test.go`, and `live_probe_test.go`
  (opt-in `-tags live` end-to-end probe against the real gateway).
- **Test-seam fix** — existing stubs passed `[]byte` to `okEnvelope`, which
  `json.Marshal` base64-encodes, so `HostAuthSaveResponse.Name` was silently
  zero in every test that "checked" it. `login_poll_response_test.go` now uses
  `json.RawMessage` and asserts the field for real.

## 0.9.0

### Native Global (international) login

Previously the plugin could only *run* Global accounts — there was no way to
*create* one. CPA's own "add auth" card calls `auth.login.start` with no region
selector, so it always opened the CN login page; the only workaround was to
hand-edit the `state` in the browser URL. Global login is now a first-class,
in-panel flow.

- `oauth.go` — new `startLoginFlow(region)` is the single login entry point;
  `handleStartLogin` (host card) and the new panel route both call it.
  `handlePollLogin` now polls the gateway that **issued** the state instead of
  always polling CN. (The two backends were measured to share state storage,
  but relying on that undocumented coincidence would break silently if upstream
  ever isolates them.)
- `main.go` — new `Region` type (`cn` / `global`) with `normalizeRegion`
  (accepts `global`, `intl`, `international`, `workbuddy.ai`, `overseas`, …),
  per-region endpoint builders, and `regionHeaders`. The latter fixes a real
  bug: `doJSON`'s fallback header set is CN, so Global `auth/state`,
  `auth/token` and `login/account` calls were being sent with a
  `codebuddy.cn` Origin — the same class of rejection as a Global JWT sent to
  `copilot.tencent.com`.
- `main.go` — new `domainForRegion`: when upstream omits `domain` on a Global
  login, the credential is stamped `www.workbuddy.ai`. Every downstream region
  decision (chat base, billing base, Origin/Referer, check-in skip, trial
  eligibility, exhaust-delete policy) keys off that field, so an empty domain
  would have silently produced a CN-classified Global account.
- `credits_handler.go` — panel login endpoints:
  - `POST /login/start` `{region}` → `{url, state, expires_at, ttl_seconds}`
  - `POST /login/poll` `{state}` → persists via `host.auth.save`, returns
    `{status, region, uid, nickname, domain}`; `status` is `pending` while
    waiting and terminal (`success` / `expired` / `error`) otherwise
  - `GET|POST /login/config` → read/set `default_region` (runtime-only, like
    the check-in toggle: the host exposes no plugin-config write callback)
- `panel.html` — a 登录账号 button opening a modal with a CN / Global picker,
  which opens the browser login page and drives the poll loop with live
  progress. The existing 导入凭证 path is unchanged.
- New `default_region` config (also `WB_DEFAULT_REGION` env) — controls which
  gateway **host-driven** logins (the CPA card) target. Default `cn`,
  preserving historical behaviour.
- **Tests** — `region_test.go` extended and `login_panel_test.go` added:
  region normalisation, per-region endpoints/headers, domain fallback,
  region-from-start-request precedence, poll-follows-login-region (asserting
  the *other* gateway is never contacted), zero-region CN fallback, and an
  end-to-end `/login/start`→`/login/poll` run against a fake Global gateway
  asserting the persisted credential's domain, file name and billing base.
- **Test seam** — `hostCallHook` in `main.go` lets tests stub host RPCs; the
  real path needs a live host function-pointer table that unit tests cannot
  construct.
- **Dead code** — `endpointAuthState`, `endpointAuthToken` and
  `endpointLoginAcct` removed: login endpoints are now built per region
  (`Region.authStateURL` / `authTokenURL` / `loginAcctURL`) because a Global
  login must hit `workbuddy.ai`. The chat/models/refresh CN aliases stay.
- **Note** — the old workaround (start a CN login, then hand-edit the browser
  URL from `copilot.tencent.com/login` to `www.workbuddy.ai/login` keeping the
  same `state`) still works, but is no longer necessary.

## 0.8.7

### Model list sync with upstream + fallback observability

- `models.go` — static `wbModels()` realigned to the upstream cli agent list
  (verified against two live accounts): added `deepseek-v4.1-flash`; removed
  `hy4-preview-x`, `hy3-preview`, `hy3-preview-agent`, `deepseek-v4-flash`
  (no longer offered by upstream). List order now matches upstream.
- `models.go` — dynamic-discovery fallback is no longer silent: when the
  upstream models API can't be used (missing token, API error, no cli agent),
  the plugin logs a warning with the reason and serves the static list.
- `models.go` — fallback results are now cached with a shorter TTL (1 min vs
  5 min for real data). Previously every `/v1/models` query retried a failing
  upstream call, spamming the API and adding latency.
- `creditlog.go` — `deepseek-v4.1-flash` added to the 0.5 credit factor tier.

## Unreleased

- Added request-level credit usage collection with JSONL persistence, retention limits, and model/hour/session aggregation APIs (/creditlog and /creditlog/summary).
- Added panel-ready account credit package data and usage breakdown endpoints for per-request and session inspection.
- Added prompt-cache token visibility: cache reads and cache writes are now parsed, stored and displayed as separate columns.
  - `usage.go` — `usageDetailFromMap` now reads the OpenAI-nested counters
    `prompt_tokens_details.cached_tokens` / `input_tokens_details.cached_tokens`
    (previously only flat `cached_tokens` was read, so OpenAI-shaped upstreams
    always reported zero cache hits). Also reads `cache_creation_input_tokens`
    and `prompt_tokens_details.cache_creation_tokens`, and now accepts
    `output_tokens_details.reasoning_tokens` alongside the existing
    `completion_tokens_details` path.
  - `creditlog.go` — new `creditEntry.CacheWrite` (`cache_creation_tokens`)
    distinguishes cache writes from the existing `Cached` (`cached_tokens`)
    read counter. Cache-creation tokens were previously dropped before storage
    because `usageDetailLite` had no field for them.
  - `creditlog.go` — `creditSnapshot()` exposes `cache_creation_tokens`; the
    per-model aggregate (`creditModelTotal`) now carries both `cached_tokens`
    and `cache_creation_tokens`; `creditGlobalSummary()` now reports token
    counts at all (it previously exposed only request/credit counters).
  - `panel.html` — the 消耗明细 view shows 缓存读取(命中) / 缓存写入 /
    缓存命中率 / 输入 Token cards plus per-model and recent-request cache
    read/write columns.
  - Live-verified against `copilot.tencent.com/v2/chat/completions` (glm-5.3):
    on a real prompt-cache hit the upstream returns
    `prompt_tokens_details.cached_tokens=4043` while the **flat**
    `cached_tokens` stays `0`. Flat-only parsing therefore reported zero cache
    reads for a request that hit 4043 tokens — the bug this fixes. A captured
    cache-hit response is pinned as a regression fixture in
    `usage_detail_test.go` (`realCacheHitUsage`).
  - Note: the upstream also exposes `prompt_cache_hit_tokens` /
    `prompt_cache_write_tokens` / `prompt_cache_miss_tokens`, which are not
    parsed. They carry the same read/write information as the keys above, so
    nothing is lost; wiring them would be redundant.
- Added per-model credits ↔ tokens conversion (`creditrate.go`,
  `GET /creditlog/rates`, panel 换算 UI).
  - `creditrate.go` (new) — the rate card is now a single table that answers
    both directions. `creditsForTokens` is the forward (spend) conversion and
    `tokensForCredits` / `tokensForCreditsBlended` are its exact inverse, so the
    rate the panel advertises can never contradict the credits it reports.
    Three billable classes are priced separately:
    `credits = (uncached_input + output×factor + cached×cache_factor) / tokens_per_credit`.
  - `creditrate.go` — the per-model factor is operator-overridable via
    `credit_rates` (`"glm-5.3=1.5,kimi-k2.7=0.8"`), and the scale via
    `tokens_per_credit` (default 1000). Overrides replace the whole set on each
    configure, so deleting a line from `config.yaml` actually reverts that model
    instead of leaving a stale factor behind. A malformed entry is skipped
    rather than discarding the operator's other corrections.
  - `creditlog.go` — the duplicated factor table and forward formula were
    removed; `estimateCredits` now delegates to `creditsForTokens`, so the
    estimate and the published rate share one implementation.
  - **Fix** — cache reads were charged twice. The upstream folds prompt-cache
    hits INTO `prompt_tokens` (the pinned live fixture:
    `prompt_tokens=4443` = 4043 cached + 400 miss), so pricing the raw prompt
    count billed each hit once at full input rate and again at the discounted
    cache rate. Input is now net of cache reads before pricing, which makes a
    cache-heavy request *cheaper* than the same volume uncached — previously it
    cost strictly more (regression test:
    `TestRecordCreditUsage_DoesNotDoubleCountCacheReads`). The stored
    `input_tokens` stays raw so the panel's volume columns remain truthful.
  - `management.go` — new `GET .../creditlog/rates`, registered in the
    management API. Without query params it returns the rate card only; with
    `?credits=N` it adds the per-model `N credits → M tokens` columns, and
    `?output_share=0..1` picks the completion share of the mix. Bad input
    returns an error rather than a table of zeros.
  - `panel.html` — the 消耗明细 view gained a 积分 ↔ Token 换算 section: an
    interactive estimator (enter a credit budget + optional output share, get
    the per-model token equivalent) plus a per-model rate table that marks
    config-overridden factors with ✱. Per-model rows in the existing Token
    detail also show their own `1积分≈N入/M出`.
  - Tests — 33 new cases in `creditrate_test.go` /
    `creditrate_contract_test.go` cover both conversion directions, the
    round-trip inverse, per-model divergence, override precedence and
    replacement, malformed-config tolerance, the handler's validation, and the
    exact JSON field names the panel reads (a rename would otherwise silently
    render "-" instead of failing).

## 0.8.2

### Concurrency + lifecycle hardening

- `lifecycle.go` — P0-2: `reconcileOneAccount` now routes credits fetch
  through `cachedAccountDetails(force=true)` so singleflight serializes
  concurrent writers, eliminating a Load→Store race that could clobber
  newer plan/checkin values.
- `lifecycle.go` — P1-4: Global `lifecycleDelete` now requires a second
  `fetchUserResource` confirmation before deleting. Prevents transient 402
  from irreversibly removing an account.
- `checkin.go` — P1-5: after a successful checkin the credits cache is
  refreshed immediately (was only updating the checkin field). Panel now
  shows updated balance without waiting for the async reconcile pass.
- `cache.go` — P1-1 documented trade-off: force=true callers still join
  singleflight (skipping would re-introduce P0-2).
- `main.go` — P0-5: `scheduler_mode` ConfigField description now warns that
  `off + lifecycle_auto=false` leaves exhausted accounts routable.

## 0.8.1

### Bug fixes + compliance polish

- `keepalive.go` (new) — daily 22:00 access-token refresh to prevent Keycloak
  offline-session expiry; reuses `schedulerLoop`, routes via `host.http.do`,
  uses CPA native `disabled` field for session-dead auths.
- `models.go` — fix `filterExcludedModels` slice aliasing that corrupted
  `dynamicModelsCache` (P0).
- `billing.go` — route all billing API calls through `hostHTTPDo` (was missed
  in v0.7.0); improve "parse failed" error to include a redacted body snippet.
- `checkin.go` — avoid double `fetchCheckinStatus` in classify already-branch.
- `billing.go` — `performCheckinCall` now sets `success=true` as bool to avoid
  downstream type-mismatch when upstream returns a string.
- `host_auth.go` — fresh slice in `hostAuthList` to avoid aliasing RPC response.
- `oauth.go` — route `handleRefreshAuth` via `hostHTTPDo` (last path still on
  `sharedHTTPClient()`); make OAuth error messages actionable.

## 0.8.0

### Refactor — community-grade file layout

完成 v0.7.0 合规改造后的代码组织大重构，把两个超大主档拆成单一职责的
小文件，对齐 CPA 原生 plugin 案例的"一个能力一个文件"原则。

**File splits (main.go 2940 → 809, management.go 2263 → 349, lifecycle.go 980 → 535)：**

- `redact.go` (49) — redactSecrets + 4 个 regex + truncate
- `usage.go` (242) — handleUsage + publishUsage + forwardUsageToCPAMP + sseUsageCollector
- `payload.go` (469) — prepareUpstreamBody + 4 个 InPlace mutator + 4 个 legacy 包装
- `stream.go` (452) — streamEmit/Close + pumpUpstreamStream + collectUpstreamStream + aggregate*
- `models.go` (443) — callModelsAPI + fetchDynamicModels + resolveUpstreamModel + alias 反解
- `oauth.go` (240) — handleStartLogin/PollLogin/RefreshAuth + newLoginClient + doJSON
- `host_bridge.go` (388) — hostHTTPDo/DoStream/Read/Close + hostStreamReader + Direct fallbacks
- `billing.go` (486) — billing API + fetch* + perform* + JSON helpers
- `cache.go` (183) — accountCache + accountDetailFlight singleflight + prune
- `host_auth.go` (73) — hostAuthList/Get/GetBundle (host auth-store RPC)
- `usage_config.go` (202) — configure + resolveUsageReport + probe* + config vars
- `checkin.go` (515) — handleManualCheckin + runAutoCheckin + schedulerLoop + classify/execute/summarize
- `credits_handler.go` (285) — handleImportAuth/CheckinConfig/ClaimTrial/SelectAuth/CreditsQuery
- `panel.go` (266) — buildDashboardEx + summarizeCredits + servePanel + panelHTML
- `policy.go` (188) — lifecycleAction decisions + displayNote + labelForAuth
- `authfile.go` (299) — authFileNameFor/sanitizeUIDForFileName/hostAuthPersist/deleteAuth + path safety

**保留的小文件**：`scheduler.go` (138)、`active_auth.go` (158) — 本来就够小。

**文档（社区标准）：**

- `README.md` — 英文版，Features / Quickstart / Configuration / Lifecycle / Development / License
- `README_CN.md` — 中文版
- `LICENSE` — MIT
- `Makefile` — build / test / lint / clean / release / tag 目标
- `.gitignore` — 忽略 `*.so` / `*.h` / `bin/` / `dist/`
- `docs/architecture.md` — 模块图 + 数据流 + 关键设计决策 + 与 CPA 的集成点
- `docs/development.md` — 本地构建 / 测试 / 调试 / 发布流程
- `docs/definition-of-done.md` — v0.8.0 验收标准（量化可测）

### Lint / style

- `gofmt -l .` → 0 files
- `go vet ./...` → 0 issues
- `gocritic check ./...` → 0 issues（修复 policy.go 的 ifElseChain）
- `staticcheck` 真实代码问题 0（工具链版本噪音已过滤）

### Bug Fixes (carried over from v0.6.31 / v0.7.0)

本次重构完整保留了之前所有 bug 修复：
- UID 路径穿越白名单（authfile.go sanitizeUIDForFileName）
- refresh_token 不再泄露到 chat 上游（main.go backendHeaders）
- invalidateAccountCredits 数据竞争修复（值拷贝）
- handleManualCheckin early-already merge（不丢 credits/plan）
- configure 嵌套锁修复（parse-then-lock）
- scheduler_mode off 接通（handleSchedulerPick 读取配置）
- deleteAuth 调 clearActiveAuthIfMatch
- runAutoCheckin 串行改并发（sem=4）
- cachedAccountDetails singleflight
- panel.html XSS 修复（addEventListener + dataset）
- panel.html CSRF（fetch credentials:omit）
- redactSecrets 裸 JWT 兜底
- pumpUpstreamStream context cancel
- out[:0] 共享底层数组改新 slice
- 热路径 4 次 JSON 序列化合并为 1 次
- 冒泡排序改 sort.Slice
- usageReportConfigured/buildDashboard 死代码删除
- handleManualCheckin 三段拆分（classify/execute/summarize）
- management BasePath 缓存（register 时读取宿主注入）

### Tests

- 115/115 tests pass (`go test -race`)
- 新增 `TestSchedulerPick_OffMode_Defers` 覆盖 scheduler_mode=off 行为

## 0.7.0

### Compliance — CPA native patterns
本次大版本把「自建通道」全部替换为 CPA 官方提供的 RPC / 能力接口，
对齐 `sdk/pluginapi` 的设计意图。生产路径 100% 走宿主桥接，插件不再
绕过宿主审计 / request-log / transport policy。

- **所有上游 HTTP 调用走 `host.http.do` / `host.http.do_stream`**：
  - `models API`、`billing API`、`usage 上报`、`chat completions`（流式 + 非流式）
    全部从 `sharedHTTPClient().Do` 切到 `hostHTTPDo` / `hostHTTPDoStream`。
  - 宿主 request-log 现在能捕获插件的出站请求和原始响应（之前完全看不到）。
  - 宿主 transport policy（proxy、超时、连接池）对插件上游调用生效。
  - `sharedHTTPClient` 降级为 fallback 专用：仅当宿主桥不可用（单元测试 /
    老版本 CPA）时使用。新代码直接调用 `sharedHTTPClient` 视为合规 bug。
- **`hostStreamReader` 适配层**：把宿主桥的 32KB 任意字节块适配为 `io.Reader`，
  `bufio.Scanner` 的 SSE 行切分逻辑不变，pump / collect / aggregate 全部透明迁移。
- **`UsagePlugin` 能力声明 + `handleUsage` RPC handler**：
  - 注册能力 `usage_plugin: true`，宿主每次请求完成后会把规范化的
    `pluginapi.UsageRecord` 推送给插件。
  - 插件在 `handleUsage` 里把 record 转发到 CPAMP，与宿主 `DefaultManager`
    的记录并行，不再重复也不遗漏。
  - 旧路径 `publishUsage` 保留向后兼容（老版本 CPA 没接 UsagePlugin 时仍可
    上报），新路径 `handleUsage` 同步触发，CPAMP 侧基于 (timestamp + auth +
    model + total_tokens) 幂等去重。
- **`reportUsageToCPAMP` 重命名为 `forwardUsageToCPAMP` 并走 host.http.do**：
  CPAMP 上报自身也走宿主桥，宿主能看到插件的运维流量。

### Architecture notes
- `hostBridgeAvailable()` 检查 `hostAPI.call` 是否为 nil，统一决定是否
  fallback。生产环境永远为 true，单元测试永远为 false（无宿主）。
- 所有 `*Direct` 函数仅服务测试；生产路径不经过。
- 宿主侧 `sanitizePluginRequest` 会把 `ExecutorRequest.HTTPClient` 置 nil
  （跨 c-shared 边界接口无法传输），所以**插件不可能用宿主注入的
  HTTPClient**——`host.http.*` RPC 是 c-shared 插件访问宿主 transport 的
  唯一合规方式，本版本全部采用。

## 0.6.31

### Security
- **UID 路径穿越修复**：`authFileNameFor` 新增 `sanitizeUIDForFileName` 白名单
  （`[^a-zA-Z0-9_-]+` → `_`、长度 ≤64、拒绝 `.`/`..`），导入凭证的
  `workbuddy-<uid>.json` 不再可能被 `../` 注入到任意路径。
- **refresh_token 停止泄露到 chat 上游**：`backendHeaders` 移除
  `X-Refresh-Token`。refresh_token 是长期凭证，只在 refresh 端点用；之前每次
  chat completion 都附带它，上游日志一旦记录请求头即等同账号被盗。
- **插件层 management 鉴权 + 限流**：`handleManagement` 入口对所有 POST /
  写端点新增插件层防护：constant-time Bearer 比对（`crypto/subtle`），
  per-IP token-bucket 限流（容量 5、每 6s 1 个）。配置方式：
  `config_yaml management_key:` 或 env `WB_MANAGEMENT_KEY`。空则保持
  历史行为（仅依赖宿主鉴权）。
- **panel.html XSS 修复**：4 处 `onclick="...('${esc(auth_index)}',this)"`
  改为 `data-action` + `data-auth-index` + `addEventListener`。`esc()` 只
  转义 HTML 不防 JS 字符串上下文注入。
- **panel.html CSRF 缓解**：`fetch` 显式 `credentials:'omit'`，面板纯靠
  Authorization Bearer，不再隐式带 cookie。
- **redactSecrets 兜底裸 JWT**：新增 `redactREJWTLoose` 正则，匹配不带
  `Bearer` 前缀、`access_token` key 的 `eyJ…` 两段/三段 JWT。

### Bug Fixes
- `invalidateAccountCredits` 数据竞争：直接改 sync.Map 共享 entry 的字段
  （`e.credits = nil`），并发 dashboard / reconcile / chat 后置 invalidate
  会拿到撕裂状态。改为 `fresh := *e; Store(&fresh)` 值拷贝，与其他 4 处
  写法一致。
- `handleManualCheckin` "early already" 路径丢 credits/plan：直接构造
  `accountCacheEntry{checkin: ci}` 覆盖整个 entry，签到后面板积分消失。
  改为 merge prev 的 credits/plan。
- `configure` 嵌套锁：在 `checkinAutoMu` 内嵌套获取 `lifecycleAutoMu` /
  `schedulerModeMu`，未来加反向获取路径即死锁。改为两阶段：无锁解析到
  局部变量，再分别单锁写入。
- `scheduler_mode: off` 配置断链：configure 解析但 `handleSchedulerPick`
  从不读取，"off" 实际表现为 "credits"。现在 off 正确 defer 给内置 scheduler。
- 删除 Global 账号后 `activeAuthID` 残留指向已删 ID：`deleteAuth` 两个成功
  路径现在都调 `clearActiveAuthIfMatch(authID)`。
- `runAutoCheckin` 重复 `fetchCheckinStatus` + 变量 shadow：原代码内层
  `ci` shadow 外层，且第二次调用与第一次状态可能不一致。改为单次调用，
  签到成功才 refresh。
- `out[:0]` 共享底层数组：`filtered := out[:0]` 复用底层数组在 range 中
  写入，改为 `make([]wbAccount, 0, len(out))`。
- `pumpUpstreamStream` 无 context：`http.NewRequest` 无 context，客户端
  断开后 goroutine 一直读到 120s 超时。改为 `NewRequestWithContext` +
  cancel 传入 pump，所有退出路径释放。

### Performance
- **热路径 4 次 JSON 序列化合并为 1 次**：新增 `prepareUpstreamBody` 统一
  `forceStreamBody` + `normalizeToolsForUpstream` + `rewriteSystemForUpstream`
  + `ensureSystemMessage` + `rewriteModelInBody`，单次 unmarshal + 单次
  marshal。每次 chat completion 省 4-5 个 JSON 往返。
- **`runAutoCheckin` 串行改并发**：抽出 `processAutoCheckinAccount`，主循环
  `sem=4` 并发。N 账号从 3N 串行 HTTP 降到并发 4 路。
- **`cachedAccountDetails` 加 singleflight**：per-authID `sync.Map` + done
  channel。并发 dashboard / reconcile 对同一账号只跑 1 次上游 fetch，
  其他 goroutine 等结果，消除 6x upstream QPS + last-writer-wins。
- **冒泡排序改 sort.Slice**：`pruneAccountCacheSoftCap` 从 O(n²) 降到 O(n log n)。

### Refactor
- **handleManualCheckin 273 行拆分**：`classifyCheckinTargets` /
  `executeCheckinBatch` / `summarizeCheckinResults` 三段独立函数，各自
  单一职责，便于单测。
- **management BasePath 不再硬编码**：register 时缓存宿主注入的 BasePath，
  handleManagement 用 cached 值。宿主未来版本化路径不会失效。
- 死代码清理：删 `upstreamBase` legacy 常量、`usageReportConfigured` 无人
  调用、`buildDashboard` 包装函数。

### Tests
- 新增 `TestSchedulerPick_OffMode_Defers` 覆盖 scheduler_mode=off 行为。
- 全套 115 tests + `-race` 通过。

## 0.6.29

### Fixed
- 修复签到后按钮不变"已签到"、套餐标记丢失的问题
  根因：handleManualCheckin/runAutoCheckin/handleClaimTrial 在签到/领取成功后
  accountCache.Delete(f.ID) 把 cache 清了，light load 时 checkin/plan 是 nil。
  handleCreditsQuery 的 cache merge 逻辑从 prev.plan（空）取值而不是用刚获取的
  fetchPaymentType(sa) 结果，导致 plan 在 light load 后丢失。
  修复：签到/领取成功后把 checkinSummary 存回 cache 而不是删除；
  handleCreditsQuery cache merge 用刚获取的 plan；runAutoCheckin/handleClaimTrial
  改为 invalidate credits（置 nil）而不是删除整个 cache entry。

## 0.6.28

### Fixed
- 修复面板选中卡片与实际路由账号不一致的根本问题
  根因：activeAuthID 存的是 auth.Index（运行时 SHA256 hash），但 scheduler
  的 SchedulerAuthCandidate.ID 是 auth.ID（持久化 UUID），两者永远不匹配，
  导致 pickActiveAuth 永远走 fallback 选第一个，面板显示选中第一个但实际
  路由到别的账号。同时 cachedCreditsScore 用 auth.ID 查 accountCache（key
  是 auth.Index）也查不到，exhausted 判断也坏了。
  修复：全链路统一用 auth.ID — activeAuthID、accountCache key、
  lifecycleState key、面板 selected 判断、/select API 返回值全部改用
  auth.ID。lifecycle 函数（reconcileOneAccount/disableAuth/reenableAuth/
  deleteAuth/syncAuthNote）加 authID 参数，resolveAuthIndex 改为
  resolveAuthIndexAndID 同时返回 index+ID。
- 修复首次加载面板时选中耗尽账号的问题
  首次 GET /accounts 不拉 credits（fetchCredits=false），所有卡片
  Exhausted=false，ensureDefaultActiveAuth 选第一个。lazyLoadCredits
  异步获取积分后发现第一个已耗尽，但选中状态不会更新。
  修复：lazyLoadCredits 全部完成后前端静默再拉一次 /accounts（此时
  cache 已有 credits，light load 能拿到正确 exhausted 和 selected），
  重新渲染卡片。

## 0.6.27

### Fixed
- ensureDefaultActiveAuth 也检查 Exhausted：面板刷新时选中账号已耗尽会同步切换
  修复 scheduler.pick 切了但面板 ensureDefaultActiveAuth 又选回去的 race
  现在 pickActiveAuth 和 ensureDefaultActiveAuth 用同一套规则，选中状态不会漂移

## 0.6.26

### Fixed
- 选中账号积分耗尽时自动切换到第一个可用账号，并同步更新选中状态
  全部耗尽时留在当前账号不 flip-flop
  修复 v0.6.25 过度 sticky 导致耗尽后一直报错的问题

## 0.6.25

### Fixed
- 选中账号 sticky：scheduler 不会因缓存过期/积分耗尽自动切换到别的账号
  只有 host 把选中账号从候选列表移除（disabled/deleted）才切换
  修复面板显示选中A但实际路由到B、静默消耗积分的问题

## 0.6.24

### Fixed
- model.static / model.for_auth 现在尊重 CPA 的 oauth-excluded-models 配置
  在 config.yaml 的 oauth-excluded-models.workbuddy 里列出的模型不再出现在 /models

## 0.6.23

### Fixed
- usage import URL 自动探测：先试 127.0.0.1:18317（裸机/Docker host），再试 Docker 服务名 cpa-manager-plus:18317
  不再写死 Docker 服务名，裸机安装也能自动找到 CPAMP

## 0.6.22

### Fixed
- ExecutorModelScope 改为 OAuth：插件只处理 workbuddy auth 绑定的模型
  不再拦截其他 openai-compatible 供应商的同名裸模型（如 deepseek-v4-flash、glm-5.2）
  修复启用 workbuddy 后自定义供应商模型请求不进监控的问题

## 0.6.21

### Fixed
- 积分懒加载改为并发：所有卡片同时请求，不再逐个排队

## 0.6.20

### Fixed
- 懒加载积分时同时拉取 plan（套餐类型），修复 plan 徽章显示「-」不更新

## 0.6.19

### Added
- 每张卡片新增「刷新」按钮：单独查询积分并即时更新该卡

## 0.6.18

### Added
- 积分懒加载：进页面先渲染骨架卡（加载中…），逐卡异步拉积分，失败自动重试一次
- 后端 `/accounts` 默认不再并发拉所有账号 credits（避免上游 500）
- `/credits?auth_index=` 单账号查询返回完整字段（region/exhausted/trial_claimed）

### Fixed
- 缓存有效时仍返回缓存的 credits，不再触发上游请求

## 0.6.17

### Fixed
- 流式路径也强制 `stream:true`：WorkBuddy API 现仅支持 stream 模式，`stream:false` 会报 "Non-stream chat request is currently not supported"

## 0.6.16

### Fixed
- 夜间模式：用量汇总卡与账号卡统一 `--card` 底色；内部指标格改用 `--surface`，避免汇总卡看起来更深/发黑

## 0.6.15

### Added
- 面板「选用」账号：默认第一张可用卡；选中卡决定 CN/Global 路由（读 domain，不解码 JWT）
- 选中账号耗尽/禁用/消失时随机切换下一张可用卡并记住

### Changed
- scheduler.pick 改为始终跟随 active 选中账号（不再依赖 credits 排行模式）

## 0.6.14

### Fixed
- Global 账号聊天 401/400 修复：JWT iss=workbuddy.ai 必须走 www.workbuddy.ai 端点（copilot.tencent.com 会对 Global token 返回 401）
- Global 请求自动注入 system message（www.workbuddy.ai 对 user-only 请求返回 code 11101）
- token 刷新和 models 发现也走域名感知端点

## 0.6.13

### Changed
- 请求监控 key 自动探测：config → env（CPAMP_ADMIN_KEY/USAGE_REPORT_KEY）→ docker secret `/run/secrets/cpamp_admin_key`，无需手写 usage_report_key


## 0.6.12

### Changed
- 删除无效 `usage.PublishRecord` 路径，请求监控仅走 CPAMP `/v0/management/usage/import`


## 0.6.11

### Fixed
- **请求监控**：c-shared 隔离导致 `usage.PublishRecord` 进不了宿主 redisqueue；改为异步 POST CPA-Manager-Plus `/v0/management/usage/import`（`usage_report_url`/`usage_report_key`）
- 补全 ExecutorType/AuthType/Source；配置字段暴露于管理面板


## 0.6.10

### Fixed
- **批量签到先过滤再操作**：Global 不参与；今日已签跳过；仅对 CN 未签账号调用 daily-checkin
- 返回 `summary{success,already,skipped_global,fail,eligible}`，面板文案不再把 Global/已签当失败
- 分类/签到并发（限流），降低「全部签到」卡到 502 context canceled

## 0.6.9

### Changed
- **Panel theme adaptive**: CSS variables now default to light (paper) theme; `[data-theme="white"]` and `[data-theme="dark"]` overrides align with CPA management panel tokens. Embedded iframe mirrors parent `data-theme` via MutationObserver; standalone page follows `prefers-color-scheme`. All hardcoded dark colors (toast, modal, input, buttons) replaced with theme-aware CSS variables.

## 0.6.3

### Fixed
- Auth identity: parse/refresh leave ID empty; regression tests (A-01)
- Stream pump: emit failure is failed usage; defer streamClose (A-06)
- No dual-write after host.auth.save (A-15)
- Scheduler skips host-disabled candidates (A-04)
- Global delete reconstructs path via peer auth dir (A-07)
- Panel IP ban wait parses upstream window (A-08)
- accountCache concurrent errs race + soft cap (A-02)
- Dashboard single host.auth.get per row (A-05)
- Instant check-in/trial button state (panel)


## 0.6.2

### Fixed
- **Credits look frozen after chat**: cache TTL 5m→45s; invalidate cache after successful chat (stream + non-stream)
- **Spend math**: package used = cycle size−remain; account total_size from package sizes; TotalDosage treated as capacity pool (not consumption)
- **Check-in packs inflate "available"**: UI labels 可用/已用/额度池 so grant vs spend is visible; note shows 余/已用/池

## 0.6.1

### Added
- WorkBuddy panel **用量汇总**：筛选范围内 剩余/已用/总量/占比 + 进度条；全部视图附 CN/Global 分项
- Dashboard API `summary` 字段：`total_remain` / `total_used` / 分区域统计

### Notes
- CPAMP Auth 页进度条仅支持内置 `codex/claude/kimi/xai/antigravity`（`QUOTA_PROVIDER_TYPES` 白名单）；workbuddy 无法靠 `note` 注入进度条，完整用量看插件面板

## 0.6.0

### Added
- **Credit lifecycle** (plugin-only, no CPA/CPAMP source changes):
  - CN exhausted → write auth file `disabled:true` (host skips scheduling)
  - Global exhausted → **delete** auth file (`os.Remove` on path from `host.auth.get`)
  - CN disabled + credits return (after check-in / refresh) → `disabled:false`
  - Executor hard credit errors → async reconcile; pure 429 does not delete Global
  - Unknown credits → no-op (safe default)
- Auth file **note** / **label** enrichment: `CN · 余 x · …` / `Global · …` / 已禁用
- Panel: CN/Global filter tags + counts; disabled badge; lifecycle toast on refresh
- Panel: management-key discipline to avoid CPA IP ban (no request without key; 401/403 backoff)
- Config field `lifecycle_auto` (default true)

### Changed
- Scheduled tick **no longer auto-claims Global trial** (one-shot; manual `/trial` / panel only)
- Tick = CN check-in (if `checkin_auto`) + lifecycle reconcile for all regions
- Import/save writes top-level `type`/`logo`/`note`/`disabled` with nested auth/account
- Force dashboard refresh runs lifecycle and may drop deleted Global rows

### Notes (CPAMP Auth page)
- Filter letter **「W」** / brand typeBadge colors cannot be fixed from the plugin (frontend static icon table)
- Plugin sets `Metadata.logo` + registration Logo; Auth cards show **note** for region/credits summary
- Full UX: WorkBuddy side panel

## 0.5.0

### Added
- International (Global) WorkBuddy account support (`www.workbuddy.ai` domain)
- Domain-aware billing API routing: CN accounts → `codebuddy.cn`, Global → `workbuddy.ai`
- Expert trial pack claim API: `POST /plugins/workbuddy/trial` (Global only, one-time 250 credits / 14 days)
- Panel region badges: light green `CN` (daily checkin) + light orange `Global` (expert trial)
- "全部领取" batch claim button for Global accounts
- Auto-scheduler region branch: CN → daily checkin, Global → claim expert trial if unclaimed
- `wbAccount.region` and `wbAccount.trial_claimed` fields in accounts API response
- `hasTrialPack()` helper detects trial pack from `get-user-resource` packages

### Changed
- `billingBase` selection is now domain-driven via `billingBaseFor(sa)`
- `backendHeaders` Origin/Referer dynamically set per account domain via `originRefererFor(sa)`
- Panel card buttons: CN → 签到, Global → 领取专家加油包 / 已领取
- "全部签到" button only triggers CN accounts (Global accounts are skipped with a message)
- `runAutoCheckin` branches by region: CN daily checkin, Global trial claim

## 0.4.3

### Changed
- Panel import modal: white surface + dark text for readable contrast (was dark-on-dark)

## 0.4.2

### Changed
- Panel: credential import is a toolbar button (left of 刷新数据) opening a modal, instead of an always-visible card

## 0.4.1

### Added
- Panel **耗尽** badge + `exhausted` field on accounts API (shared with scheduler)
- Credential **import** API `POST /plugins/workbuddy/import` + panel paste UI
- Per-account check-in lock (multi-tab safe)
- `executor.count_tokens` stub (`input_tokens:0` — upstream has no API)
- LICENSE (MIT), VERSION file, GitHub Actions multi-arch release workflow

### Changed
- SSE cleanChunk strips empty `extra_fields` / `refusal` / `reasoning_content`
- Scheduler credits mode prefers non-exhausted accounts first

## 0.4.0

### Added
- CPA **Scheduler** capability with `scheduler_mode`: `off` (default) | `credits`
- Credits-aware multi-account pick using panel credit cache

## 0.3.18

### Fixed
- ConfigFields use SDK `ConfigFieldType*` constants

## 0.3.17

### Fixed
- `FrontendAuthProvider` set false; remove dead frontend-auth handlers

## 0.3.16

### Fixed
- Panel refresh toast + busy feedback

## 0.3.15

### Fixed
- Normalize OpenAI object `tool_choice` for CodeBuddy upstream
