# QoderWork CPA Plugin — Loop 文档 v3

> **2026-07-26 重写。PAT + 纯软件 COSY 签名已端到端验证通过。**
> **废弃：WASM 签名、qodercli 子进程、device_token 直签。**

---

## 架构（最终版，已实测 200 OK）

```
CPA Gateway
  └─ qoderwork plugin (Go, c-shared)
       ├─ auth: PAT → jobToken/exchange → jt-/jrt-
       ├─ sign: COSY (RSA + AES-CBC + MD5) 纯 Go
       ├─ encode: QoderEncoding (custom base64 + 三段重排)
       ├─ infer: POST gateway.qoder.com.cn/algo/.../agent_chat_generation
       ├─ checkin: /sash/api/v1/me/daily-check-in/* (jt- Bearer)
       └─ quota: /api/v2/quota/usage (jt- Bearer)
```

**为什么这条路：**
- ✅ 已实测：qwen3.8-max 返回 "Hi!"，SSE 正常，usage 正确
- ✅ 无需 WASM：签名是纯软件算法，Go 标准库全覆盖
- ✅ 无需子进程：桌面端 WorkerTransport 直签，不 spawn CLI
- ✅ PAT 100 年有效，jobToken 24h+48h 自动 refresh，无需重复登录
- ✅ 开源验证：cubk1/qoder2api (Java 73★)、bzym2/QoderGateway (Python 13★) 都是这条路

---

## 技术要点速查

### PAT 能力边界

| 能力 | PAT (pt-) | jobToken (jt-) | 备注 |
|---|---|---|---|
| 换 jobToken | ✅ | — | `POST /api/v1/jobToken/exchange` |
| 推理对话 | ❌ 401 | ✅ | 需 COSY 签名 |
| 签到 | ❌ | ✅ | `/sash/api/v1/me/daily-check-in/*` |
| 积分查询 | ❌ | ✅ | `/api/v2/quota/usage` |
| 用户信息 | ❌ | ✅ | `/api/v1/userinfo` |
| 有效期 | 100 年（创建时填） | 24h（jrt- 48h 可 refresh） | |

### COSY 签名核心（Go 伪码）

```go
tempKey := randomBytes(16)
cosyKey := b64(rsaPKCS1v15(pubKey, tempKey))
info := b64(aesCBC(json(identity), tempKey, tempKey))
payloadB64 := b64(jsonSorted({cosyVersion:"0.1.43", info, requestId, version:"v1"}))
sig := md5(payloadB64 + "\n" + cosyKey + "\n" + ts + "\n" + body + "\n" + path)
Authorization: "Bearer COSY." + payloadB64 + "." + sig
```

### QoderEncoding

```
自定义字母表: _doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!
padding: $
步骤: base64(plain) → 三段重排 std[n-a:] + std[a:n-a] + std[:a] → 映射到自定义字母表
```

### 推理端点

```
POST https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation
     ?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1

Headers: cosy-data-policy, cosy-machinetype, cosy-clienttype=5, cosy-date, cosy-user,
         cosy-key, cosy-clientip, authorization, cosy-version=0.1.43, cosy-machineid,
         cosy-machinetoken, login-version=v2, x-model-key=<model>, x-model-source=system

Body: qoderEncode(json(baseprompt_template))
```

### 模型 key 映射

| Qoder key | 后端 | price_factor |
|---|---|---|
| auto | Smart Routing | 1.0x |
| ultimate | 专家级 | 1.6x |
| performance | 高级 | 1.1x |
| efficient | 标准 | 0.3x |
| lite | 免费 | Free |
| qwen3.8-max | Qwen3.8-Max | 已验证 |
| qmodel | Qwen3.7-Max | 0.5x |
| q35model | Qwen3.7-Plus | 0.1x |
| dmodel | DeepSeek-V4-Pro | 0.5x |
| dfmodel | DeepSeek-V4-Flash | 0.1x |
| gm51model | GLM-5.2 | 0.6x |
| kmodel | Kimi-K3 | 0.8x |
| mmodel | MiniMax-M3 | 0.2x |

---

## Loop 1: Plugin 骨架（~30min）

| # | 任务 | 验收 |
|---|---|---|
| 1.1 | go.mod + CPA SDK v7 | go mod tidy 成功 |
| 1.2 | main.go C ABI | c-shared 编译通过 |
| 1.3 | RPC dispatch stub | CPA 加载不 panic |
| 1.4 | plugin register | CPAMP 显示 |
| 1.5 | model.static | /v1/models 含 qoder-* |
| 1.6 | Makefile | make build → .so |

---

## Loop 2: Auth 模块（~1h）

| # | 任务 | 验收 |
|---|---|---|
| 2.1 | auth.login_start: 提示填 PAT | 返回 manual_instructions |
| 2.2 | auth.parse: 接受 PAT JSON | 解析 pt- 成功 |
| 2.3 | auth.refresh: jobToken/refresh | jt- 换新成功 |
| 2.4 | auth_storage | qoderwork-<uid>.json 落盘 |
| 2.5 | PAT → jobToken exchange | 返回 jt-/jrt- |
| 2.6 | jt- 过期检测 + 自动 refresh | 401 时触发 |

