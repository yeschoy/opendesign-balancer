# opendesign-balancer

OpenDesign 账号池代理。对下游（new-api 等中转站）提供一把 API 密钥和 OpenAI / Anthropic 兼容接口，把请求分发到一组 OpenDesign 账号上，负责选号、会话粘性、并发与 RPM 控制、额度轮询、错误处理和日志，并带一个 Web 管理台。

单个 Go 二进制，无外部依赖，无数据库。

## 快速开始

```sh
go build -o odb ./cmd/odb
ODB_ADMIN_PASSWORD='至少8位的密码' ./odb -addr :8790 -data ./data
```

打开 `http://host:8790/`：

1. **账号 → 设备授权添加**：拿到验证码和激活链接，在**已登录该 OpenDesign 账号**的浏览器里打开并批准，账号连同 `sk-amr-` 运行密钥和 `vela_ctrl_` 控制密钥自动入池。也可以手动添加或批量导入（每行 `运行密钥[,控制密钥[,名称[,代理URL]]]`）。
2. **下游密钥 → 新建密钥**，得到 `sk-odb-…`（只显示一次）。要让脚本自动上号，再在同一页 **新建导入密钥** 得到 `sk-odbimp-…`，见“开放导入接口”。
3. 在 new-api 里新建渠道：类型 OpenAI（或 Anthropic Claude），Base URL 填本服务地址，密钥填 `sk-odb-…`，**打开“透传请求头”**。

不设 `ODB_ADMIN_PASSWORD` 时，第一次打开管理台会要求设置密码。服务暴露到公网前务必先设好密码。

| 环境变量 | 参数 | 默认 |
|---|---|---|
| `ODB_ADDR` | `-addr` | `:8790` |
| `ODB_DATA_DIR` | `-data` | `./data` |
| `ODB_ADMIN_PASSWORD` | | 仅在尚未设置密码时生效 |

### Docker 部署

```sh
cat > .env <<'X'          # 已在 .gitignore 中，权限 0600
ODB_ADMIN_PASSWORD=...     # 只在第一次启动、还没设密码时生效
ODB_UID=1000               # 容器以宿主用户运行，./data 属于该用户
ODB_GID=1000
X
mkdir -p data && chmod 700 data
docker compose up -d --build
```

- 容器加入共享网络 `ai-internal`（与 new-api 同一网络）。new-api 渠道的 Base URL 填 `http://opendesign-balancer:8790`。
- 管理台只绑定宿主机 `127.0.0.1:8790`，远程访问用 SSH 隧道 `ssh -L 8790:127.0.0.1:8790 主机`，或通过反向代理加 HTTPS。
- 数据在 `./data`，升级：`git pull && docker compose up -d --build`；回滚：切回旧版本后同样重建。
- 停止时会等待在途请求最多 30 秒（`stop_grace_period: 35s`）。

## 下游接口

| 路径 | 说明 |
|---|---|
| `POST /v1/chat/completions` | OpenAI Chat，原样转发到上游 |
| `POST /v1/responses` | OpenAI Responses，原样转发 |
| `POST /v1/messages` | Anthropic Messages，原样转发（错误按 Anthropic 格式返回） |
| `POST /v1/*` | 其他 POST 同样转发 |
| `GET /v1/models` | 上游模型列表，按分组缓存 10 分钟，并发请求合并为一次上游读取 |
| `POST /open/v1/accounts/import` | 开放导入接口（自动上号），用**导入密钥**鉴权，见下文 |

鉴权：`Authorization: Bearer sk-odb-…` 或 `x-api-key`。响应头带 `X-Request-Id`（可在请求日志里查）和 `X-Attempts`。

### 开放导入接口（自动上号）

给注册脚本或其他账号池用的接口，让新账号不经管理台直接入池。它用单独的一类密钥鉴权：在 **下游密钥 → 导入密钥 → 新建** 得到 `sk-odbimp-…`（只显示一次）。导入密钥只能导入账号，不能调用模型；下游密钥和管理员会话都不能导入。

```sh
curl -X POST http://host:8790/open/v1/accounts/import \
  -H 'Authorization: Bearer sk-odbimp-…' -H 'Content-Type: application/json' \
  -d '{"runtimeKey":"sk-amr-…","controlKey":"vela_ctrl_…","name":"账号A","group":"可选","note":"可选"}'
```

