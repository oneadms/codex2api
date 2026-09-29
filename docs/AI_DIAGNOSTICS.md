# AI 日志诊断与修复草稿（第一版）

服务内置 worker，在管理 UI 配置后采集错误、聚合并调用模型，在临时 Git worktree 内生成修复补丁，可选推送分支并创建 GitHub **草稿 PR**。所有修复只指向 **`custom/main`**；`main` 专用于跟随官方更新，程序拒绝将其作为修复目标。

```text
Codex2API：HTTP 5xx / panic / 已接入的上游 400、5xx
  → diagnostics.jsonl（脱敏、有界队列、轮转）
  → 服务端内置 worker（管理 UI 配置、定时或手动扫描）
  → 按错误指纹聚合 → 选择相关源码 → AI 根因分析和精确替换
  → 临时 worktree：路径、规模、Go 语法、gofmt、diff 检查
  → 本地 report.json + repair.patch
  → 开启创建草稿 PR：codex/autofix/* → 草稿 PR → custom/main
  → GitHub Actions 构建、测试 → 人工审核、合并、部署
```

## 1. 推荐：在管理 UI 配置

部署包含本功能的新版 Docker 镜像后，进入管理后台 **AI 诊断**（`/admin/diagnostics`）：

1. 填写本服务 Base URL（以 /v1 结尾）、Codex 模型名称和本服务 API Key。
2. 确认 GitHub 仓库，默认 `oneadms/codex2api`。私有仓库读取或创建 PR 时填写 GitHub Token，授权目标仓库 Contents / Pull requests 读写权限。
3. 开启「日志采集」，按需开启「自动诊断」和「创建草稿 PR」，点击「保存配置」。也可以保存后点「立即扫描」。
4. 在同一页面查看采集状态、任务错误、近期错误、诊断报告、补丁及草稿 PR 链接。

无需额外安装 worker、手动克隆仓库、创建 systemd 服务或编辑诊断环境文件。镜像已包含 Git 和 GitHub CLI。服务器会自动拉取指定仓库的 `custom/main`，按配置运行；保存时立即应用，重启后恢复。修改配置或停用会取消正在运行的任务。一个服务进程同一时间仅运行一个扫描任务，每批最长 15 分钟。

配置保存在现有数据库；密钥保存后不回显，留空保留已有值，勾选删除可清除。沿用项目已有凭据加密机制：配置了 `CODEX_CRED_ENCRYPTION_KEY` 时加密存储，未配置时为明文数据库字段。数据库和现有 `/data` 卷需要持久化：错误日志、托管仓库和修复状态位于 `DATA_DIR/diagnostics`（Docker 默认 `/data/diagnostics`）。

默认关闭采集、自动诊断及 PR 发布。采集开启后直到首次错误才有事件，空日志正常；未达到次数阈值时不会克隆仓库或调用模型。默认每 5 分钟扫描最近 24 小时、同类错误至少 3 次、每批最多 1 个问题，置信度门槛 0.8，均可在 UI 调整。

Codex SDK 使用配置的 API Key 作为 `CODEX_API_KEY`，并将 Base URL 指向本服务 `/v1`，实际请求为 `/v1/responses`。远程地址要求 HTTPS；仅回环地址允许 HTTP。Codex 在隔离的仓库副本中修改代码并运行相关 Go 测试，不访问业务容器文件系统。

所有诊断管理 API 都使用已有管理密钥认证。只生成报告时，公共仓库不需要 GitHub Token；启用创建草稿 PR 后需要 Token。避免使用 GitHub Actions 自带的 `GITHUB_TOKEN`，其创建 PR 的事件通常不会触发新的 CI。细粒度 PAT 到期后可直接在 UI 更新。

`LOG_DISABLED=true` 会阻止诊断采集，UI 会给出错误。可选的 `CODEX_DIAG_REVISION` 用于标记部署 commit；`CODEX_DIAG_ENABLED` 和 `CODEX_DIAG_LOG_PATH` 不再控制主服务，采集由管理 UI 决定。

### 采集内容

- 采集下游 HTTP 5xx、panic，以及调用现有 `logUpstreamError` 的上游 400/5xx；不保证覆盖所有上游协议错误。已有流式状态覆盖 `x-access-log-status` 也会进入采集。
- 不记录请求正文、请求头、账号密钥或完整上游响应；上游只提取 `error.type/code/message`。panic 只保留仓库调用栈，不采集函数参数。
- 使用路由模板，保留请求 ID、模型、状态码和可用的部署 SHA；日志再次清理常见凭据、邮箱及 URL 查询参数。自由文本错误可能仍含应用自定义敏感内容，发送模型前应检查真实样本。
- 单文件 16 MiB，保留一个 `.1` 轮转文件；权限 `0600`。256 条有界队列，队列满时丢弃诊断事件，不等待磁盘或模型。写入故障会记录服务日志，关闭时报告丢弃数。
- 每个服务进程使用自己的日志文件；多副本第一版各自使用独立的 DATA_DIR；共享数据库时配置在启动或本进程保存时加载，不提供跨副本调度选主。不要让多个进程写同一个轮转文件。

