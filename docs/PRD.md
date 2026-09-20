# PRD：Codex Turn-State Cache —— 292 State 全自动注入插件

> 参考文章：《292 State 注入 — Codex 不降智、不 Overload 的底层原理与实现》
> （blog.caowo.de，2026-09-18，归类：技术分析 / 逆向工程）
> 交叉参照：`cpa-plugin-codex-turn-state`（arden-aaai）仓库的 FINDINGS.md 与实现。

---

## 1. 机制摘要（文章 + FINDINGS 交叉修正）

文章讨论社区中 ChatGPT/Codex 体验集体恶化的三类问题：**降智**（输出质量断崖
下跌）、**Overload**（cloud agent 频繁过载排队）、**429 限流**（远未触及速率
上限却被限流）。

### 1.1 292 / 312 的真实语义（重要修正）

`292` / `312` **不是 HTTP 状态码**，而是 `X-Codex-Turn-State` 响应/请求头的
**头值长度**（base64 字符数）：

| 状态 | base64 长度 | 解码字节 | 含义 |
| --- | ---: | ---: | --- |
| 正常态 | 292 | 217 | 可复用的"模板"（通行证） |
| 降级态 | 312 | 233 | 被限流/降级的标记，**不可复用** |

长度差正好 16 字节 = 一个 AES-CBC 块：降级态是同一 Fernet 结构多带一块密文。
这是从外部区分两种状态的唯一可靠信号。

### 1.2 Fernet 令牌结构（FINDINGS.md）

`X-Codex-Turn-State` 是 base64url 的 Fernet 令牌：

```
0x80 (1B 版本) | ts (8B 大端 Unix 秒，签发时间) | IV (16B)
| ciphertext (AES-CBC) | HMAC (32B)
```

- 签发时间**无需密钥即可读取**。
- 有效期约 1 小时，**从令牌内嵌时间戳起算，不是代理捕获时刻**——签在令牌
  内部，过期上游直接拒绝（`Encrypted content could not be decrypted`），
  不会静默忽略。
- 令牌每 turn 新签，同一值重复出现即重放。

### 1.3 复用规则（FINDINGS 总结的四条硬规则）

1. state **绝不**跨账号复用；
2. state **绝不**跨模型复用（同账号内也不行）；
3. 同账号同模型**可跨 IP** 复用；
4. TTL 从令牌自身时间戳起算。

### 1.4 原方案（文章的 codex-state-kit）流程

```
采集：Clash 切住宅/原生 V6 → keeper 发一条 gpt-6-astra 请求
      → 提取 state 写入文件 → 切回日常线路
使用：inject_proxy 拦截 Codex 出站请求 → 注入 state
      → 收到 312（降级态）时通知 keeper 立即续期
      → 到期前 5 分钟定时自动续期（keeper ~45s 轮询）
```

已知限制：312 后采不到新 292 存在空窗期；模型必须对齐；state 与账号绑定。

---

## 2. 两仓库对比核对（谁实现得全）

| 能力 | cpa-plugin-codex-turn-state（对方） | codex-turn-state-cache（本仓库 v0.3.0） |
| --- | --- | --- |
| 292 模板采集 | ✅ probe 角色，落盘 store | ✅ 被动 in-band 内存采集 |
| 312 降级态识别 | ✅ 长度判定 + 降级日志 | ✅ 计数、决策日志、永不缓存 |
| 312→292 替换（文章核心） | ✅ replace-only / always 两种模式 | ✅ replace-only / always 两种模式 |
| TTL 基准 | ✅ 令牌内嵌 Fernet 时间戳，拒绝未来时间戳 | ✅ 严格 Fernet 结构、时间戳与 30 秒安全余量 |
| 采集/使用隔离 | ✅ probe/business 双角色 + 落盘 | ➖ 单进程 in-band（被动续采，无需角色切换，是架构优势） |
| 请求关联防错 | ❌ 无 pending 机制 | ✅ requestID 绑定，完成的请求不可再写入 |
| 降级观测（312 监控线） | ✅ 降级事件日志 | ✅ 降级计数 + 日志 |
| 管理 API + 看板 | ✅ status/clear/selftest + 看板页面 | ✅ status/clear/probe/delete/reveal + 看板页面 |
| 决策日志 | ✅ harvest/substitute/pass/skip | ➖ captured/injected 两种 |
| dry_run / 配置校验 | ✅ | ➖ 无需（配置仅两个容量项） |
| 落盘持久化 | ✅ 0700/0600 原子写 | ✅ 仅采集事件 JSONL；state 仍纯内存（重启即清，隐私更优） |

