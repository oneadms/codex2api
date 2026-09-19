# 本地调试环境（GoLand / IDEA Debug + Docker 依赖）

本机调试形态：**Docker 只跑依赖（PostgreSQL + Redis），Go 后端在 GoLand/IDEA 里以 Debug 方式跑，
前端用 Vite dev server 跑**。改代码只需在 IDE 里 Rerun，不需要重建镜像。

---

## 1. 两条命令开始

```bash
make dev-up          # 起 Docker 依赖（Postgres + Redis），已健康检查通过才算完
```

然后在 GoLand / IDEA 顶部运行下拉框选择 **`00 一键 Debug（后端 + 前端）`**，点 🐞 调试按钮。

> 只调后端就选 `01 后端 Debug (Postgres+Redis)`；完全不想用 Docker 就用 `02 后端 Debug (SQLite+Memory)`。

---

## 2. 组件与端口

| 组件 | 启动方式 | 地址 / 凭据 |
| --- | --- | --- |
| PostgreSQL 18 | `make dev-up`（Docker） | `127.0.0.1:5432`，库/用户/密码均为 `codex2api` |
| Redis 7 | `make dev-up`（Docker） | `127.0.0.1:6379`，DB 0，无密码 |
| Go 后端 | GoLand Debug（配置 01） | `http://127.0.0.1:8080` |
| 前端 Vite | GoLand Run（配置 03） | `http://127.0.0.1:5173/admin/` |
| 管理台密钥 | Run Configuration 注入 | `codex2api-dev`（`X-Admin-Key` 请求头） |

**看哪里：**

- 日常联调 / 改前端 → <http://127.0.0.1:5173/admin/>（Vite 热更新，`/api`、`/health`、`/v1` 自动反代到 8080）
- 想验证「嵌进二进制的前端产物」 → <http://127.0.0.1:8080/admin/>
- 其余入口：`/key-usage`、`/image-studio`、`/account-portal`（都挂在 8080 上）

---

## 3. 为什么不用改 `.env`（关键机制）

仓库里的 `.env` 是**给容器用的**：`DATABASE_PATH=/data/codex2api.db`、`REDIS_ADDR=redis:6379`、
`IMAGE_ASSET_DIR=/data/images` —— 这些路径/主机名在 macOS 宿主机上不存在，直接 `go run .` 会失败。

`config.Load()` 用的是 `godotenv.Load()`，它的语义是：**已存在的进程环境变量不会被 `.env` 覆盖**。
所以 Run Configuration 里 `envs` 注入的值优先级高于 `.env`，无需动 `.env` 就能本机调试：

| 变量 | `.env`（容器） | 本机 Debug 覆盖值 |
| --- | --- | --- |
| `DATABASE_DRIVER` | `sqlite` | `postgres` |
| `DATABASE_HOST/PORT` | — | `127.0.0.1:5432` |
| `DATABASE_PATH` | `/data/codex2api.db` | SQLite 模式下改为 `data/codex2api.db` |
| `CACHE_DRIVER` | `memory` | `redis` |
| `REDIS_ADDR` | — | `127.0.0.1:6379` |
| `IMAGE_ASSET_DIR` | `/data/images` | `data/images` |
| `LOG_DIR` | `logs` | `logs`（不变） |
| `CODEX_BIND` | 未设（0.0.0.0） | `127.0.0.1`（只本机可访问） |
| `ADMIN_SECRET` | 注释掉 | `codex2api-dev`（固定密钥，省掉首次初始化） |
| `CODEX_GIN_DEBUG` | 无 | `1`（启动打印全部注册路由，排查 404 用） |

> `DATABASE_PATH`、`IMAGE_ASSET_DIR`、`LOG_DIR` 用**相对路径**是安全的：Run Configuration 的
> Working directory 固定为 `$PROJECT_DIR$`，相对路径即项目根目录。

---

## 4. IDE 里现成的 4 个运行配置

文件位于 `.idea/runConfigurations/`（该目录被 `.gitignore` 忽略，只影响本机）。

| 配置名 | 类型 | 用途 |
| --- | --- | --- |
| `00 一键 Debug（后端 + 前端）` | Compound | 一次点 🐞 同时起后端 Debug + 前端 Vite |
| `01 后端 Debug (Postgres+Redis)` | Go Application | **默认推荐**，与线上一致 |
| `02 后端 Debug (SQLite+Memory)` | Go Application | 零依赖，不启 Docker 也能调；库文件 `data/codex2api.db` |
| `03 前端 Vite Dev Server (5173)` | npm run `dev` | 前端独立启动 |