## 2. 可选：独立 CLI worker

只有需要将诊断独立部署时才使用本节；通过 UI 启用内置 worker 时可跳过第 2–5 节。CLI worker 运行在有 Git、GitHub CLI 和仓库克隆的开发机或管理服务器，读取 `DATA_DIR/diagnostics/diagnostics.jsonl` 的共享目录或同步副本。

诊断日志权限是 `0600`，worker 需使用与服务相同的 Unix 用户，或读取专门同步给 worker 用户的副本；仅加入相同用户组不足以读取原文件。worker 用户也需要拥有其工作克隆及状态目录的写权限。

```bash
go build -o diagnose ./cmd/diagnose
./diagnose list --logs /path/to/diagnostics.jsonl
```

`list` 只输出最近 24 小时的错误分组，不需要模型或 GitHub 凭据。它同时读取 `.1` 和当前日志；忽略正在写入的末尾半行，并报告损坏行数和超过 1,000 个分组的事件数。它不读取旧的 `server_error.log` / `bad_request.log` 文本格式。

worker **不读取业务 `.env`**。单独为 worker 配置以下环境变量，例如使用 [环境文件示例](../deploy/diagnose.env.example)：

```dotenv
DIAG_LLM_URL=https://your-model-provider.example/v1/chat/completions
DIAG_LLM_MODEL=your-model-name
DIAG_LLM_API_KEY=your-model-api-key
```

接口需支持非流式 Chat Completions 的 `messages`、`max_tokens` 和 JSON 文本输出，完成时返回 `finish_reason=stop`。远程地址要求 HTTPS；仅回环地址允许 HTTP。不跟随重定向。每次模型请求超时 3 分钟、响应最大 256 KiB。可用现有代理地址，但独立模型端点可避免被诊断服务自身故障影响 worker。

## 3. CLI：先生成本地报告

```bash
./diagnose run \
  --repo /path/to/codex2api \
  --logs /path/to/diagnostics.jsonl \
  --state-dir /path/to/diag-state
```

默认固定 `--base custom/main`。worker 会从指定 clone 的 `origin` 获取 `custom/main`，以其最新 commit 创建临时 worktree；不会切换原工作区分支，也不会带入未提交修改。要对比运行版本和修复基线，可查看报告中的 `incident.sample.revision` 与 `base_sha`。第一版不会自动复现历史部署环境。

默认最近 24 小时同一指纹至少出现 3 次才处理；单批最多 1 个问题、最多 2 次模型调用。指纹包含错误来源、路由、方法、状态、模型、归一化消息及 panic 代码位置。上游次数是记录的失败尝试数，不一定等于受影响请求数。

结果在 stdout 输出 JSON，文件在 `state-dir/runs/<fingerprint>-<base-sha>/`：

| 文件 | 用途 |
| --- | --- |
| `report.json` | 错误样本、代码基线、源码选择、根因及精确替换、验证范围 |
| `repair.patch` | 真实 Git unified diff；只有修复通过本地检查才生成 |
| `pr-body.md` | 发布时生成的草稿 PR 描述 |

状态 `prepared` 表示已生成补丁，**不表示编译或测试通过**。`no_fix` 表示证据不足、不宜改代码，或模型置信度低于门槛。模型置信度只是辅助筛选，不能代替验证。鉴权失败、额度耗尽、上游限流/故障会优先作为运行问题处理，不强行生成补丁。

对于大文件，模型根据错误和路径清单选择符号/错误字面量或堆栈行号，worker 提供有界代码片段。信息不足时输出 `no_fix`；不声称能自动修复所有问题。

## 4. CLI：发布草稿 PR 到 custom/main

先为 worker 配置 Git 拉取/推送权限和 `gh` 认证。交互式机器可运行：

```bash
gh auth login
gh auth setup-git
```

无人值守 worker 可使用专用 GitHub App installation token 或细粒度 PAT，仅授权 `oneadms/codex2api` 的 Contents / Pull requests 读写权限，通过 `GH_TOKEN` 提供，并配置对应的 Git credential helper 或 SSH 凭据。不要将 token 放进 remote URL。避免用 GitHub Actions 自带的 `GITHUB_TOKEN` 创建修复 PR，其产生的事件通常不会触发新的 CI workflow。

