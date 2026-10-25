---
last_updated: 2026-10-25
source_files:
  - internal/server/handler.go
  - internal/server/messages.go
  - internal/server/responses.go
  - internal/server/images.go
  - internal/server/search.go
  - internal/server/fetch.go
  - internal/server/busy.go
  - internal/server/capacity_test.go
  - internal/server/metrics_prom.go
  - internal/upstream/client.go
  - internal/upstream/headers.go
  - internal/panel/panel.go
kb_version: KB-v1.0
---

# 服务层与IPC通信

Go 项目没有 IPC，对应物是**三类接口契约**：对外 HTTP 入口（客户端 → 网关）、上游 HTTP 出站（网关 → CodeBuddy）、内部 Go 包接口（pool/session/store 的注入面）。

## 一、对外 HTTP 入口全表

注册处 `internal/server/handler.go:192-220`。未启用（`admin`/`metrics`）时**路径根本不注册** → 404。

| 方法 + 路径 | handler | 鉴权 | 说明 |
|---|---|---|---|
| `POST /v1/chat/completions` | `chatCompletions` | Bearer | 主链路，详见 [聊天补全全链路](../03-核心数据业务流/聊天补全全链路.md) |
| `POST /v1/responses` | `responses` | Bearer | Responses 入站 → chat → 回写 `response.*` 事件流；不保存 `previous_response_id`/`store` |
| `GET /v1/responses` | `responsesWebsocketUnsupported` | Bearer | 固定 **426** + `Upgrade: websocket`（让 Codex 立刻粘性回落 HTTPS） |
| `POST /v1/images/generations` | `images(...,false)` | Bearer | 文生图，仅 cn 域 |
| `POST /v1/images/edits` | `images(...,true)` | Bearer | 图生图，唯一接受 multipart 的端点 |
| `POST /v1/messages`、`POST /messages` | `messages` | `x-api-key` 或 Bearer | Anthropic Messages 入站 → chat → 回写 Messages JSON/SSE |
| `POST /v1/search` | `search` | Bearer | 上游 agenttool 检索透传；`deep:true` 走综述式 |
| `POST /v1/fetch` | `fetch` | Bearer | DeepSeek Fetch 形状网页抓取 |
| `POST /v2/chat/busy` | `chatBusy` | Bearer | 「模型忙」预检，**本地按池台账算，不打上游** |
| `GET /v1/models` | `models` | Bearer | 动态目录（CN 10min / global 1h） |
| `GET /v1/capacity` | `capacity` | Bearer（要求 api_key 非空） | 按模型可用容量，`Cache-Control: no-store` |
| `GET /status` | `status` | Bearer | 池汇总 + 每账号详情 + `model_locks` |
| `GET /healthz` | `healthz` | **无** | 200/503 + `realm_servable` + `X-Service` 头 |
| `/panel/` | `cfg.Panel` | 面板自带 Bearer | 面板为 nil 时不挂载 |
| `POST /admin/tasks/{name}/run` | `audit("task.run",…)` → `adminTaskRun` | Bearer | 需 `admin.enabled`；202 已启动 / 409 同名在跑 / 503 Tasks 为 nil |
| `GET /metrics` | `promMetrics` | Bearer | 需 `metrics.enabled`；Prometheus 文本 |

鉴权只认 `Authorization: Bearer <key>`（前缀大小写敏感）；Anthropic 路径额外接受 `x-api-key`（内部提升为 Bearer 再走同一校验）。`api_key` 为空 → 全部放行（`internal/httpauth/httpauth.go:26-43`）。

实测（空池、`api_key=smoke_key`）：