- 也可以用 `X-Import-Key` 头；批量用 `{"accounts":[…]}`，每次最多 200 个；`runtimeKey` 必填（也接受 `apiKey`/`key` 字段名），`controlKey`、`name`、`group`、`note`、`proxyUrl` 可选。不填 `group` 时用导入密钥的默认分组。
- **没有重复**：立即入池并启用，备注写入 `开放接口导入 <时间>（密钥 <导入密钥名>）` 和调用方给的 note，有控制密钥的马上刷新额度。
- **有重复**（运行密钥、控制密钥或名称与现有账号相同）：不写 `config.json`，而是放进 `data/staging.json` 暂存区，返回 202 和冲突的账号列表。管理台「账号」页底部的暂存区里可以**覆盖**所选的现有账号（换上新密钥和名称，备注追加覆盖时间，清除旧密钥造成的冷却/隔离）、**作为新账号添加**（仅当没有账号持有这把运行密钥，比如只是重名）或**丢弃**。同一把运行密钥再次提交只刷新暂存条目，不重复堆积；暂存区上限 500 条。
- 密钥、控制密钥、名称都和现有账号完全相同时返回 `unchanged`，不改任何东西，方便脚本幂等重跑。
- 响应：`{"added","staged","unchanged","failed","results":[{"index","status","accountId","stageId","conflicts","error"}]}`。全部成功 200，只有暂存的 202，全部失败 400，鉴权失败 401，同一 IP 鉴权连续失败 5 次后 60 秒内 429。请求体上限 4 MiB，未知字段拒绝。
- 日志只记账号 ID、名称、分组和导入密钥 ID，不记任何密钥。

### 返回给下游的状态码

账号层面的问题不会原样回给 new-api，否则 new-api 会把整个渠道判为失效：

| 情况 | 下游收到 |
|---|---|
| 上游 401/403（换号后仍失败） | 502 `upstream_account_auth_failed` |
| 模型滚动窗口用尽 `model_limit_exceeded`、套餐并发上限 `tier_limit_exceeded`（上游都是 **400**） | 429 + 按网关给的重置时间算的 `Retry-After` |
| 余额不足 `insufficient_balance`（402 或 429）、套餐不含该模型 `tier_model_not_entitled` | 503 |
| 网关自身供应商凭据故障 `upstream_provider_*`（上游 500） | 502，不扣账号健康分 |
| 其他上游 429 | 429 + `Retry-After` |
| 本地所有账号并发/RPM 满载 | 429 `pool_capacity` + 精确 `Retry-After` |
| 账号都在冷却（含该模型冷却） | 429 `pool_cooling_down` + 最早恢复时间 |
| 分组内没有可用账号 | 503 `pool_unavailable` |
| 上游把 400/422 包成 5xx | 还原为 400/422，不冷却账号 |
| 其他 4xx（参数错误等） | 原样透传 |
| 超时 / 连接失败 | 504 / 502 |

## 工作原理

### 选号

每个请求固定使用一份配置快照。流程：硬资格过滤 → 会话绑定命中 → 健康过滤（可选）→ 按优先级分组 → 组内按策略排序 → 并发/RPM 准入 → 取得租约。排序、准入、计数、绑定在同一把锁内完成，中间没有 I/O。

不可调度的账号：已禁用、被隔离、冷却中、该模型冷却中、额度快照证明这个模型没有任何来源能付费（见下文“额度”）。