---

## Loop 3: COSY 签名 + Encoding（~1h）

| # | 任务 | 验收 |
|---|---|---|
| 3.1 | sign.go: RSA/AES/MD5 | 与 Python 输出一致 |
| 3.2 | encoding.go: qoderEncode/Decode | 与 Python 输出一致 |
| 3.3 | bearer.go: buildBearer | 生成完整 Authorization |
| 3.4 | headers.go: buildHeaders | 15 个 headers 全 |
| 3.5 | 单元测试 | go test PASS |

---

## Loop 4: 对话执行（~1.5h，核心）

| # | 任务 | 验收 |
|---|---|---|
| 4.1 | body.go: 构造 baseprompt | 字段完整 |
| 4.2 | executor.execute_stream | SSE 流式输出 |
| 4.3 | executor.execute | 非流式聚合 |
| 4.4 | SSE 解析: data:{"body":"..."} → OpenAI delta | 正确转换 |
| 4.5 | 模型路由: CPA name → x-model-key | qwen3.8-max 通 |
| 4.6 | 错误处理: 401 → refresh → 重试 | 自动恢复 |
| 4.7 | 超时/取消 | context.Context |

**验收：** `curl CPA/v1/chat/completions -d '{"model":"qwen3.8-max","messages":[{"role":"user","content":"hi"}]}'` 返回流式

---

## Loop 5: 签到 + 积分（~30min）

| # | 任务 | 验收 |
|---|---|---|
| 5.1 | credits 查询 | total/used/remaining |
| 5.2 | 签到状态 | CLAIMABLE/CLAIMED |
| 5.3 | 签到领取 | +100 credits |
| 5.4 | 定时签到 goroutine | 09:00/21:00 UTC+8 |
| 5.5 | 耗尽自动 disable | lifecycle |

---

## Loop 6: 多账号调度（~30min）

| # | 任务 | 验收 |
|---|---|---|
| 6.1 | scheduler.pick | 按 remaining 选 |
| 6.2 | 跳过 disabled | 正确过滤 |
| 6.3 | 并发安全 | 无竞态 |

---

## Loop 7: 管理界面 + 部署（~1h）

| # | 任务 | 验收 |
|---|---|---|
| 7.1 | management routes | CPAMP 显示 |
| 7.2 | panel.html | 渲染正确 |
| 7.3 | 编译 arm64+amd64 | .so 生成 |
| 7.4 | 部署 CPA | plugin 加载 |
| 7.5 | E2E | 全流程通 |
| 7.6 | README + GitHub push | tag v0.1.0 |

---

## 时间线

| Loop | 预计 | 累计 | 难度 |
|---|---|---|---|
| 1. 骨架 | 30min | 30min | ⭐ |
| 2. Auth | 1h | 1.5h | ⭐⭐ |
| 3. 签名 | 1h | 2.5h | ⭐⭐ |
| 4. 对话 | 1.5h | 4h | ⭐⭐⭐ |
| 5. 签到 | 30min | 4.5h | ⭐ |
| 6. 调度 | 30min | 5h | ⭐ |
| 7. 部署 | 1h | 6h | ⭐⭐ |

**总计 ~6 小时**（比 v2 少 1h，省掉 WASM 和子进程管理）

---

## 执行策略

- **Loop 1-3**: Hermes 直接写（参考 workbuddy + cubk1）
- **Loop 4**: Hermes 写框架 + 实测（已有 Python 参考）
- **Loop 5-7**: Claudium 后台（逻辑明确，多文件）

---

## 风险矩阵

| 风险 | 概率 | Loop | 影响 | 回退 |
|---|---|---|---|---|
| Go 签名细节差异 | 低 | 3 | 401 | 对照 Python diff |
| jobToken refresh 未验证 | 中 | 2 | 24h 后失败 | PAT 重换 |
| 模型 key 映射不全 | 中 | 4 | 404 | 从 CLI bundle 抄 |
| Qoder 封自动化 | 低 | all | 账号封 | 限速 |
| SSE 嵌套 JSON 解析 | 低 | 4 | 解析错 | 格式已固定 |

---

## 关键参考

- **Python 验证脚本（已通过）：** `/tmp/qw_web/qoder_chat_test.py`
- **Body 模板：** `/tmp/qw_web/baseprompt_clean.json`
- **Java 参考：** `github.com/cubk1/qoder2api`
- **Python 参考：** `github.com/bzym2/QoderGateway`
- **JS 原始：** `/tmp/qw_extract/app/resources/asar_out/out/main/main.js:2614427-2616623`
- **PAT 获取：** `/root/qoderwork/scripts/qoder_cn_pat_login.py`
