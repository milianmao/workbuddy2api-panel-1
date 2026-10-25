---
last_updated: 2026-10-25
source_files:
  - internal/pool/pool.go
  - internal/pool/entry.go
  - internal/pool/state.go
  - internal/livecfg/livecfg.go
  - internal/session/session.go
  - internal/panel/panel.go
  - internal/panel/taskcenter.go
  - internal/panel/taskjobs.go
  - internal/panel/ring.go
  - internal/server/handler.go
  - internal/server/budget.go
  - internal/server/wafip.go
kb_version: KB-v1.0
---

# Store 状态管理全景

Go 后端没有 Zustand。对应物是**11 个进程内状态容器**，各有明确的并发模型。改状态前先看这张表——这是本项目最容易引入数据竞争的地方。

## 状态容器全表

| 容器 | 位置 | 并发模型 | 生命周期 | 落盘 |
|---|---|---|---|---|
| `pool.Pool` | `internal/pool/pool.go:15-106` | `sync.RWMutex` + `atomic.Bool dirty`；写入全在写锁内 | 进程级 | `state.json`（5s flush）+ Redis 镜像 |
| `session.Router` | `internal/session/session.go:32-66` | `sync.RWMutex`（快路径 RLock、慢路径写锁 re-check） | 进程级 | Redis `wb2api:bind:*`（fire-and-forget） |
| `livecfg.Holder` | `internal/livecfg/livecfg.go:72-95` | `atomic.Pointer[Snapshot]`，整体替换 | 进程级 | 由 config 文件承载（保存时才写） |
| `server.Handler` 内部状态 | `internal/server/handler.go:159-170` | `degrade`（mutex）、`wafIP`（mutex）、`taskRunning`（mutex）、`budget`（mutex） | 进程级，**重启清零** | 无 |
| `panel.Panel` | `internal/panel/panel.go:88-124` | `loginMu` / `taskMu` / `queueOnce` / `growthConc atomic` | 进程级 | 部分（task-jobs/task-logs） |
| `panel.taskJobManager` | `internal/panel/taskjobs.go:39-131` | 内部 mutex + `chan struct{}` 信号量 + per-uid `active` map | 进程级 | `task-jobs.json`（原子写） |
| `panel.Ring`（日志环） | `internal/panel/ring.go:24-28` | mutex；容量 500/频道 | 进程级 | 任务频道 → `task-logs.json` |
| `usage.Recorder` | `internal/usage/usage.go:104+` | mutex + 30s 防抖 flush | 进程级 | `usage.json` |
| `balance.Recorder` | `internal/balance/balance.go:114+` | mutex + 30s 防抖 flush | 进程级 | `balance_history.json` |
| `credithist` | `internal/credithist/credithist.go:50-60` | mutex，**锁内同步落盘** | 进程级 | `credit-history.json` |
| `reqlog.Recorder` | `internal/reqlog/reqlog.go:24-30` | mutex 保护最近 100 条 + `atomic.Int64` 计数 + 归档队列 | 进程级 | `request-logs/*.jsonl` |

## 1. `pool.Pool` —— 唯一的业务真源

**State 结构**：`byUID map[string]*entry`（单账号状态全在 `entry`），外加 20 多个注入参数（阈值/时长/权重/回调）。字段清单与归属见 [类型系统与数据模型](类型系统与数据模型.md)。

**更新模式**：
- 所有写入都在 `p.mu.Lock()` 内，读用 `RLock`。
- 每次写入调 `p.dirty.Store(true)`；`Flush()` 与后台 flusher 用 `dirty.Swap(false)`，写盘失败置回 true（`internal/pool/persist.go:100-123`）。
- 冷却/熔断/降权/成本**不单独持久化时刻**，而是在 `stateOverviewLocked` 落盘时惰性过滤已过期项（`persist.go:333-410`）——过期项自然消失，无需后台清理。

**Selector 风格**：没有缓存视图，全部现算。`Status()` / `List()` / `CountsDetailed*()` / `RealmHealth()` / `ServableNow()` / `ServableForRealm()` 每次遍历 `byUID`。`countsDetailedForRealm` 委托 `RealmHealth`，避免两套口径（`internal/pool/state.go:582-585`）。

**为什么不做缓存**：账号数有界（几十个），现算成本可忽略；缓存会带来一致性负担，而状态变更路径极多（冷却、熔断、面板操作、定时任务）。

## 2. `session.Router` —— 绑定表