| 端点 | 结果 |
|---|---|
| `/healthz` | 503 `{"healthy":0,"total":0,"service":"workbuddy2api","realm_servable":{"cn":false,"global":false}}` |
| `/v1/chat/completions` | 503 `no_healthy_account` + `gateway_hint` |
| `/v1/images/generations` | 503 `no_account`（"没有可用的 cn 账号（图像只在 cn 域出图）"） |
| `/v1/messages` | 503 Anthropic 形状 `{"type":"error","error":{"type":"overloaded_error",...}}` |
| `GET /v1/responses` | 426 |
| `/v2/chat/busy` | 200 `{"code":0,"data":[],"msg":"OK","requestId":...}` |
| `/v1/capacity` | 400 `invalid_request_error`（缺 `model`） |
| `/metrics`（未启用） | 404 |
| `/admin/tasks/checkin/run`（未启用） | 404 |

## 二、上游出站端点

Base 与鉴权头风格见 [技术栈与中间件](../01-架构总览/技术栈与中间件.md)。

| 域 | 方法 + 路径 | base | 用途 |
|---|---|---|---|
| chat | `POST /v2/chat/completions` | chatBase | 聊天补全（SSE），CN/global 同路径 |
| refresh | `POST /v2/plugin/auth/token/refresh` | chatBase | token 刷新（独有 `X-Refresh-Token` 头） |
| 余额 | `POST /v2/billing/meter/get-user-resource`（global 优先无 `/v2`，404 回落） | billingBase | 余额查询 |
| 企业额度 | `POST /v2/billing/meter/get-enterprise-user-usage` | billingBase | 静态 Key/企业号 |
| 签到 | `POST /v2/billing/meter/daily-checkin`（global 优先无 `/v2`） | billingBase | 每日签到 |
| 签到活动 | `POST /v2/billing/meter/checkin-activity-status` | billingBase | 签到活动状态（只读） |
| 账单汇总 | `POST /billing/meter/get-user-resource-summary` / `-paid-packages` / `-free-packages` | billingBase | 积分构成页 |
| 活跃上报 | `POST /v2/report` | billingBase | 事件数组，必须含 `userId` |
| 任务 | `GET /v2/activity/growth/tasks`、`POST .../tasks/accept`、`POST /activity/growth/tasks/{code}/claim`（**web 域**） | chatBase / webBase | 成长任务 |
| 旅行 | `GET /activity/growth/buddy/travel/status`、`/buddy/info`；`POST .../travel/depart`、`.../travel/claim`、`/buddy/first`、`/buddy/agreement` | chatBase | 猫猫旅行 |
| 连登/抽奖 | `GET /activity/growth/streak`、`/lottery/summary`；`POST /activity/growth/redeem`、`/lottery/draw` | chatBase | 连登与抽奖 |
| 出图 | `POST /v2/images/generations`、`/v2/images/edits` | chatBase | 图像生成 |
| 检索 | `POST /agenttool/v1/search`、`/agenttool/v1/webfetch`、`/agenttool/v1/agentic_search` | chatBase | 联网检索/抓页/综述 |
| 模型目录 | `GET /console/enterprises/personal/models`、`GET /v3/config`、`GET /v2/enterprises/personal/models` | chatBase | CN/global 目录 |
| 繁忙度 | `POST /v2/chat/busy` | chatBase | 单批 ≤50 |
| 礼包/补偿 | `POST /billing/meter/claim-gift`、`/claim-compensation`、`/compensation-status`、`/check-gift-claimed` | billingBase | 夜猫子礼包与补偿 |
| trial | `POST /billing/ide/trial` | billingBase | 仅 global，`14051` 视为幂等 |
| global 注册 | `POST /billing/area/get-country-code`、`/console/login/account`；`GET /auth/realms/copilot/overseas/user/register` | global billing | global 号激活 |

出站头三族（`internal/upstream/headers.go`）：chat 族带会话/链路头（`X-Conversation-ID` / `X-Conversation-Request-ID` / `X-Request-ID` / `X-B3-*`，`:431-500`）；billing 族恒 `axios/1.16.1` UA + `X-CodeBuddy-Request: 1`（`:614-641`）；refresh 族独有 `X-Refresh-Token` + `X-Auth-Refresh-Source: plugin`（`:642-652`）。**聊天请求永不携带 `X-Refresh-Token`**。

## 三、面板 HTTP 入口

