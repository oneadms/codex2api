# TRAECN 账号积分（Code / Work 双池）

账号页的「积分用量」列按端点分池显示：**Code**（IDE 侧可用额度）和 **Work**（Work 专属额度）各自显示总量、已消耗、剩余量和消耗百分比。两个池分开消耗、互不通用，因此不合并成一个数字。进度条沿用 Codex 账号的颜色：低于 70% 为绿色，70% 起为黄色，90% 起为红色。

## 数据来源

服务端调用 `POST https://api.trae.cn/trae/api/v2/pay/ide_user_ent_usage`，默认按 Work 侧取全集：

```json
{"require_usage":true,"req_source":2}
```

- `req_source=2`（Work / Lite 客户端视角）返回全部权益包；
- `req_source=1`（IDE 客户端视角）看不到 Work 专属包，仅在上面的请求失败时作为兜底。

上游的 `usage_summary` 是两个池的**合计**（实测同一账号：`total_amount=5250`、`consumed_amount=3250`，而 IDE 侧只有 3250 额度），因此不能当单池余额。真正的分池依据是每个权益包的 `entitlement_base_info.available_endpoint`：

- `0` → **Code 池**（IDE 侧可用，含通用包、月赠、签到奖励等）
- `1` → **Work 池**（Work 专属包，IDE 客户端看不到）

解析规则：只统计带 `quota.credits_limit`（> 0）的积分包，功能权益（`enable_solo_*` 等没有额度上限的包）不计入；同一权益包在两次查询里会用 `entitlement_id` 去重；已消耗按 `usage.credits_amount` 累加并按包上限截断；剩余量不低于 0，进度条限定 0–100%。

实测同一账号的两个池：

| 池 | 总量 | 已消耗 | 剩余 | 上游 `cn_credits_remain_info` |
| --- | --- | --- | --- | --- |
| Code（endpoint 0） | 3250 | 3250 | 0 | `ide_credits: 0` |
| Work（endpoint 1） | 2000 | 0 | 2000 | `work_credits: 2000` |

推理响应里的用量通知也直接给这两个数：`cn_credits_remain_info: {"ide_credits":0,"work_credits":2000}`，网关会解析它并写回账号（见下）。

数据缺失、非积分计费或没有任何有效权益包时显示查询失败，不能把未知值或美元额度当成零积分。这份汇总代表上游当前权益，不是账号终身累计获得或消耗的积分。

## 用哪个池扣费：access_type

`POST {host}/api/agent/v3/llm_utils_chat` 的请求体用 `access_type` 选择端点（同一次抓包对比实测）：

- 不带该字段或 `access_type=0` → **Code（IDE）池**。IDE 池见底时上游在 HTTP 200 的 SSE 里回 `{"code":4008,"message":"Your requests have exceeded the quota."}`，一点都不扣。
- `access_type=1` → **Work 池**，请求成功并且 `work_credits` 下降（`access_type=2` 同样扣 Work 池，网关固定用 1）。

除此之外，客户端身份字段（`client_type` / `is_remote_req` / `agent_type` / `mode_type` / `req_source`）都不影响扣哪个池。模型选择在两个端点上都用同一份目录（`/api/ide/v1/batch_get_detail_param`），但 `access_type=1` 时同样只接受目录里真实存在的 `config_name`，乱填会回 `4001 the param is invalid`。

## 账号级积分池设置

管理台 TRAECN 的编辑弹窗里有「积分池」下拉，对应账号凭据 `traecn_credits_pool`：

- `auto`（默认）：先按 Code 池发；当网关明确知道「Code 池剩余为 0 且 Work 池还有额度」时，自动给请求加上 `access_type=1`。
- `code`：只用 IDE 侧额度（回落行为与改动前一致）。
- `work`：强制走 Work 端点。

`auto` 依赖的余额快照有两个来源，都带 15 分钟有效期，过期就回到 Code 池（避免上游补额度后继续扣错池）：

1. 管理台积分查询的结果（打开账号页时每分钟刷新，服务端缓存 1 分钟）；
2. 上游响应 SSE 里的 `cn_credits_remain_info`：即使请求以 4008 失败，这条通知也会带回来，网关扫描响应流时顺手记录。

额度不足撞上 Code 池用尽、但 Work 池还有额度时，账号只做 5 秒的换池冷却（而不是原来的 5 分钟额度冷却），让紧接着的重试就能用 Work 端点；Work 池也空了才恢复成完整的额度冷却。

## 状态列上的积分徽章

额度快照（15 分钟有效）会决定账号列表状态列上的徽章，避免出现「积分已经 100%、状态还是可用」的误导：

