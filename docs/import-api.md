# 账号导入接口

`POST https://ods.yeschoy.io/open/v1/accounts/import`

把 OpenDesign / River / Ollama 账号直接写入账号池。只用导入密钥 `sk-odbimp-…`（管理台「下游密钥 → 导入密钥」创建）。下游密钥 `sk-odb-…` 和管理员登录都不能调用。

```sh
curl -X POST https://ods.yeschoy.io/open/v1/accounts/import \
  -H 'Authorization: Bearer sk-odbimp-…' \
  -H 'Content-Type: application/json' \
  -d '{"runtimeKey":"sk-amr-…","controlKey":"vela_ctrl_…","name":"账号A"}'
```

鉴权也接受 `X-Import-Key: sk-odbimp-…`。密钥停用后立即失效。

## 请求

单条：字段放在根对象。批量：`{"accounts":[…]}`，最多 200 条，此时忽略根上的账号字段。`{"accounts":[]}` 按单条处理。请求体上限 4 MiB。未知字段整单拒绝。

| 字段 | 必填 | 约束 |
|---|---|---|
| `runtimeKey` | 是 | ≤ 512 字符。也接受字段名 `apiKey`、`key`。OpenDesign 形如 `sk-amr-…`、River 以 `rv_` 开头；Ollama 的 key 没有固定形态（自建服务填任意值如 `ollama`），此时必须带 `provider` |
| `provider` | 否 | `opendesign` / `river` / `ollama`。不填时由 `runtimeKey` 的前缀推断（`sk-` / `rv_` / `oll-`），前缀识别不出的 key 才会用到它 |
| `controlKey` | 否 | 只有 OpenDesign 有。有值时必须以 `vela_ctrl_` 开头，≤ 512 字符。有它才会自动刷新额度；River 和 Ollama 账号不能带 |
| `name` | 否 | ≤ 50 字符 |
| `group` | 否 | 不填则用这把导入密钥的默认分组；默认分组是 `default` 时按号池改为 `river` / `ollama` |
| `note` | 否 | ≤ 400 字符。入池时会加上导入时间和密钥名 |
| `proxyUrl` | 否 | `http` / `https` / `socks5` / `socks5h`，必须带 host，不能有空白 |

## 响应

```json
{
  "added": 1,
  "staged": 0,
  "unchanged": 0,
  "failed": 0,
  "results": [
    {"index": 0, "status": "added", "accountId": "acc_…", "name": "账号A"}
  ]
}
```

`results[i]` 对应请求里的第 i 条。

| `status` | 含义 | 额外字段 |
|---|---|---|
| `added` | 已入池并启用 | `accountId` |
| `unchanged` | 运行密钥、控制密钥、名称都与现有账号一致，未改动。可重复提交 | `accountId`、`conflicts` |
| `staged` | 与现有账号冲突，未写入账号池，进入暂存。同一把运行密钥再提交只刷新这一条 | `stageId`、`conflicts` |
| `error` | 本条校验失败，其他条继续处理 | `error` |

`conflicts[]`：`accountId`、`name`、`email`、`group`、`reason`（`runtimeKey` | `controlKey` | `name`）。不含密钥。

冲突不会自动覆盖。管理员在管理台处理暂存（覆盖、作为新账号添加，或丢弃）。调用方把 `staged` 当成已接收即可。

| HTTP | 何时 |
|---|---|
| 200 | 至少一条 `added` 或 `unchanged`（可同时有 `staged` / `error`） |
| 202 | 没有入池或未变化，但有 `staged` |
| 400 | JSON 非法、未知字段、超过 200 条，或全部为 `error`。正文 `{"error":"…"}` |
| 401 | 密钥缺失、错误或已停用 |
| 429 | 同一 IP 鉴权连续失败 5 次，60 秒内拒绝。`Retry-After: 60` |

单条失败示例：`{"index":0,"status":"error","name":"b2","error":"runtimeKey must start with sk-amr- (OpenDesign) or rv_ (River); an Ollama key has no prefix and needs \"provider\":\"ollama\""}`。常见 `error`：`runtimeKey is required`、`runtimeKey must start with …`、`provider must be opendesign, river or ollama`、`ollama accounts have no controlKey`、`controlKey must start with vela_ctrl_`、`name longer than 50`、`note longer than 400`、`proxyUrl …`、`暂存区已满（500 条），请先在管理台处理`。

## Ollama 示例

Ollama 的 key 没有可识别的前缀，显式声明号池即可（同一把 key 既能推理，也能读 `/api/balance` 余额，所以没有控制密钥）：

```sh
curl -X POST https://ods.yeschoy.io/open/v1/accounts/import \
  -H 'Authorization: Bearer sk-odbimp-…' \
  -H 'Content-Type: application/json' \
  -d '{"accounts":[
        {"runtimeKey":"<ollama API key 1>","provider":"ollama","name":"ollama-1"},
        {"runtimeKey":"<ollama API key 2>","provider":"ollama","name":"ollama-2","note":"团队B"}
      ]}'
```

入池后账号进 `ollama` 组；下游密钥的分组填 `ollama` 才会调度到它们。