策略（`routing.strategy`），额度相关的几种参考 [magpie](https://github.com/yetone/magpie) 的 `internal/routes`：

| 策略 | 规则 |
|---|---|
| `smart`（默认） | 余量充足的账号里，最长窗口最早重置的先用（按小时比较），因为剩余额度会在重置时作废；已用 ≥ `lowShare`(90%) 的排后，≥ `spentShare`(98%) 的再排后；再往后是靠套餐模型额度付费的（计费周期先结束的先用，因为不结转），只剩钱包可用的最后；无额度数据的排在已知账号之后 |
| `pace` | 周窗口剩余比例 ÷ 距重置小时数，越高越先；相差 10% 以内视为同档，按近期 token 打散 |
| `least-used` | 已用比例最低优先，其次近期 token |
| `weighted` | 按 `weight` 平滑加权轮询（NGINX smooth WRR） |
| `roundrobin` | 最久未被选中优先 |
| `least-selections` | 选中次数最少优先，平局按在途数 |
| `least-connections` | 在途数最少优先 |
| `ordered` | 按配置顺序，前一个不可用才用下一个 |

`priority`（1–100，越小越优先）先于策略生效：只有高优先级的账号都不可用或满载时才用低优先级的。

下游密钥属于一个分组（`group`），只调度同分组的账号，绝不跨组借号。

### 会话粘性

new-api 打开“透传请求头”后，本服务按以下顺序识别会话，取第一个有效值：

1. Responses 的 `previous_response_id`（上游按账号保存 response，必须回到同一账号；本服务会记录每个 response id 由哪个账号产生）
2. `routing.sessionHeaders` 里配置的自定义头
3. 父会话头：`X-Parent-Session-Id`、`X-Codex-Parent-Thread-Id`、`X-Claude-Code-Parent-Agent-Id`（父子 agent 落到同一账号）
4. 请求体：`prompt_cache_key`、`session_id`、`conversation_id`、`thread_id`（含 `metadata.` 下同名字段）、Claude Code 的 `metadata.user_id` 中的 session
5. 会话头：`X-Claude-Code-Session-Id`、`Session-Id`/`Session_id`、`X-Session-Id`、`X-Session-Affinity`、`Thread-Id`、`Conversation-Id`、`X-Conversation-Id`
6. 兜底：首条 system/developer 和首条 user 消息文本（各截 4096 字节）的 HMAC，弱粘性
7. 都没有：普通选号

注意：OpenDesign 网关自己注入 prompt cache，并按服务端目录加权挑选上游供应商密钥（不按用户），所以会话粘性不保证命中供应商缓存；它保证的是同一会话的计费、限额和 `previous_response_id` 落在同一账号。

会话值经 `HMAC-SHA256(routingSecret, …)` 生成指纹并按分组隔离，原值和指纹都不写日志。绑定表在内存中（LRU，上限 5 万条），显式会话 2 小时滑动过期，弱粘性 15 分钟。

绑定采用两阶段提交：未命中时先写“待确认”绑定，请求真正写到上游后才确认；请求未发出就失败时，只由创建它的请求删除。绑定账号忙时（`onBindingBusy`）：`overflow` 临时借用其他账号但不改绑；`wait-overflow` 先等待再借；`reject` 直接 429。绑定账号被禁用、冷却、隔离、额度耗尽、删除或更换密钥/代理时，绑定失效并改派。

### 并发与 RPM

每个账号可设 `maxConcurrent` 和 `maxRpm`（0 为不限）。网关返回 `tier_limit_exceeded`（如 `membership concurrency limit exceeded 6/5`、`rpm 61/60`）时，会自动记下套餐上限（这里是并发 5 / RPM 60），与手设值取较小者，点“恢复”清除。官方价格页写的“任务并发”（Go 10、Plus 20、Pro/Max 30）指 OpenDesign 应用内任务，和 API 并发上限不是一回事，所以不预设，以网关实际告知为准。准入顺序固定：并发 → RPM，并发满时不消耗 RPM。RPM 是精确的 60 秒滚动窗口：准入时预留，请求写到上游后计入窗口，此后无论成败都不退还；没发出去就失败时退还。租约释放幂等。

全部满载时最多等待 `concurrencyWaitMs`，超时返回本地 429 和精确的 `Retry-After`，记为 `errorCategory: capacity`，不计入健康样本。

客户端断开时中止上游请求，记 499，不写错误日志和健康样本。

### 换号与错误规则

每个请求最多尝试 `maxAccountsPerRequest`（默认 2）个账号，不会重复选同一个。只在**首包之前**、满足以下之一时换号：错误规则把账号冷却或隔离、上游 401、请求根本没发出去（连接/代理失败）。普通 5xx、超时不换号；**首包之后绝不重放**。

`errorRules` 有序、首条命中生效，匹配状态码、正文关键字（大小写不敏感，不支持正则）和响应头；动作是 `ignore` / `degrade` / `cooldown` / `hard-quarantine`，作用于账号或账号×模型。冷却时长依次取：响应头（如 `Retry-After`）→ 正文里 `bodyAfter` 短语后面的 RFC3339 时刻（如 `Try again after 2026-10-23T05:00:00Z`）→ `fallback`，不超过 `max`。`downstreamStatus` 指定这是最后一次失败时回给下游的状态码。未命中任何规则只记一次降级样本，**不会自动冷却**。

默认规则按 OpenDesign 官方客户端的错误目录（[nexu-io/open-design](https://github.com/nexu-io/open-design) `specs/current/run-error-catalog.md`、`apps/daemon/src/integrations/vela-errors.ts`）整理：

| 规则 | 匹配 | 动作 | 回下游 |
|---|---|---|---|
| `platform-provider-credentials` | `upstream_provider_unauthenticated/forbidden`（网关把自己的供应商 401/403 改写成 500） | 忽略：不扣健康分、不换号 | 502 |
| `account-suspended` | `account_suspended` | 隔离账号 | 503 |
| `runtime-key-invalid` | 401 + `invalid_api_key` / `unauthenticated` / `auth_required` | 隔离账号（重新授权后点“恢复”） | 502 |
| `model-window-limit` | `model_limit_exceeded`（HTTP 400，不扣费） | 该模型冷却到网关给的时刻，换号 | 429 |
| `membership-concurrency` | `tier_limit_exceeded` | 账号短冷却（≤1 分钟）并学习上限，换号 | 429 |
| `usage-limit-exceeded` | `usage_limit_exceeded`（API Key 美元用量上限） | 账号冷却，换号 | 429 |
| `insufficient-balance` | `insufficient_balance`、`余额不足`、`预扣费额度失败` 等 | 按额度来源冷却（Design Plan 模型→整号，套餐模型→该号全部套餐模型），10 分钟起逐次翻倍至 6 小时，成功清零；有控制密钥的同时刷新额度 | 503 |
| `tier-model-not-entitled` | `tier_model_not_entitled` / `tier_request_kind_not_entitled` | 该模型冷却 24 小时 | 503 |
| `rate-limited` | 其他 429 | 账号冷却（按 Retry-After，30 秒～5 分钟） | 429 |
| `forbidden` | 其他 403 | 账号冷却 5 分钟 | 503 |

已部署的实例升级后，在“路由设置”点“恢复默认规则”再保存，才会换成新规则。

`retryRules` 命中（默认 400/404/413/422）后立即停止换号，原样返回，避免一个用户的坏参数扩散成全池失败。但如果同一个响应已被错误规则判为账号问题（冷却或隔离），说明不是请求的错，照常换号：上游的 `model_limit_exceeded`、`tier_limit_exceeded` 都是 400。

### 流式

等到第一个完整 SSE 事件并确认不是错误事件后才开始向下游写，因此首包前的错误仍可换号。首包超时 120 秒，空闲超时 360 秒，开始转发后每 25 秒在事件边界写一行 `: PING` 心跳。

### 额度

依据官方[模型额度说明](https://open-design.ai/zh/model-credits/)、[价格页](https://open-design.ai/zh/pricing/)和官方客户端规范 `specs/current/coding-plan-client-billing.md`：

- 模型分两类。**Design Plan 模型**（DeepSeek V4/V4.1、MiMo、GLM-5.3 Flash、Kimi K2.7 Code、Claude Haiku 5.5、GPT-6 Luna 等）先扣 Design Plan 7 天窗口（Go 还有 5 小时窗口），用尽后扣**套餐模型额度**。**套餐模型**（Claude Sonnet/Opus/Fable、GPT-6 Sol/Astra、Gemini、Grok、图片模型等）不走窗口，直接扣套餐模型额度。两者都用尽后请求停止，除非开了自动充值或钱包有余额。
- **套餐能调哪些模型**，以每把 API Key 自己的 `/v1/models` 为准：套餐外的模型不在列表里，或标 `enabled:false`，官方客户端也是按它锁模型的。本服务每 10 分钟读一次（免费，没有控制密钥的账号也读），不在某账号列表里的模型不会派给该账号；分组里所有账号都不支持时，直接回 503 `model_not_entitled`，不打上游。Go 档没有套餐模型，也会排除。下游 `/v1/models` 只返回分组内至少一个账号能调用的模型。套餐外的模型实测是直接不出现在列表里：Go 档的 Key 只列出 12 个 Design Plan 模型；Plus 档 39 个；Pro/Max 档 42 个，多出 `claude-fable-5.1`、`claude-opus-5.5`、`gpt-6-astra`，和价格页一致。用 Key 自己的列表判断，套餐调整时不用改代码。设置里的 `ignoreCatalog` 可以关掉这项检查。
- 一个模型属于哪类，以 `billing/preflight?modelId=` 的 `modelCovered` 为准（`true` 为 Design Plan 模型）。本服务对最近被请求过的模型逐账号查询并缓存 6 小时。价格页的限时活动（如“GPT-6.1 Sol 已加入 Design Plan”、DeepSeek 谷价）**只在 OpenDesign 应用内生效，不适用于 API**，实测 API 下 `gpt-6.1-sol` 的 `modelCovered` 为 `false`。
- 有控制密钥的账号每 60 秒读一次工作区、套餐窗口、钱包和账单摘要（`balances.subscriptionCredits` 是套餐模型额度，`rechargeCredits` 是充值额度），全局并发 2，失败按 60s×2ⁿ 退避（上限 15 分钟）。聊天请求不等待额度刷新。各项分开保存，不相加、不用 `creditsPerUsd` 换算。某一项读取失败时保留上次成功值，不写成 0；控制密钥 401/403 时标记“控制密钥失效”，该账号按额度未知处理。快照超过 15 分钟视为过期。
- 只有拿得出证据时才把账号排除：该模型能用的窗口没有余量，且账单摘要（新鲜的）显示套餐模型额度为 0，且钱包和充值额度为 0（或关闭了 `walletFallback`）。官方规范明确说“钱包为 0 不能证明请求无法开始”，摘要读不到时不排除，交给真实的 `insufficient_balance` 错误处理。

概览页的窗口剩余按工作区去重求和，多个账号属于同一工作区时只算一次，账号列表里会标“共享工作区”。

### 只有 API Key 的账号

只填 `sk-amr-` 运行密钥、不填控制密钥完全可用，只是看不到剩余额度：

- **能调哪些模型**：照样按这把 Key 自己的 `/v1/models` 判断（不需要控制密钥）。
- **扣哪份额度**：有任一带控制密钥的账号时按 preflight 判断模型类别，否则用内置的 Design Plan 模型清单（价格页列出的 12 个，也是 Go 档 Key 的完整模型列表），清单外、但出现在某把 Key 模型列表里的算套餐模型。
- **额度用完**：靠上游错误推断。`insufficient_balance`（余额不足）出现在 Design Plan 模型上，说明窗口、套餐模型额度、钱包都空了，整个账号冷却；出现在套餐模型上，只冷却这个账号的全部套餐模型，Design Plan 模型照常用。冷却从 10 分钟起，同一账号连续命中逐次翻倍，最长 6 小时，成功一次清零。网关的余额预检在推理前拒绝，不扣费，所以探测是免费的。`model_limit_exceeded` 自带重置时刻，按它精确冷却。
- **选号**：额度未知的账号排在额度已知、还有余量的账号之后；全是这种账号时按选中次数和在途数平均分配。
- **界面**：显示“仅 API Key”、近 24 小时请求数和 token、推断出的额度耗尽冷却和连续失败次数。要看真实额度，用“重新设备授权”补上控制密钥。

### 模型价格与费用
“路由设置 → 模型价格”按每百万 token 配置各模型的输入、缓存命中、输出价格（厂商原币种，不换算），默认已按 2026-10-23 查到的官方价格填好；备注里写“请核对”的不是从官方页读到的。请求日志据此给每笔请求算费用并显示缓存率，概览显示 24h 费用（人民币、美元分开）。
- DeepSeek 谷峰计费：价格填的是空闲价，“高峰倍数”填 2；北京时间工作日 9-12、14-18 按倍数计，法定节假日在“高峰例外日”里填日期。
- 同一模型可配多行，“生效日期”到了的最新一行生效（用于已公布的调价，如 gemini-4-argon 12-01 起翻倍）。
- 长上下文：提示 token 达到阈值时整个请求按“长”价。缓存写入 token 上游没有给出，不计。
- 未配价格的模型、没有用量的请求不计费用。

### 日志与统计

- 请求日志 `data/logs/requests-*.jsonl`：每个请求一行终态记录（账号、选号原因、绑定结果、状态、上游状态、耗时、首字时间、token、每次尝试）。
- 错误日志 `data/logs/errors-*.jsonl`：每次失败的上游尝试一行，用 `requestId + attemptIndex` 与请求日志关联；原因先脱敏，再截断到 16 KiB，非 JSON 正文只记字节数。
- 5 MiB 分段，单条 ≤ 64 KiB，保留 30 天、合计 ≤ 100 MiB，写入走异步队列，写盘失败时丢弃并计数，不影响模型流量。
- 请求日志的 `protocol` 字段标明入站协议：`openai-chat` / `openai-responses` / `anthropic-messages` / `openai-completions` / `openai-embeddings`，可按协议筛选。
- 详细日志 `data/logs/details-*.jsonl`：**默认关闭**（路由设置 → 日志 → 详细日志）。开启后每个请求记客户端侧的请求头、请求体、响应头、响应体（流式记原始 SSE），每个体截断到“单个体上限”（默认 64 KB），保留条数默认 500。`Authorization`/`x-api-key`/`Cookie` 等头整体打码，其余内容过一遍密钥脱敏，但提示词和模型输出会明文落盘，排查完请关闭并清空。
- 除开启详细日志外，日志不包含：任何密钥、代理 URL、请求头值、请求/响应正文、会话原值或指纹、账号备注。

统计在请求结束时写入分钟桶（24 小时），不从日志推导。

## 数据目录

| 文件 | 内容 |
|---|---|
| `config.json` | 账号（含密钥）、下游密钥与导入密钥的哈希、设置、规则、管理员密码哈希（PBKDF2-SHA256） |
| `staging.json` | 开放导入接口暂存的重复账号（含密钥），等待管理台确认覆盖、添加或丢弃 |
| `metadata.json` | 账号状态（冷却/隔离）、健康样本、额度快照、统计、`routingSecret`。冷却、隔离、额度和配置变化 15 秒内写盘；只有统计和健康样本变化时最多每分钟写一次 |
| `selection-counters.json` | 选号计数，最多每 5 秒写一次 |
| `logs/` | 请求与错误日志，缓冲写入，每秒刷盘；查询读内存，不受影响 |

所有文件 0600、目录 0700，写盘先写临时文件再 rename。JSON 损坏时启动失败并保留原文件。**整个目录都是机密，不要提交或共享。**

`config.json` 也可以手工编辑，然后 `kill -HUP <pid>` 或管理台调用 `/api/config/reload` 热加载，在途请求继续使用旧快照。

### 主要配置项（`routing`）

| 字段 | 默认 | 说明 |
|---|---|---|
| `strategy` | `smart` | 见上表 |
| `sticky` | `true` | 会话粘性 |
| `onBindingBusy` | `overflow` | `overflow` / `wait-overflow` / `reject` |
| `explicitTtlSec` / `fallbackTtlSec` | 7200 / 900 | 绑定滑动过期 |
| `fallbackHash` | `true` | 无会话标识时按消息哈希弱粘性 |
| `sessionHeaders` | `[]` | 额外会话头 |
| `concurrencyWaitMs` | 2000 | 满载等待，0–30000 |
| `maxAccountsPerRequest` | 2 | 1–5 |
| `ignoreCatalog` | `false` | 不按各 API Key 的模型列表判断套餐权限 |
| `walletFallback` | `true` | 窗口和套餐模型额度都用尽后，是否允许只剩钱包/充值额度的账号付费（真金白银） |
| `lowShare` / `spentShare` | 90 / 98 | 已用达到多少 % 开始靠后 / 视为耗尽（按已用百分比，不是剩余） |
| `healthFilter` / `minimumHealth` | `false` / 0.2 | 24h 成功率过滤，样本少于 20 视为健康 |
| `firstEventTimeoutSec` / `idleTimeoutSec` / `heartbeatSec` | 120 / 360 / 25 | 流式 |
| `requestTimeoutSec` | 900 | 非流式总超时 |
| `maxBodyMB` | 64 | 请求体上限 |

`quota`：`enabled`(true)、`intervalSec`(60)、`concurrency`(2)、`staleSec`(900)。`logs`：`retentionDays`(30)、`maxRequests`(50000)、`maxErrors`(10000)、`maxTotalMB`(100)。账号：`maxConcurrent`、`maxRpm`、`weight`(1–100)、`priority`(1–100，默认 50)、`proxyUrl`（http/https/socks5/socks5h，代理失败不会回退直连）。

## 限制

- **单进程**。并发计数、RPM 窗口、会话绑定都在内存中，不跨进程共享，不要多副本部署同一个数据目录。重启后会话绑定清空（下一轮请求会重新绑定）。
- 设备授权的批准必须在目标账号已登录的浏览器里完成；本服务不接触 cookie。
- 控制面读不到是否开了自动充值。开了自动充值、但窗口和各项额度都为 0 的账号会被判为额度耗尽而不调度。
- `model_limit_exceeded`、`tier_limit_exceeded` 的正文格式来自官方客户端源码，尚未在本账号上真实触发过，见 [docs/upstream-findings.md](docs/upstream-findings.md)。

## 回滚

停掉进程，换回旧二进制，用同一个数据目录启动即可。升级前建议备份 `data/config.json` 和 `data/metadata.json`。`metadata.json` 或 `selection-counters.json` 损坏时可以删除（会丢失冷却状态、健康样本、统计和计数，并生成新的 `routingSecret`，已有会话绑定失效），`config.json` 不能删。

## 开发

```sh
go vet ./... && go test -race ./...
```

集成测试用临时目录和本地 mock 上游，不使用真实凭据。