- Code 池还有额度 → 只有普通状态徽章；
- Code 池用尽、Work 池还有额度（且没被强制只用 Code）→ 额外显示「只剩 Work 积分」，请求会自动走 Work 端点；
- 可用的池都是 0 → 额外显示「积分用尽」，账号按限流处理到下一次日探针（默认 24 小时），不再按分钟级反复试探；
- 快照过期或从来没查到过 → 不加徽章，不做判断。

积分列本身在剩余为 0 时也会直接写「已用尽」，不再只显示「剩余 0」。

## 额度不足的账号怎么恢复：每日探针

额度不足（上游 4008）不是瞬时抖动，积分只会随签到或订阅周期回补，所以：

1. 命中 4008 时账号**直接按限流处理**，冷却到下一次日探针（`TRAECN_QUOTA_COOLDOWN_MINUTES`，默认 1440 分钟，范围 5–1440）；
2. 调度和重试都不会再反复试探这个账号（不再出现「5 分钟后又可用、然后又失败」的抖动）；
3. 每天一个随机时刻（按 `日期 + 账号 ID` 派生，同一天同账号时刻固定，重启不会挪动）对该账号做**一次**探针：查询双池余额；
   - 查到可用额度（Code 有余额，或 Code 用尽但 Work 有余额）→ 解冻，账号恢复为可用，并写账号事件「额度已恢复」；
   - 仍然为 0 → 冷却续到下一次日探针；
   - 查询本身失败（网络/上游）→ 不当作恢复，也不延长，第二天同一时刻再试；
4. 探针只针对冷却/错误状态的账号，管理员手动停用的账号不探；`TRAECN_CREDITS_PROBE_DISABLED=1` 可以整体关闭。

探针用积分查询而不是发一次推理请求：额度不足的根因就是积分，查积分能直接回答「额度回来了吗」，而且不消耗账号积分、不依赖模型目录。管理台打开账号页时每分钟也会刷新当前页余额，余额一恢复就会提前解冻，不必等满 24 小时。

## 查询与缓存

- 管理接口：`GET /api/admin/accounts/:id/traecn/credits`；加 `?refresh=1` 手动刷新。
- 响应为 `{"credits":{"pools":[{"kind":"code",...},{"kind":"work",...}]},"stale":false,"error":""}`；失败池只有 `kind` 与 `error`，不带余额字段。`TRAECN_CREDITS_WORK_DISABLED=1` 时只查 IDE 视角，Work 池标记为不可用。
- 只在管理台当前页可见时每分钟查询当前页账号；翻页或离开页面取消浏览器请求。签到和刷新令牌后也会重新查询积分。
- 前端批量查询最多 4 个并发，服务端上游查询总并发同样限制为 4；同账号的同时查询合并为一次。
- 服务端成功与失败结果均缓存 1 分钟，最多保留 1024 个账号；重启后重新查询，不需要数据库迁移。积分查询结果同时写入账号的内存余额快照，供 `auto` 选池。
- 失败时保留缓存中的最后成功值，标注「数据已过期」并保留实际更新时间；从未查询成功的账号显示失败提示。
- 单次响应最多读取 1 MiB，出站请求超时 20 秒，共享查询含排队和令牌检查最多 45 秒。令牌刷新仍遵守现有凭据轮换保护。

积分查询使用账号的市场客户端指纹、当前访问令牌和代理/Resin 出口。浏览器只接收汇总数据，不接收上游令牌或完整权益包。积分展示本身不修改账号调度状态；上游明确限流或拒绝额度的冷却规则见 [断线恢复与限流处理](traecn-resume.md#429-与额度不足)。

## 怎么自己复核

`proxy/traecn_live_probe_test.go` 里有一组默认跳过的真实上游探针（会消耗少量积分），用导出的账号 JSON 打开：

```bash
TRAECN_LIVE_CREDITS_FILE=/path/traecn-accounts-YYYY.json \
  go test ./proxy/ -run TestLiveTraeCN -v -count=1
```

- `TestLiveTraeCNCreditsRaw`：打印两个 `req_source` 的权益包与 `available_endpoint`，确认分池规则。
- `TestLiveTraeCNWorkPoolProbe`：逐候选身份发一次 tiny 请求，对比两个端点的余额变化（`access_type=1` 是唯一扣 Work 池的取值）。
- `TestLiveTraeCNWorkCatalogAndBody`：对比两个端点可用的 `config_name`，并验证网关真实请求体在 Work 池下的表现。
- `TestLiveTraeCNGatewayBodyOnWorkPool`：用 `buildTraeCNRequestBody` 构造真实请求体，验证不带 `access_type` 会被 4008 拒绝、带上 1 就能拿到正文。