**如果运行下拉框里没看到它们**：`File → Reload All from Disk`，或 `File → Invalidate Caches / Restart`；
再不行把 IDE 关掉重开（运行配置在项目加载时读取）。

**调试注意点：**

- Go 端用 `//go:embed frontend/dist/*` 打包前端，所以 **`frontend/dist` 必须存在**才能编译；缺失时执行 `make dist`。
- 首次 Debug 要编译整个 main 包（含 embed），会比日常慢一点；之后走增量构建。
- 断点常用位置：
  | 想查什么 | 断点位置 |
  | --- | --- |
  | 启动/配置/迁移 | `main.go` 的 `main()`、`config/config.go` 的 `Load()`、`database/postgres.go` 的 `New()` |
  | 管理台 401 / 503 | `admin/handler.go` 的 `adminAuthMiddleware()`（约 1315 行） |
  | Chat / Responses 主链路 | `proxy/handler.go` 的 `ChatCompletions()`（约 6617 行）、`Responses()`（约 3679 行） |
  | 上游请求、重试、超时 | `proxy/executor.go`、`proxy/continuous_retry.go`、`proxy/first_token_timeout.go` |
  | 会话粘性 / turn state | `proxy/continuity.go`、`proxy/codex_turn_state.go` |
  | 限流 / 并发 | `proxy/ratelimit.go`、`proxy/apikey_concurrency.go` |
  | 账号加载与调度 | `auth/store.go`（约 5102 行「从数据库加载了 N 个账号」） |
  | 缓存 / Redis | `cache/redis.go` 的 `NewRedisWithOptions()`（约 59 行） |
  | 运行时设置 | `proxy/runtime_config.go` 的 `ApplyRuntimeSettingsFromSystem()`（约 332 行） |

---

## 5. Makefile 命令速查

| 命令 | 作用 |
| --- | --- |
| `make dev-up` | 起 Postgres + Redis 并等待 healthy |
| `make dev-down` | 停依赖（保留数据） |
| `make dev-reset` | ⚠️ 删卷重建依赖 + 清 SQLite 文件（**数据全丢**） |
| `make dev-ps` / `make dev-logs` | 看容器状态 / 跟日志 |
| `make db-shell` / `make redis-shell` | 进 `psql` / `redis-cli` |
| `make backend` | 命令行起后端（等价于配置 01，排查 IDE 之外的差异时用） |
| `make frontend` / `make frontend-install` | 起 Vite / `npm ci` |
| `make dist` | 构建 `frontend/dist`（embed 必需） |
| `make test` / `make typecheck` | Go 全量测试 / 前端类型检查 + 单测 |
| `make doctor` | 体检：工具链版本、dist 是否存在、端口占用 |

---

## 6. 前端调试

- 起法：配置 `03 前端 Vite Dev Server (5173)`，或 `make frontend`；访问 <http://127.0.0.1:5173/admin/>。
  （注意 base 固定为 `/admin/`，根路径 `/` 是 404，这是设计如此。）
- 前端代码改动走 Vite HMR，**不需要 `npm run build`**；只有想让 `8080/admin` 也更新时才需要 `make dist`。
- 本次对 `frontend/vite.config.js` 的调整：
  - 代理目标从 `http://localhost:8080` 改成 `http://127.0.0.1:8080`：Node 17+ 不再重排 DNS 结果，
    `localhost` 可能先解析成 `::1`，而后端只监听 IPv4，会出现随机的代理 `ECONNREFUSED`；
  - 新增 `/v1` 代理（与文档一致，便于直接在 dev server 上敲 API）；
  - 固定 `port: 5173`。
- 页面内断点：浏览器 F12 → Sources → `webpack://`/`src/` 下的 `.tsx`（Vite 自动带 sourcemap）。
- 接口联调：`curl -H 'X-Admin-Key: codex2api-dev' http://127.0.0.1:5173/api/admin/stats`

---

## 7. 数据落在哪 / 怎么重置

| 数据 | 位置 | 清理方式 |
| --- | --- | --- |
| Postgres 数据 | Docker 卷 `codex2api-dev_pgdata` | `make dev-reset` |
| Redis 数据 | Docker 卷 `codex2api-dev_redisdata` | `make dev-reset` |
| SQLite 库（模式 02） | `./data/codex2api.db`（已 gitignore） | `make clean` 或删 `data/` |
| 图片资产 | `./data/images` | 同上 |
| 运行日志 | `./logs/`、`./logs/security/` | `make clean` |