49 个 API + 3 个静态资源（共 52 条 `HandleFunc`），全部注册在 `internal/panel/panel.go:190-245`，pattern **自带 `/panel` 前缀**（外层 mux 不剥前缀）。按域分组：

| 域 | 端点 |
|---|---|
| 静态 | `GET /panel/{$}`、`/panel/theme.js`、`/panel/app.js`（均 `no-store`，脚本 URL 带内容指纹） |
| 观测 | `overview`、`logs`、`error_summary`、`request_metrics`、`request_logs`、`credit_history` |
| 模型 | `models`（含 `refresh=1`）、`model_probes` |
| 账号 | `accounts/add_apikey`、`import/cockpit`、`login/start`、`login/poll`、`login/regions`、`accounts/{uid}/{model_toggle,probe_ban,revive,disable,pause,resume,checkin,balance,remove,travel,compensation}` |
| 任务 | `accounts/{uid}/tasks`、`accounts/{uid}/tasks/job`、`tasks/jobs`、`tasks/queue`、`accounts/{uid}/tasks/{accept,accept_all,claim,auto,auto_all}`、`tasks/scan_all`、`tasks/run_queue` |
| 批量 | `checkin_all`、`travel_all`、`activity_all`、`keepalive_all`、`balance_all`、`compensation_all` |
| 用量/余额 | `usage`、`usage/save`、`packages`、`balance_history`、`school/vouchers` |
| 配置 | `GET /panel/api/config`、`POST /panel/api/config` |

安全头对页面、脚本、API、401 统一生效：CSP `default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; form-action 'none'; base-uri 'none'`，另有 `nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: no-referrer`、COOP/CORP（`internal/panel/index.go:67-79`）。

## 四、内部 Go 接口（注入面）

| 接口 / 函数类型 | 定义 | 实现方 |
|---|---|---|
| `auth` 文件读写 | `auth.LoadDir` / `Auth.SaveAtomic` / `SaveAtomicRetry` | `internal/auth` |
| `pool.StoreSnapshotter` | `SaveState([]byte)`、`LoadState() ([]byte, bool)` | `internal/redisstore`（Noop 或 Upstash） |
| `session.Config.Available` / `AvailableForModel` | `func() []string` / `func(model string) []string` | `pool.AvailableUIDs` / `realmAwareAvailableForModel` 闭包 |
| `upstream.CreditObserver` | `func(uid string, credits int64)` | `credithist.Observe` |
| `pool.SetModelRateOf` | `func(realm, model string) string` | `upstream.Client.ModelRate` |
| `scheduler.GrowthRunner` / `SweepRunner` | `func() (int, string)` | `panel.RunGrowthQueueNow` / `RunTaskSweepNow` |
| `server.TaskRunner` | 补跑入口 | `scheduler` |
| `panel.LoadConfig` / `SaveConfig` | 配置读写闭包 | `cmd/server` 的 `Load` / `saveConfig` |
| `pool.StoreSnapshotter` 之外 | 无其他远程依赖 | — |

## 五、长期连接与流

- **SSE 中继**：`Content-Type: text/event-stream`、`Cache-Control: no-cache`、`Connection: keep-alive`、`X-Accel-Buffering: no`，逐帧 `Flush`；被吞空帧且静默 ≥10s 时写 `: keepalive` 注释帧（`internal/upstream/sse.go:620-656`、`internal/server/continue.go:46-48`）。
- **无 WebSocket**：`GET /v1/responses` 明确回 426（本网关不做连接内状态复用）。
- **Redis**：唯一长连接远程依赖，Upstash TLS，读写语义见 [技术栈与中间件](../01-架构总览/技术栈与中间件.md)。
- **models.dev**：按需单次拉全量（4.7MB），5s 超时，进程内建索引后复用。

## 相关文档

- 端点背后的数据结构 → 详见 [类型系统与数据模型.md](类型系统与数据模型.md)
- 面板前端结构 → 详见 [组件架构与通信.md](组件架构与通信.md)
- 端点触发后的完整流程 → 详见 [../03-核心数据业务流/README.md](../03-核心数据业务流/README.md)