`entries map[string]entry{uid, lastActive}`。快路径 RLock 命中即 `touch` 滚动续期；慢路径写锁 re-check 后双段分配（`internal/session/session.go:147-206`）。GC ticker 删除过期并镜像删 Redis（`:251-267`）。

**与 pool 的关系**：Router 只存 uid 字符串，不存指针——避免与 pool 的锁交叉。可用性判断通过注入的闭包 `Available` / `AvailableForModel` 现查。

## 3. `livecfg.Holder` —— 热配置

`atomic.Pointer[Snapshot]`：`Load()` 拿不可变快照，`Store()` 整体替换（`internal/livecfg/livecfg.go:84-95`）。

**陷阱（本项目已存在）**：`Store` 是整体覆盖，不合并。面板保存路径的 `live.Store(...)`（`cmd/server/main.go:693-700`）**没有回填 `ModelFallback`**，而该字段只来自启动快照（`main.go:313`）——面板保存后 `ModelFallback` 变成 nil。新增热字段时必须同时改两处。

## 4. `server.Handler` 的四个小状态机

| 状态 | 字段 | 写入时机 | 清零 |
|---|---|---|---|
| 提示词降级 | `degrade.until` | 内容拦截首遇 → 次日 00:00 CST（已激活不续期） | 到期自然失效 / 重启 |
| WAF IP 级拦截 | `wafIP{window, uids, activeUntil}` | 60s 窗内 ≥2 个不同 UID 命中 WAF 403 | 60s 后自然失效 / 重启 |
| 任务去重 | `taskRunning map[string]bool` | `/admin/tasks/{name}/run` | 任务结束 / 重启 |
| 日预算 | `budget{used, day, burst, cacheRounds}` | 每请求 `add(observed)`；`admit()` 惰性日切 | CST 日切 |

这四个都**不落盘**——重启清零可接受（降级/预算重启极罕见，WAF 拦截是进程内观测）。

## 5. `panel.Panel` 的状态

| 字段 | 用途 | 并发 |
|---|---|---|
| `logins map[string]loginSession` | OAuth 设备流会话（state → 会话），15min TTL | `loginMu` |
| `taskLocks map[string]*sync.Mutex` | per-account 任务互斥（`TryLock` 语义，条目常驻） | `taskMu` 只保护 map 本身 |
| `q *queueState` | 一次性执行队列（运行标志 + items + seq） | `queueOnce` 初始化，内部锁保护 |
| `taskJobs *taskJobManager` | 持久化后台作业 | 见下 |
| `growthConc atomic.Int64` | 排程并发（当前未消费，见 [已知问题](../06-迭代与变更/已知问题与陷阱.md)） | atomic |
| `notifier func(string)` | 排程汇总通知出口（alerting webhook） | 装配期设定、运行期只读 |

**per-account 锁的语义**：`TryLock` 而非 `Lock`——同一账号已有任务在跑时直接 409/让位，**不排队**。理由是动作虽幂等但每遍含多次真实对话，排队只会白烧配额（`internal/panel/panel.go:96-98` 注释）。

## 6. 日志环 `panel.Ring`

三频道（chat/task/sys，`internal/panel/ring.go:24-28`），每频道容量 500；任务频道额外原子落盘 `task-logs.json`（`:99-196`）。`io.MultiWriter(os.Stderr, pn.Logs())` 让标准 `log` 与控制台输出行为完全不变（`cmd/server/main.go:389-390`）。

## Selector / 更新模式约定

| 约定 | 说明 |
|---|---|
| 读用现算 | 不缓存派生值；`/status`、面板 overview 都直接遍历 |
| 写必须置 dirty | 否则不落盘；`Flush` 用 `Swap(false)` |
| 锁内不做 IO | Redis 写是 fire-and-forget；文件写只在 `Flush` / 显式 `saveLocked` |
| 跨容器不通锁 | pool 与 session 互不持对方锁；只用注入闭包单向查询 |
| 进程内状态可丢 | 降级/预算/WAF/任务去重/探索 timer 都不落盘，重启归零是有意设计 |
| 时间用 CST 判日 | 额度、签到、夜猫子窗口按北京零点；见 `internal/cst` |

## 相关文档

- 状态字段定义 → 详见 [类型系统与数据模型](类型系统与数据模型.md)
- 状态如何随请求变化 → 详见 [../03-核心数据业务流/账号池选号与冷却.md](../03-核心数据业务流/账号池选号与冷却.md)
- 前端如何消费这些状态 → 详见 [组件架构与通信](组件架构与通信.md)
