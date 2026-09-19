# 合并冲突解决交接文档

## 任务背景

**目标**：把 upstream 官方仓库 (https://github.com/james-6-23/codex2api) 的 `main` 分支合并到本地二开分支 `custom/main`。

**仓库布局**：
- `origin` = https://github.com/oneadms/codex2api.git （用户自己的 fork，二开分支所在）
- `upstream` = https://github.com/james-6-23/codex2api.git （官方仓库）

**分支状态**（合并前）：
- `custom/main` 领先 upstream/main 52 个提交（用户的二开功能，主要是 TraeCN 系列功能）
- `upstream/main` 领先 custom/main 14 个提交（官方新功能，主要是 CodexTurnState、Glass 主题、Prism 模式等）

**合并命令**（已执行）：
```bash
git fetch upstream
git merge upstream/main --no-ff -m "Merge remote-tracking branch 'upstream/main' into custom/main"
```

## 当前进度

**合并已启动，处于冲突解决阶段**。共 9 个冲突文件：

| 文件 | 状态 | 备注 |
|------|------|------|
| `admin/handler.go` | ✅ 已用 Python 脚本解决，**但未 git add** | 保留了 HEAD 全部字段 + upstream 的 3 个 CodexTurnState 字段 |
| `frontend/src/locales/en.json` | ⏳ 待解决 | i18n 翻译文件，需保留双方新增 key |
| `frontend/src/locales/zh.json` | ⏳ 待解决 | 同上 |
| `frontend/src/pages/Proxies.tsx` | ⏳ 待解决 | 代理页面 |
| `proxy/executor.go` | ⏳ 待解决 | 代理执行器 |
| `proxy/resin.go` | ⏳ 待解决 | Resin 出口相关 |
| `proxy/upstream_trace.go` | ⏳ 待解决 | 上游追踪 |
| `proxy/wsrelay/executor.go` | ⏳ 待解决 | WebSocket 中继执行器 |
| `proxy/wsrelay/manager.go` | ⏳ 待解决 | WebSocket 中继管理器 |

## 冲突解决通用策略

**核心原则：双方的改动都要保留**，因为：
- HEAD (custom/main) 包含用户的 TraeCN 系列二开功能（TraeCN 账号、签到、额度池等）
- upstream/main 包含官方新功能（CodexTurnState 注入、Glass 主题、Codex egress 统一等）

**典型模式**：
1. **struct 字段冲突** → 字段集合取并集
2. **import 块冲突** → import 取并集，注意去重
3. **函数体冲突** → 仔细阅读双方意图，通常需要把双方新增的逻辑都保留（如果双方改的是不同分支条件，按 if/else 并存）
4. **JSON locale 文件冲突** → key 取并集，相同 key 的二开翻译优先（保留 HEAD 的中文翻译风格）
5. **HTTP 路由注册冲突** → 路由取并集

## 已解决文件详情

### admin/handler.go（已解决但未 add）

**冲突位置**：原 1688-1948 行，`accountResponse` struct 字段定义

**解决方案**：用 Python 脚本（见下方"关键脚本"）以 HEAD 为基础，在 `CodexPassthroughMode` 字段后插入了 upstream 的 3 个新字段：
- `CodexTurnState string` (`json:"codex_turn_state,omitempty"`)
- `CodexTurnStateModels string` (`json:"codex_turn_state_models,omitempty"`)
- `CodexTurnStateSetAt string` (`json:"codex_turn_state_set_at,omitempty"`)

**字段差异对比**：
- HEAD 独有 14 个字段：全部是 `TraeCN*` 前缀（TraeCNAPI、TraeCNHost、TraeCNDeviceID 等）
- upstream 独有 3 个字段：CodexTurnState、CodexTurnStateModels、CodexTurnStateSetAt
- 共同字段 120 个

**注意**：解决后 `CodexPassthroughMode` 字段位置变了（从 struct 末尾移到中间），Go 语言字段顺序不影响功能，无需担心。

**下一步**：`git add admin/handler.go`

## 关键脚本

### 用于解决 handler.go 的 Python 脚本（可复用模式）

```python
import sys

with open('admin/handler.go', 'r', encoding='utf-8') as f:
    lines = f.readlines()

# 找到冲突块（0-indexed）
start = mid = end = None
for i, line in enumerate(lines):
    if line.startswith('<<<<<<< HEAD'):
        start = i
    elif line.startswith('=======') and start is not None and mid is None:
        mid = i
    elif line.startswith('>>>>>>> upstream/main') and start is not None and mid is not None:
        end = i
        break

head_lines = lines[start+1:mid]
upstream_lines = lines[mid+1:end]

# 以 HEAD 为基础，在指定字段后插入 upstream 独有行
insert_idx = None
for i, l in enumerate(head_lines):
    if 'CodexPassthroughMode' in l:
        insert_idx = i + 1
        break

turn_state_lines = [l for l in upstream_lines if 'CodexTurnState' in l]
merged = head_lines[:insert_idx] + turn_state_lines + head_lines[insert_idx:]

new_lines = lines[:start] + merged + lines[end+1:]
with open('admin/handler.go', 'w', encoding='utf-8') as f:
    f.writelines(new_lines)
```

### 查找冲突位置的命令

```bash
grep -n "<<<<<<< \|======= \|>>>>>>> " <文件路径>
```

### 对比冲突双方字段差异的脚本

```python
# 适用于 struct 字段冲突：列出双方各自独有的字段名
head_fields = set(...)
upstream_fields = set(...)
print("HEAD 独有:", head_fields - upstream_fields)
print("upstream 独有:", upstream_fields - head_fields)
```

## 下一步操作清单

1. **立即执行**：`git add admin/handler.go`（该文件已解决但未标记）
2. **逐个解决剩余 8 个冲突文件**，按上述策略：
   - 先 `grep -n "<<<<<<< " <文件>` 看冲突块数量
   - 小冲突直接用 StrReplace 工具
   - 大冲突用 Python 脚本精确处理
3. **每解决一个文件**：
   - 用 `grep -c "<<<<<<< " <文件>` 验证冲突标记已清空（应输出 0）
   - `git add <文件>`
4. **全部解决后**：
   - `gofmt -l .` 检查 Go 文件格式（应该无输出）
   - `go build ./...` 验证编译
   - `cd frontend && npm run build` 验证前端构建（可选，谨慎，可能耗时）
5. **提交合并**：`git commit`（保留默认合并提交信息）

## 项目特性提示

- 这是 codex2api 项目（不是 CLAUDE.md/AGENTS.md 里说的 pgx 或 smithy-go，那些规则文件不适用）
- 后端 Go + 前端 React/TypeScript
- 用户二开的核心功能围绕 "TraeCN" 关键词（Trae 中国版账号集成）
- upstream 最近的核心功能是 "CodexTurnState 注入" 和 "Glass 主题"
- 项目根目录有 `Makefile`、`docker-compose.dev.yml`、`docs/LOCAL_DEBUG.md` 等本地开发文件（未跟踪）

## 取消合并的方法（如需要）

```bash
git merge --abort
```

## 参考命令

```bash
# 查看 upstream 领先的 14 个提交
git log --oneline HEAD..upstream/main

# 查看 custom/main 领先的 52 个提交
git log --oneline upstream/main..HEAD

# 查看当前冲突文件列表
git diff --name-only --diff-filter=U
```