```bash
./diagnose run \
  --repo /path/to/codex2api \
  --logs /path/to/diagnostics.jsonl \
  --state-dir /path/to/diag-state \
  --publish
```

已准备的补丁可在同一状态目录直接发布，不重复调用模型。程序只推送 `codex/autofix/<指纹前12位>-<基线前12位>`；创建 `--base custom/main --draft` PR，绝不直接推送 `custom/main` 或 `main`，也没有自动合并、部署操作。

同一指纹和基线只处理一次；已存在的同名 PR（包括关闭/合并）会复用，不再创建。基线更新后也会搜索同一指纹的未关闭 PR，避免重复提单。推送成功但 PR 创建失败时，下次会比较远程分支的文件树；一致则继续创建 PR，不一致则停止，不强推覆盖。

现有 `.github/workflows/pr-check.yml` 会执行构建和测试，新增的 `diagnostic-policy` 检查修复 PR 只能指向 `custom/main`，手动改变 PR 目标也会重新检查。仓库管理员可将该检查及已有测试设置为 `custom/main` 分支保护的必需检查；本实现不修改 GitHub 仓库设置。人工确认 CI 和修复逻辑后合并到 `custom/main`，再按现有部署流程上线。

## 5. CLI：持续扫描与运行维护

```bash
./diagnose watch \
  --repo /path/to/codex2api \
  --logs /path/to/diagnostics.jsonl \
  --state-dir /path/to/diag-state \
  --interval 5m \
  --publish
```

可用 [systemd 示例](../deploy/diagnose.service) 托管，修改路径、用户和日志目录后再安装。该示例依赖 worker 主机现有的 Git/gh 认证。环境文件包含密钥，放在仓库外并限制读取权限。

常用参数：`--window 24h`、`--min-count 3`、`--max-per-run 1`、`--cooldown 6h`、`--max-attempts 3`、`--min-confidence 0.8`。单批和单问题尝试上限均不得超过 10。重试只针对失败问题；证据不足和已准备的本地结果不会每轮重新调用模型。基线 commit 改变后会重新评估。

状态写入采用临时文件及原子替换，损坏状态会停止处理。Docker/Linux、macOS 和 BSD 使用系统文件锁 `.worker.lock` 防止同一 state-dir 并发运行；进程异常退出后由内核自动释放，重启无需手工删锁。锁文件保留是正常现象，不要删除。其他平台保留目录锁；旧版本遗留的 `.lock` 目录仍会阻止扫描，需确认旧 worker 停止后清理。不要在 worker 运行时手工删除状态。

日志按固定容量轮转，因此 `--window` 是最大回溯时间，繁忙服务可能只保留更短历史。报告需定期归档，状态超过 4 MiB 会停止新增写入；可在停止 worker 后归档过期条目和对应 runs 目录。同一仓库的 worker 建议共享同一 state-dir，以复用去重记录。Git 拉取使用独立的临时引用，不依赖或覆盖开发者正在使用的 `FETCH_HEAD`。

## 修复范围与验证

- 第一版只修改所选的、已跟踪的普通 Go 源文件；可在相同目录新增 `_test.go`，现有测试只读。
- 不修改工作流、依赖清单、环境文件及诊断模块；拒绝路径穿越、符号链接、模糊或重叠替换。
- 单次最多 6 个文件、12 处替换、64 KiB 替换文本、128 KiB diff、800 行增删。大范围重构留给人工。
- worktree 隔离工作区变化，但不是执行沙箱。worker 不运行模型生成的 shell、Go 程序、测试或仓库 hooks；Go 语法与格式通过进程内解析检查。运行生成代码的构建/测试留在 GitHub 托管 CI，CI 只读仓库权限，不注入生产凭据。
- 第一版提供管理后台配置及报告页面；自动修复 CI 失败、自动合并、灰度部署或回滚尚未提供。修复失败有持久化状态，最多按配置重试，不无限循环。

开发验证：

```bash
go test -race -count=1 ./internal/diag ./cmd/diagnose ./api
go test -race -count=1 ./admin ./database -run 'TestDiagnostic'
go test -race -count=1 ./proxy -run 'TestErrorLogDirUsesEnv|TestUpstreamErrorsFeedDiagnosticCollector'
```

这些测试使用临时 Git 仓库、本地模拟模型和模拟 GitHub CLI；不会操作真实 GitHub 仓库或付费调用模型。
