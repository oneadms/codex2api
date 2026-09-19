# ============================================================
# Codex2API 本地开发辅助（macOS / Linux）
#
# 日常调试只需要两步：
#   make dev-up        # 1) 起 Docker 依赖（Postgres + Redis）
#   然后在 IDEA 里点 ▶ Debug（run configuration 已配置好）
#
# 其余目标用于：命令行起后端、起前端、看日志、清库重来。
# ============================================================

SHELL := /bin/bash
COMPOSE_DEV := docker compose -f docker-compose.dev.yml

# ------------------------------------------------------------
# 本地调试环境覆盖值
# 这些变量会**覆盖** .env（godotenv 不覆盖已存在的进程环境变量），
# 用来把容器内路径(/data、redis:6379)换成宿主机路径/地址。
# ------------------------------------------------------------
DATABASE_DRIVER := postgres
DATABASE_HOST   := 127.0.0.1
DATABASE_PORT   := 5432
DATABASE_USER   := codex2api
DATABASE_PASSWORD := codex2api
DATABASE_NAME   := codex2api
CACHE_DRIVER    := redis
REDIS_ADDR      := 127.0.0.1:6379
CODEX_BIND      := 127.0.0.1
CODEX_PORT      := 8080
IMAGE_ASSET_DIR := data/images
LOG_DIR         := logs
ADMIN_SECRET    ?= codex2api-dev
CODEX_GIN_DEBUG ?= 1

export DATABASE_DRIVER DATABASE_HOST DATABASE_PORT DATABASE_USER DATABASE_PASSWORD \
       DATABASE_NAME CACHE_DRIVER REDIS_ADDR CODEX_BIND CODEX_PORT \
       IMAGE_ASSET_DIR LOG_DIR ADMIN_SECRET CODEX_GIN_DEBUG

.PHONY: help dev-up dev-down dev-restart dev-reset dev-ps dev-logs db-shell redis-shell \
        backend frontend frontend-install dist build build-backend test typecheck doctor clean

help: ## 显示可用命令
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# ---------------------------- Docker 依赖 ----------------------------
dev-up: ## 启动 Postgres + Redis（等待健康检查通过）
	@mkdir -p data logs
	$(COMPOSE_DEV) up -d --wait
	@echo "依赖就绪: postgres=127.0.0.1:$(DATABASE_PORT) redis=127.0.0.1:6379"

dev-down: ## 停止依赖（保留数据）
	$(COMPOSE_DEV) down

dev-restart: ## 重启依赖
	$(COMPOSE_DEV) restart

dev-reset: ## ⚠️ 删除开发库数据卷后重建（数据全部清空）
	$(COMPOSE_DEV) down -v
	rm -rf data/codex2api.db data/codex2api.db-*
	$(COMPOSE_DEV) up -d --wait
	@echo "开发库已重置"

dev-ps: ## 查看依赖容器状态
	$(COMPOSE_DEV) ps

dev-logs: ## 跟踪依赖日志
	$(COMPOSE_DEV) logs -f --tail=100

db-shell: ## 进入 Postgres 命令行
	docker exec -it codex2api-dev-postgres psql -U $(DATABASE_USER) -d $(DATABASE_NAME)

redis-shell: ## 进入 Redis 命令行
	docker exec -it codex2api-dev-redis redis-cli

# ---------------------------- 后端 ----------------------------
backend: dist ## 命令行起后端（等价于 IDEA 里的 Debug 配置）
	go run .

build-backend: dist ## 编译后端二进制到 ./codex2api_local
	go build -o codex2api_local .

# ---------------------------- 前端 ----------------------------
frontend-install: ## 安装前端依赖
	cd frontend && npm ci

frontend: ## 启动 Vite 开发服务器（http://127.0.0.1:5173/admin/）
	cd frontend && npm run dev

dist: frontend/dist/index.html ## 确保前端产物存在（go:embed 必需）

frontend/dist/index.html:
	@echo "frontend/dist 缺失，先构建前端（go:embed 需要）..."
	cd frontend && npm ci --no-audit --no-fund && npm run build

build: dist ## 完整构建（前端产物 + 后端二进制）
	go build -o codex2api_local .

# ---------------------------- 校验 ----------------------------
test: ## 运行 Go 测试
	go test ./... -count=1

typecheck: ## 前端类型检查 + 单测
	cd frontend && npm run typecheck && npm test

doctor: ## 检查本地环境是否满足调试条件
	@echo "== 工具链 =="
	@go version; node -v; npm -v; docker --version; docker compose version
	@echo "== frontend/dist（go:embed 必需） =="
	@test -f frontend/dist/index.html && echo "OK" || echo "缺失 → 执行 make dist"
	@echo "== 端口监听（8080/5173 应为空闲；5432/6379 由开发依赖占用属正常） =="
	@for p in 8080 5173 5432 6379; do \
		if nc -z 127.0.0.1 $$p 2>/dev/null; then echo "$$p: 已被监听"; else echo "$$p: 空闲"; fi; \
	done

clean: ## 清理本地运行产物（不动 Docker 数据）
	rm -rf codex2api_local data/codex2api.db data/codex2api.db-* logs/*.log