**结论：对方仓库对文章机制的覆盖明显更全**，尤其是 312 降级替换与 Fernet TTL
这两条核心；本仓库的优势是极简被动架构（in-band 持续续采、无需 probe 进程与
角色切换、内存不落盘）。**本次拓展把对方已验证的核心机制移植进本仓库的被动
架构，并补上看板。**

---

## 3. 已实现拓展（v0.3.0）

保持本仓库"零额外进程、零人工干预、内存不落盘"的定位，新增：

### R-A 312 降级态替换（最高优先级）

- 请求侧：出站请求已携带 `X-Codex-Turn-State` 且长度 == 312（降级态），同时
  对应桶 (auth, model) 有未过期的 292 模板 → 用模板替换该头。
- 不携带此头的请求**不强灌**（与对方仓库拍板的 replace-only 口径一致）；
  现有"有模板即注入"的行为保留为 `inject_mode: always`（默认，维持向后兼容）。
- 新增 `inject_mode` 配置：`always`（默认） / `replace-only`，其他值拒绝启动。

### R-B TTL 改为令牌内嵌时间戳

- 采集 292 时解码 Fernet 时间戳作为 issuedAt，过期 = issuedAt + 3600s。
- issuedAt 在未来（时钟偏移/文件篡改）→ 拒绝使用该模板。
- 无效、非规范或临近过期的 Fernet 结构 → 拒绝缓存。

### R-C 降级观测

- 响应侧捕获到 312 长度值：不存储，计数 + 日志
  `codex turn-state cache degraded state=observed`（降级监控线，对应文章
  "312 是撤销信号"）。

### R-D 管理 API + 看板页面

- `GET /v0/management/codex-turn-state-cache/status`（鉴权）：桶就绪度、
  issued/expires、计数器、inject_mode、版本。
- `POST /v0/management/codex-turn-state-cache/cache/clear`（鉴权）：清空缓存。
- `/v0/resource/plugins/codex-turn-state-cache/dashboard`（外壳，零数据）：
  浏览器内带管理密钥调 status/clear。
- 数据路由**绝不带 Menu 字段**（防止被降级注册到无鉴权 resource 前缀）；
  外壳路径必须是具名子路径 `/dashboard`（不能是 `/`，否则注册被静默丢弃）。
- 看板显示效果：卡片式计数器、桶表格带剩余寿命进度条、明暗主题自适应、
  `#demo` 哈希内置演示数据（供预览截图）。

### 隐私红线（沿用）

- 宿主日志不含 state 值、账号 ID、IP。
- 看板不显示 state 值；账号 ID 仅出现在鉴权后的 status 接口。
- 312 值永不入缓存。

---

## 4. 验收标准

1. `go test ./...`、`go vet ./...` 全绿（含 cgo 标签测试）。
2. 312 长度请求 + 活模板 → 替换；无头请求在 replace-only 下不动；
   429/200/取消等不影响缓存。
3. 临期采集的模板（issuedAt 早于捕获时刻）按令牌时间戳过期。
4. 产出并验证 Linux/amd64 `dist/cpa-plugin-codex-turn-state-v0.3.0.so` 及 SHA-256 校验文件。
5. 看板可在浏览器打开（demo 模式）并截图展示。
