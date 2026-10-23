# OpenDesign 上游实测记录

2026-10-23（北京时间）用一个 Design Plan Plus 账号实测。只发了免费请求：模型列表、空请求体、控制面只读接口、设备授权申请（未批准，自动过期）。没有发送付费推理请求。

## 运行面 `https://amr-link.open-design.ai/v1`（`sk-amr-` 运行密钥）

| 请求 | 结果 |
|---|---|
| `GET /models` | 200，39 个模型，`id` 即调用名，`metadata.cost/capability` 是档位不是额度 |
| `POST /chat/completions` 空体 | 400 `{"error":{"code":"invalid_model","message":"[code=invalid_model] model is required","type":"invalid_model"}}` |
| `POST /responses` 空体 | 400，同上 |
| `POST /messages` 空体，无 `anthropic-version` | 400 `{"type":"error","error":{"type":"invalid_request_error","message":"anthropic-version header is required"}}` |
| `POST /messages`，`Authorization: Bearer` 或 `x-api-key` | 都通过鉴权（返回 400 model is required） |
| 错误的运行密钥（OpenAI 入口） | 401 `{"error":{"code":"invalid_api_key","message":"[code=invalid_api_key] api key not found","type":"invalid_api_key"}}` |
| 错误的运行密钥（`/messages`） | 401 `{"error":{"message":"api key not found","type":"authentication_error"},"type":"error"}` |

结论：三个入口都存在，代理按路径原样透传即可，不需要协议转换。上游没有 provider/渠道层，错误规则只有账号和账号×模型两个维度。

## 控制面 `https://amr-api.open-design.ai`（`vela_ctrl_` 控制密钥）

字段与交接文档一致：

- `GET /api/v1/workspaces/current`：`workspaceId`、`activeWorkspaceId`、`workspaceMemberId`、`personalMembershipTier`
- `GET /api/v1/workspaces/{ws}/billing/preflight`：`codingPlan.windows[]` 的 `usedCredits/limitCredits/remainingCredits` 是字符串；不带 `modelId` 时 `funding=gateway`、`modelCovered=null`
- `GET /api/v1/billing/summary`：`balances.*Credits`、`*Usd` 都是字符串，`usageCount` 是数字
- `GET /api/v1/workspaces/{ws}/wallet/balance`：`balanceUsd`
- 错误的控制密钥：401 `{"error":"unauthenticated"}`

## 设备授权

- `POST /api/v1/cli/device-authorizations` → 201，含 `deviceId`、`deviceSecret`、`userCode`、`activationUrl`、`pollIntervalSeconds=2`、`expiresAt`（约 15 分钟）
- 批准前 `POST …/{deviceId}/token` → 200 `{"status":"pending","controlKey":null,"runtimeKey":null,"user":null}`
- `deviceSecret` 错误 → 401 `{"error":"invalid_device_secret"}`

## 2026-10-23 补充：官方资料与交接文档的出入

