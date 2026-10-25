---
last_updated: 2026-10-25
source_files:
  - AGENTS.md
  - .github/workflows/binaries.yml
  - .github/workflows/docker-ghcr.yml
  - .github/workflows/autobot.yml
  - AutoBot.yml
  - go.mod
kb_version: KB-v1.0
---

# Git工作流与CI

## 分支模型

| 分支 | 用途 |
|---|---|
| `main` | 主干；push 即触发 binaries 构建 + GHCR 镜像 |
| `autobot/*` | AutoBot 自动实现分支（`AutoBot.yml:92-101` 策略：`base: main`、`never: push_main/merge/tag/release`） |
| `feat/*`、`fix/*`、`ci/*` | 人工特性/修复分支（远端可见 `feat/anthropic-messages-responses`、`fix/autobot-window`、`ci/autobot`、`feat/fetch-endpoint` 等） |
| `backup/*` | 大改前的备份分支（如 `backup/pre-rebase-2026-10-22`） |

**没有 develop/release 分支**，也没有 PR 必须审查的分支保护证据。合并后通常直接推 `main`。

## 提交信息规范（实测 100+ 条）

格式：`<type>(<scope>): <中文摘要>`

| type | 出现频次（近 60 条） | 例 |
|---|---|---|
| `fix` | 19 | `fix(upstream): 整句规则移入 identity 作用域` |
| `feat` | 17 | `feat(panel): 导入弹窗支持一次多选多个 JSON 文件` |
| `docs` | 4 | `docs(readme): 徽章更新到当前状态` |
| `chore` | 3 | `chore(autobot): ...` |
| `test` | 2 | `test(panel): 端到端钉死「OAuth 登录的号落池即暂停选号」` |
| `refactor` | 1 | `refactor(upstream): ...` |
| `build` | 1 | `build: ...` |

**scope 取值**：`panel` / `upstream` / `server` / `pool` / `images` / `tasks` / `scheduler` / `login` / `catalog` / `readme` —— 基本对应 `internal/` 下的包名。

**正文惯例**：问题实测证据（带日期/数字/实测结果）+ 改动分条 + 测试说明（如"go test ./... 21 包全绿"）。摘要一律中文。

**合并提交两种并存**：
- 手工：`merge PR #71: feat(panel): 封禁账号只读探活 + 余额趋势可用账号数`
- GitHub 原生：`Merge pull request #59 from huniangitb/feat/fetch-endpoint`

## 发版（硬规则，`AGENTS.md:3-23`）

```bash
VERSION=$(TZ=Asia/Shanghai date +%Y.%-m.%-d.%-H%M)
[ "$(TZ=UTC date +%H)" = "$(TZ=Asia/Shanghai date +%H)" ] && VERSION=$(date -u -d '+8 hours' +%Y.%-m.%-d.%-H%M)
git tag "v$VERSION"
git push origin "v$VERSION"
```

| 规则 | 说明 |
|---|---|
| 格式 | `v2026.9.26.711` = 上海日期 + 时不补零 + 分两位 + 不带秒 |
| 不跟上游 | 禁止 `v1.x` 标签（历史遗留的 v1.2.0–v1.11.6 是旧格式，勿再模仿） |
| 同一号 | 标签、Release、二进制用同一个号；二进制里不带 `v`（面板自己加） |
| 不手改 `appVersion` | `cmd/server/main.go:42` 恒为 `dev`，只有 CI 用 `-X main.appVersion=$VERSION` 注入 |
| tzdata 陷阱 | 本机（Windows/git-bash）无 tzdata 时 `TZ=Asia/Shanghai date` **静默回落 UTC**，标签早 8 小时；用上面的显式 +8h 兜底 |

现状：92 个标签，最早 2026-09-12，最新 `v2026.10.21.1006`；488 次提交，42 个活跃日（最近 2026-10-24）。

## CI 流水线

### `binaries.yml` —— 构建 + Release

| 触发 | `push` main / `tags: v*` / `workflow_dispatch`；忽略 autobot 三文件变更 |
|---|---|
| 步骤 | `setup-go`（`go-version-file: go.mod`）→ **`go test ./...`（唯一测试）** → 版本计算 → 6 平台交叉编译 → 6 个 artifact → tag 时 `gh release create` |
| 平台 | windows/linux/darwin × amd64/arm64（手写双层 for，非 matrix——注释："matrix 等构建时间不够再用"） |
| 版本选择 | tag 推送用标签名（避免 CI 时钟与标签差一分钟变成两个号），否则现算 |
| 编译参数 | `CGO_ENABLED=0 GOOS=… GOARCH=… -trimpath -ldflags="-s -w -X main.appVersion=${VERSION}"` |
| 产物 zip | 二进制 + `config.example.json` + `README.md` |

出处 `.github/workflows/binaries.yml:24-97`。

### `docker-ghcr.yml` —— 多架构镜像

| 触发 | `push` main / `tags: v*` / `workflow_dispatch`；PR 仅在触碰构建相关路径时构建（不推送） |
|---|---|
| 步骤 | QEMU + Buildx → 登录 GHCR（PR 跳过）→ meta 生成 tag → `build-push-action` |
| 平台 | `linux/amd64,linux/arm64` |
| Tag 规则 | main → `latest` + `main` + `sha-xxx`；`v1.2.3` 风格 → `1.2.3` / `1.2` / `1`（注：当前时间戳版本不是 semver，故只产生 `latest`/`main`/`sha`） |
| 优化 | 并发组按 ref 串行（发版时 main+tag 同推不双跑）、`cache-from/to: gha`、`provenance: false`（避免 ghcr 出现 unknown 架构条目） |

### `autobot.yml` —— 自动实现 bot

cron `30 1 * * *`（上海 09:30）+ 手动。流程 `plan → analyze（分片，`max-parallel: 1`）→ implement`，只推 `autobot/*` 分支。用自有 `AUTOBOT_TOKEN` 而非 `GITHUB_TOKEN`（需要触发下游 workflow）。落地闸在 `scripts/autobot.py:301-328`：`go build ./...` + `go vet ./...` + `go test ./...`。

## 本地开发闭环

```bash
go run ./cmd/server -config config.json     # 首次运行自动生成含随机 api_key 的配置
go test ./...                               # 与 CI 同款
go test -short ./...                        # 跳过需外部依赖的用例
go vet ./internal/...                       # 实测通过
```

两个已知的环境噪音：
1. `gofmt -l .` 在 Windows 检出下列出全部 303 个文件——`core.autocrlf=true` 且无 `.gitattributes`，CRLF 被 gofmt 判为差异；转 LF 后无差异。**不要**因此批量改写行尾。
2. `go build` 首次会按 `go.mod` 的 `go 1.26.10` 下载工具链（本机已就位则无需）。

## 无 lint 步骤（事实）

全仓无 `.golangci*` 配置，三个 workflow 都不跑 `gofmt`/`vet`（`go vet` 只出现在 AutoBot 的落地闸里）。因此 **格式与 staticcheck 类问题不会在 CI 被拦**——提交前自己跑。

## 相关文档

- 发版与部署细节 → 详见 [../01-架构总览/部署与运行架构.md](../01-架构总览/部署与运行架构.md)
- 测试怎么写 → 详见 [测试策略](测试策略.md)