导入测试数据最快的方式：管理台 → 账号 → 导入，或直接往库里灌 `docker exec -i codex2api-dev-postgres psql -U codex2api -d codex2api < dump.sql`。

---

## 8. 常见问题

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| 启动报 `必须通过 .env 或环境变量配置 PostgreSQL (DATABASE_HOST)` | Run Configuration 的环境变量没生效（改成手建配置了） | 用现成的 01/02 配置，或补上 `DATABASE_*` 变量 |
| 报 `必须…配置 SQLite 数据库路径` | 选了 02 但 `DATABASE_PATH` 丢了 | 补 `DATABASE_PATH=data/codex2api.db` 并 `mkdir -p data` |
| 报 `数据库连接测试失败: connection refused` | 依赖没起 / 端口没通 | `make dev-up`，再 `make dev-ps` 看 healthy |
| `bind: address already in use` (8080) | 有旧进程或容器占着 8080 | `lsof -i :8080`；`docker stop codex2api codex2api-sqlite` 等旧容器 |
| 5173 提示端口被占用 | 已有 vite 在跑 | 关掉旧的，或改用 `--port`；配了 `strictPort: false` 会自动换端口 |
| 配置 03 提示 Node 解释器未配置 | IDE 里没绑定 Node | `Settings → Languages & Frameworks → Node.js` 选本机 Node（v22.12+，当前 v24），或把配置里的 interpreter 改成具体路径 |
| 侧边栏/nav 点击 404、接口 502 | 后端没起或崩了 | 看 Debug 控制台；`curl http://127.0.0.1:8080/health` |
| 前端接口报 503 `bootstrap_required` | 没带 `ADMIN_SECRET` 或页面还没初始化 | 确认配置里注入了 `ADMIN_SECRET=codex2api-dev`，页面登录用同一串 |
| `/v1/*` 全返回 503 `not configured` | 库里还没有任何 API Key | 管理台 → API 密钥 → 新建一把；这属于正常业务状态 |
| 改了前端但 8080/admin 没变化 | 8080 提供的是 go:embed 的构建产物 | `make dist` 后重跑；调试请走 5173 |
| 编译报 `pattern frontend/dist/*: no matching files` | `frontend/dist` 被清了 | `make dist` |
| Debug 时打的断点显示灰色「不可达」 | 编译产物与源码不同步 / 被内联优化 | 重新 Build；必要时 `go clean -cache` |
| 想看清路由注册，找不到入口 | 默认 `ReleaseMode` | 配置里已有 `CODEX_GIN_DEBUG=1`，启动会打印全部路由表 |

---

## 9. 本次新增/修改的文件（回滚方式）

| 文件 | 说明 |
| --- | --- |
| `docker-compose.dev.yml` | 新增。只起 Postgres + Redis，端口仅绑 `127.0.0.1`，容器名 `codex2api-dev-*`，与线上 compose 的容器/卷互不干扰 |
| `Makefile` | 新增。把上面所有命令固化成目标，并把本机环境变量集中在一处 |
| `.idea/runConfigurations/*.xml` | 新增 4 个运行配置（`.idea/` 本就被 gitignore） |
| `frontend/vite.config.js` | 修改：`/v1` 代理、代理目标改 `127.0.0.1`、固定端口 5173 |
| `main.go` | 修改：新增 `CODEX_GIN_DEBUG` 开关（默认仍是 `ReleaseMode`，线上行为不变） |
| `docs/LOCAL_DEBUG.md` | 新增，即本文档 |

回滚代码改动：

```bash
git checkout main.go frontend/vite.config.js
rm -rf .idea/runConfigurations docker-compose.dev.yml Makefile docs/LOCAL_DEBUG.md
make dev-down && docker volume rm codex2api-dev_pgdata codex2api-dev_redisdata
```

---

## 10. 与容器部署的边界

- **不要**用 `docker compose -f docker-compose.local.yml up` 来做本地 Debug：它会额外起一个 app 容器占住 8080，
  IDE 里的后端就起不来了。要用容器跑整套时，先 `make dev-down` 或停掉 app 容器。
- 本机 `127.0.0.1:8080` 与容器内 `8080` 是两套，`.env` 只对容器生效，IDE 侧以 Run Configuration 为准。