来源：[模型额度说明](https://open-design.ai/zh/model-credits/)、[价格页](https://open-design.ai/zh/pricing/)、官方开源客户端 [nexu-io/open-design](https://github.com/nexu-io/open-design)（`specs/current/run-error-catalog.md`、`specs/current/coding-plan-client-billing.md`、`apps/daemon/src/integrations/vela-errors.ts`、`packages/contracts/src/runtime/model-window-limit.ts`、`packages/contracts/src/runtime/membership-concurrency-limit.ts`，commit `e03d5c2`）。

交接文档中需要更正的地方：

1. “套餐窗口用尽后改扣钱包”：不对。Plus/Pro/Max 先扣 Design Plan 额度，用尽后扣**套餐模型额度**（摘要里的 `subscriptionCredits`），都用尽才停止，除非开了自动充值。Go 没有套餐模型额度。
2. “钱包 > 0 或 Design Plan 有额度才能调用”：不完整。还要算套餐模型额度；而且**套餐模型**（Claude Sonnet/Opus/Fable、GPT-6 Sol/Astra、Gemini、Grok、图片模型）根本不扣 Design Plan 窗口。官方规范还写明“钱包为 0 不能证明请求无法开始”。
3. 模型类别要逐模型查 `preflight?modelId=`。实测（只读）：`deepseek-v4.1-flash`、`claude-haiku-5.5`、`gpt-6-luna` 为 `coding_plan / modelCovered:true`；`claude-sonnet-5.5`、`gpt-6-sol`、`gpt-6.1-sol`、`claude-opus-5`、`claude-fable-5`、`gemini-3.8-flash`、`gpt-image-2` 为 `gateway / modelCovered:false`。价格页说 GPT-6.1 Sol 已加入 Design Plan，但那是应用内限时活动，官方写明“不适用于 MCP、CLI、API”。
4. `model_limit_exceeded` 是 **HTTP 400** + `Retry-After`，正文 `You have reached the 5-hour usage limit for <model>. Try again after <RFC3339>. This request was not charged to Wallet Credits.`，不是额度耗尽，到点即恢复。
5. `insufficient_balance` 官方目录记为 429（`HTTP 429: {"code":"insufficient_balance",...}`）；本账号实测为 **402**：`{"error":{"code":"insufficient_balance","message":"[code=insufficient_balance] Insufficient wallet balance. Please top up your wallet before retrying.","type":"insufficient_balance"}}`（调用 `claude-sonnet-5.5`、空消息体，网关在推理前预检拒绝）。两种都要处理。也有中文“预扣费额度失败 / 余额不足”。
6. 交接文档没提到的错误码：
   - `tier_limit_exceeded`（HTTP 400）：套餐并发/RPM 上限，正文如 `membership concurrency limit exceeded 6/5`、`rpm 61/60`，可能带 `resets at <RFC3339>`
   - `usage_limit_exceeded`（429）：API Key 美元用量上限
   - `tier_request_kind_not_entitled`：套餐不含该请求类型
   - `account_suspended`：封号
   - `upstream_provider_unauthenticated` / `upstream_provider_forbidden`（HTTP 500，“Upstream provider credentials are missing or invalid.”）：网关自己的供应商凭据坏了，与调用方账号无关
   - 运行密钥失效、暂停、移出团队、工作区锁定、客户端过旧可能是 401/403/409/426
7. 5 小时窗口：只有 Go 有；Plus/Pro/Max 只有 7 天窗口，从订阅开始时间起每满 7 天重置。
8. Plus 的套餐模型不含 GPT-6 Astra、Claude Fable 5.1、Claude Opus 5.5（会返回 `tier_model_not_entitled`）。
9. 网关自己注入 prompt cache，按服务端目录加权选择上游供应商密钥，不按用户，所以账号粘性不保证命中供应商缓存。

10. 套餐能调用哪些模型：每把运行密钥的 `/v1/models` 带 `enabled` 字段，官方客户端（`apps/web/src/components/agentModelSelection.ts`）注明套餐外的模型为 `enabled:false`。2026-10-23 读取 9 把 Key：套餐外的模型不出现在列表里（没见到 `enabled:false`）。Go 档 12 个（只有 Design Plan 模型），Plus 档 39 个，Pro/Max 档 42 个（多出 `claude-fable-5.1`、`claude-opus-5.5`、`gpt-6-astra`），与价格页一致。`claude-opus-5`、`claude-fable-5` 是另外的旧版本 ID，Plus 也能列出。

## 仍未验证

- `model_limit_exceeded`、`tier_limit_exceeded`、`usage_limit_exceeded` 没有在本账号上真实触发过，格式取自官方客户端源码和测试。
- 流式响应中途出错的事件格式。代理把 `event: error`、`event: response.failed`、顶层带 `error` 的 data 都当作错误事件。
- 自动充值是否开启无法从控制面读到。开了自动充值的账号在窗口、套餐模型额度、钱包都为 0 时仍能付费，但本服务会把它判为额度耗尽而排除；目前没有针对这种账号的开关。
