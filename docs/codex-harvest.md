# Mihomo 与采票工作台

管理台侧栏的「采票工作台」统一管理出口、自动采票、手动任务和记录。
功能参考 [sub2api v2.8.0](https://github.com/ranxi2001/sub2api/releases/tag/v2.8.0)，
模块来源和许可见 [NOTICE](../third_party/sub2api/NOTICE.md)。

## Docker 使用

1. 启动新的镜像，保留现有 `/data` 持久卷。镜像默认 `DATA_DIR=/data`。
2. 进入「采票工作台 → Mihomo」，点击「安装内核」。内核固定为 v1.19.31，
   从官方发行版下载并验证 SHA-256，支持 Linux amd64 / arm64。
3. 填入 Clash / Mihomo YAML 订阅地址，更新订阅与节点。可追加订阅、按出口地区筛选、
   检测节点、停用或恢复节点，也可开启「每个节点只用一次」。
4. 内核运行后点击「设为采票代理」。在「概览与控制」配置门控模型并启用采票。
   例如 `gpt-6-astra`；默认目标长度按账号套餐推导，个人 292、团队 332。
5. 选择速度预设或修改各项参数，然后保存。所有采票设置统一在工作台管理。

托管内核只监听回环地址，代理端口 `3101`、控制端口 `9098`，无需映射给公网。
内核、订阅与节点状态保存在 `$DATA_DIR/mihomo-managed`。Windows 服务可以使用外部代理，
内核安装和运行需在 Linux / Docker 中完成。

订阅下载失败或新配置校验失败时保留原配置及运行状态。
订阅地址、内核密钥、票据和 Cookie 不出现在管理接口的状态响应中。

## 采票与业务请求

- 自动采票及手动采票使用独立 WebSocket，每次生成新会话，不复用业务连接池。
- 与参考版本一致，采票会复用**同账号、同模型**仍新鲜的 Cookie；首次采票或 Cookie
  过期不带 Cookie。账号自定义 Cookie 和旧 turn-state 不会混入采票请求。
- 只接收成功终态、形状与长度符合要求且带有效 Cookie 的采票结果，先落库再发布。
- Cookie 独立保留捕获时间，有效窗口为 240 秒，并遵守更短的服务器到期时间。
  业务仅更新 turn-state 时不延长旧 Cookie 的窗口。本地票据仍采用 240 秒缓存上限和
  30 秒余量，这些是网关缓存策略，不是上游模型或票据寿命的保证。
- 自动采票补齐主票与备用票。每张票各自保存 Cookie、会话、代理及可识别的节点信息。
  HTTP 与 WebSocket 业务注入后沿用所选快照，Cookie 或会话变化会隔离 WebSocket 连接。
- 业务成功终态回带合格票时更新票据；鉴权拒绝或回带非目标形状时撤销已发送的票，
  提升备用票并补票。不会为验证票据而重放已经完成的业务请求。
- 票据绑定的代理优先于账号代理及 Resin。普通轮换代理即使地址相同，也**不保证出口 IP 相同**。

## 定向节点学习

托管节点轮换和定向学习是参考版本中两种不同能力。定向学习需要外部 Mihomo 的
专用 Selector 与 listener，不能接管托管模式的 UseOnce 节点组。

外部配置放在 `$DATA_DIR/mihomo-codex/config.yaml`，订阅 provider 的本地 YAML 快照也放在该目录下。
最小结构如下（订阅地址和密钥换成自己的）：

```yaml
mixed-port: 3101
allow-lan: false
external-controller: 127.0.0.1:9098
secret: replace-with-a-private-secret
proxy-providers:
  airport:
    type: http
    url: https://example.com/subscription
    path: ./airport.yaml
    interval: 3600
proxy-groups:
  - name: CODEX-HARVEST-SELECT
    type: select
    use: [airport]
listeners:
  - name: codex-harvest-directed
    type: mixed
    listen: 127.0.0.1
    port: 3102
    proxy: CODEX-HARVEST-SELECT
rules:
  - MATCH,CODEX-HARVEST-SELECT
```

将采票代理设为 `http://127.0.0.1:3102` 并启用节点学习。后端验证控制器、provider
快照和节点身份后，按账号身份、模型、票据形状记录成功、未命中、网络错误、账号错误、
延迟和冷却时间；优先近期成功节点并探索其他节点。定向采票和业务请求持有节点锁直到
响应关闭，节点身份变化时拒绝沿用原绑定。

管理台会明确显示定向学习是否可用。不可用时退回代理轮换，不会把普通代理标成固定出口。

## 范围、任务与历史

- 自动采票可选择全部账号或指定分组，支持仅可调度账号或优先可调度账号。
- 「跳过采票」只影响采票，不修改业务调度资格。账号正在处理业务时不开始新的探测。
- 手动任务支持模型列表、请求间隔、最大请求数、429 等待、成功后停止和随时取消。
  切换规则支持每次请求、312 或两次失败、仅 312、保持当前节点；规则作用于可定向节点。
- 自动与手动任务按账号互斥，请求预算包含 401 刷新令牌后的重试。
- 节点学习和事件保存在 SQLite / PostgreSQL，重启后保留。重置学习记录会推进版本，
  重置前的在途请求不能重新写回旧统计。事件保留 7 天，最多约 10,000 条。
- 管理台轮询任务状态，展示请求次数、HTTP 状态、票据长度、Cookie 数和结果，不展示凭据。

Docker image workflow 先执行 Linux 内核测试、采票集成测试、PostgreSQL 持久化测试和
前端类型检查，再构建 amd64 / arm64 镜像。
