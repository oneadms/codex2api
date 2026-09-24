# TRAECN 工具调用中断：排查、修复与取证

日期：2026-09-25。修复分支：`codex/fix-traecn-missing-tool-calls`，基于
`custom/main` 的 `c89585b52c19749ef8e2924fe09871ba5a92e9a2`。开始时工作区干净；本文记录合并前的修复验证结果，验证过程未部署或修改生产配置。

已复现并修复三处代码缺陷：空工具数组遮蔽嵌套调用；映射别名漏掉 TRAECN 工具能力；其他渠道的能力缓存覆盖 TRAECN 清单。
使用提供的账号完成了 22 次有效真实请求，获得 18 个可执行调用和 4 次正常最终答复。
这些测试使用可控的合成提示词、工具和历史，**不是原故障 Codex App 会话的抓包**，因此不能断言原线上故障已全部解决。

## 目录

- [已确认的缺陷](#已确认的缺陷)
- [真实请求证据](#真实请求证据)
- [代码检查结论](#代码检查结论)
- [限量诊断](#限量诊断)
- [测试与复现命令](#测试与复现命令)
- [仍缺少的证据](#仍缺少的证据)

## 已确认的缺陷

### 1. 空数组遮蔽真实调用

`traeToolCalls` 原来遇到第一个数组就返回，包括空的 `tool_calls: []`。下列回放中，原始 SSE 明明带有 `read_file`，转换后却只剩进度文字和完成事件：

```text
event: output
data: {"response":"接下来检查……","tool_calls":[],"message":{"tool_calls":[{"id":"call_read","function_call":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"}}]}}

event: done
data: {"finish_reason":"stop"}
```

修复为跳过空数组，继续寻找 `message.tool_calls`、`choices.0.delta.tool_calls`、`choices.0.message.tool_calls`。
多个位置都有非空调用时仍按原有优先级取第一份，避免将镜像字段的参数重复拼接。没有无依据地合并多个不同来源的调用批次。

证据：`traecn-replay-before.log` 中空数组/嵌套调用用例失败；修复后的回放通过。
`TestTraeCNDiagnosticCapturesThreeStages` 从 HTTP 入站运行完整 handler，断言实际出站体与抓取文件一致、原始 SSE 未经转换、Responses 中确有可执行 `read_file`，并核对关联 ID。
真实上游的 22 个有效样本没有出现上述嵌套形态；这是已确认的转换缺陷，尚不能归因到用户原故障。

### 2. GPT 映射别名没有获得 TRAECN 工具能力

把测试扩展到 `CodexModelsManifestHandler` 后发现：`gpt-6-astra → kimi-k3` 被模型列表标记为别名，`owned_by` 是 `codex2api`。
原有清单构建只根据 `owned_by=trae` 添加工具能力，因而别名条目漏掉 `tool_mode=direct`、`apply_patch_tool_type=freeform` 和包含 `max` 的思考档位。
原生 `kimi-k3` 条目则有这些字段。这与界面上显示什么模型名无关。

修复在 TRAECN 专用渠道生成 Codex manifest 时按实际选定渠道应用桥接能力，保留普通模型列表的别名归属。
`TestTraeCNManifestMappedAliasInheritsBridgeCapabilities` 通过完整 handler 验证别名与原生名均有 `direct/freeform/max`，并保持 HTTP 模式。
`traecn-manifest-handler-before.log` 保留了修复前的失败和缺字段的实际 handler 响应。

### 3. 无关 Codex 能力缓存再次覆盖 TRAECN 清单

`applyStoredModelCapabilities` 原来会把其他可见 Codex 账号对 `gpt-6-astra` 学到的 `code_mode_only` 等能力，与未知的 Trae 快照取交集。
即使已经生成了正确的 TRAECN 清单，交集仍会删除 `tool_mode`、`apply_patch_tool_type`。

修复让 TRAECN 专用渠道保留网关实现的桥接契约，不套用其他渠道的缓存。
SQLite 回归测试同时放入 Codex 与 Trae 账号，以及真实结构的能力快照；修复前失败，修复后完整 handler 保留 `direct/freeform`。
其他渠道的能力缓存处理保持原有行为。证据见 `traecn-manifest-before.log`、`traecn-manifest-after.log` 和最终回归日志。

以上两个清单问题会影响客户端注册的工具和调用模式；本次未捕获原故障 App 实际收到的 manifest，不能据此推定其当时确实使用了 code mode。

### 结束原因的观测修正

`emitCompleted` 仍为没有结束原因的完成帧补兼容值 `stop`，但现在同时记录来源：

| 字段 | 含义 |
| --- | --- |
| `finish_reason_source=upstream` | 在上游字段观察到结束原因 |
| `finish_reason_source=gateway_default` | 上游没有给原因，网关补出 `stop` |
| `finish_reason_path` / `finish_reason_event` | 原因所在的字段路径和事件 |

这是日志和取证语义的修正，不是要求每轮都调用工具。正常文本最终答复仍可成功完成；纯思考结束继续沿用既有的空输出失败/截断处理。没有新增自动续跑、追加“继续”、重试策略、强制工具选择或思考隐藏。

## 真实请求证据

同一导出账号、Code 池、`gpt-6-astra → kimi-k3`；目录查询得到 46 个配置，其中包含精确的 `kimi-k3`。
请求发送到该账号配置的上游端点；以下后端信息来自该端点返回的 SSE，不能进一步证明其内部物理路由。

所有 22 个有效样本均核对了 `inbound.json → canonical.json → outbound.json → upstream-0.sse → responses.sse`。
入站是测试构造的 Responses 请求，保留调用前原文；出站均为：

| 参数 | 实际值 |
| --- | --- |
| `model` / `config_name` | `kimi-k3` / `kimi-k3` |
| `function` | `chat_v3` |
| `tool_choice` | `auto` |
| `reasoning_effort` | 最小用例不设置；其余为 `max` |
| `max_tokens` | `4096` |
| `stop` | 未设置 |
| 工具参数 schema | `tools[].function.parameters` 是编码一次的 JSON 字符串 |

上游真实工具增量使用 `tool_calls[].function_call`。不能把声明中的 `function` 与调用历史/响应中的 `function_call` 混为一谈，也没有把 Trae 的 schema 编码直接改成标准 OpenAI 对象格式。
上游接受了 `max` 并返回调用，但这不证明它内部严格实现了该思考档位的语义。

| 对照组 | 实际断言 | 结果 |
| --- | --- | --- |
| 最小单工具 | `read_file(path=README.md)`；关闭仓库原有的提示词 guard | 1 次通过 |
| 恢复 max、代表性编码提示词、多工具集 | 正确工具名、call_id、完成状态、参数 | 3 次通过 |
| namespace、additional_tools | 恢复 `files.read_file` 的名字、namespace 和参数 | 2 次通过 |
| custom、tool_search | 可执行的 custom/tool_search item 及其输入/查询 | 2 次通过 |
| 调用/结果历史 | 读取历史后调用 `write_file(path=config.txt, content=version=2)` | 1 次通过 |
| 已完成任务 | 带工具声明仍可直接答复 `OK` | 1 次通过 |
| 修复 Go Add、修改 JSON 端口、更新 README 命令 | 每项均经历读取、写入、校验、最终答复 | 3 项任务、12 次请求通过 |

三个完整任务使用 35 个工具（含 32 个干扰工具）、约 17–18 KiB 请求体、`max/auto`，走完整 Responses handler。
只执行内存中的夹具读写和预期内容校验；这里的 `run_tests` 是夹具检查器，不是任意 shell 或项目 Go 测试。
任务收到提前的纯文本完成会立即失败；上限 8 轮是测试预算，不是重试。正式运行关闭网关请求重试与恢复，每项实际 4 轮。

逐份核对得到 **18 个原始调用 → 18 个可执行调用，4 次正常最终答复**，无截断的正式抓取。
全部终帧都明确返回 `finish_reason=stop`，包括含工具调用的响应。因此 `stop` 本身不能证明“没有调用”。

### 上游自报模型不一致

20 次 SSE 自报 `provider_model_name=ali-kimi-k3`；另外两次出站仍是 K3，却自报 `kimi-k2.6`，且 `timing_cost.is_retry=true`、`extra_info.model` 与其一致：

| 关联 ID | 用例 | 结果 |
| --- | --- | --- |
| `a9970fe3-4018-4c6c-965e-f14458242c73` | custom apply_patch | 可执行调用通过 |
| `35ab97ed-a88f-49c2-a505-1a4cb0bcf8ac` | 完整任务中的一轮 | 可执行调用通过 |

另有一次 `is_retry=true` 仍自报 K3。以上是同一次网关请求内上游报告的行为，不是测试通过网关重试掩盖失败。
两次模型变化均未造成缺调用，不能认定它就是用户停止现象的原因。诊断已记录这些提供方字段，便于后续对照。

### 测试夹具修正记录

首次完整任务探针把无参数工具的 `required` 编成了 `null`，Kimi 返回 `4027`。同时发现测试 Store 构造器会默认启用请求重试。
修正夹具为 `required: []`，并在 handler 创建后显式设置普通/限流重试次数为 0，重新完整运行三项任务。
该初次运行的 17 份抓取不计入上述 22 个有效样本；没有将失败计为通过，也没有据此修改生产工具转换。

本地证据目录（被 Git 忽略）：`logs/traecn-investigation-20260925/`。
`evidence-summary.json` 索引每个关联 ID、出站工具名、原始调用 ID、转换后的 item、提供方模型和结束原因。
正式样本在 `traecn-kimi-k3-first/`、`traecn-kimi-k3-matrix/`、`traecn-kimi-k3-tasks-fixed/`；同名 `.log` 保存断言结果。
已针对账号实际 access/refresh token 扫描这些保存文件，未发现凭据。

## 代码检查结论

| 路径 | 核对结果 |
| --- | --- |
| `traeCNRequestBodyPlan` | 上述样本目标 config、function、effort、tokens、choice 正确；stop 未设置。保留现有 auto 行为。未知 config 的旧有回退分支仍存在，但正式样本没有走该分支。 |
| `traeCNMessagesFromResponses` | developer 转 system；函数调用与结果按 call_id 配对；历史使用原生 function_call。单轮历史和三项完整任务均通过。 |
| `traecn_tools.go` / `traecn_tool_identity.go` | additional_tools、namespace 展平/恢复、custom 和 tool_search 桥接已实测；未发现本次所需工具被静默丢弃。 |
| 不支持的托管工具 | web_search、image_generation、MCP/computer 等声明在现有转换器中明确跳过，不等于普通 function 转换丢失。诊断将其标为 unsupported。历史 item 的可表示性另有处理。 |
| `parseTraePayload` / `consume` | 核对 native SSE 和嵌套 data 解包；结束帧先消费工具调用再完成；回归覆盖参数分片与终帧完整快照。 |
| 模型清单 | 已修复两处会漏掉 direct/freeform 的路径；完整 handler 回归通过，原 App 的实际清单/工具模式仍待抓取。 |

## 限量诊断

默认关闭。为本地测试进程设置：

```bash
export TRAECN_DIAGNOSTIC_DIR=logs/traecn-diagnostics-run1
export TRAECN_DIAGNOSTIC_REQUESTS=3
export TRAECN_DIAGNOSTIC_RAW=1
export TRAECN_DIAGNOSTIC_MAX_BYTES=1048576
```

| 变量 | 行为 |
| --- | --- |
| `TRAECN_DIAGNOSTIC_DIR` | 显式指定本地捕获目录 |
| `TRAECN_DIAGNOSTIC_REQUESTS` | 必须大于 0；最多 20 个请求目录，已有 `traecn-*` 目录占名额，重启不重置 |
| `TRAECN_DIAGNOSTIC_RAW` | 只有 `1` 才保存正文；其他值仅存元数据 |
| `TRAECN_DIAGNOSTIC_MAX_BYTES` | 每份原始文件默认 1 MiB，上限 16 MiB |

每个请求使用同一个 `request_id`，入站元数据另含 `gateway_request_id`；常规终态日志可关联到该目录。
原始文件有 `inbound.json`、`canonical.json`、`outbound.json`、`upstream-0.sse`、`responses.sse`。
直接调用 executor 时若没有 HTTP 入站快照，会明确记为 `executor-input.json`，不会冒充原始 HTTP 抓包。
既有游标重连如发生，会用同一 ID 下的 `upstream-1.sse` 等编号保存；本修改不启用或新增重连。

`metadata.jsonl` 记录工具转换前后名称/类型/数量、载体来源、支持类型、工具契约、请求参数、解析字段数量及上游模型信息，限 256 KiB。
独立的 `terminal.json` 限 16 KiB，即使逐帧元数据达到上限仍保留结束来源。
原始文件截断会写 `.truncated` 标记和字节统计。早期真实抓取的终态位于 `metadata.jsonl`，尚无后来补充的独立终态文件。

目录权限 0700、文件 0600；不保存请求/响应鉴权头；脱敏已知账号、请求头凭据及常见 JSON 鉴权字段。
RAW 文件仍包含任务提示词、推理文本或工具结果，应用于显式开启的本地取证，不进入常规日志或 Git。
流文件只反映实际读到的字节，连接取消、进程退出或截断时不能当作完整的上游响应。

诊断元数据中的 tool_count 是 namespace 展平后的叶子声明数，可能含重复声明和不支持类型；出站数量表示实际发出的函数数。
具体的 stop 值及完整请求对象只在 RAW 文件内；只开元数据时记录 stop 是否存在/类型。
捕获满后无需重启仍会停止生成新文件；再次取证应选择新的目录，或关闭 REQUESTS。

## 测试与复现命令

运行版本：`go version go1.26.6 darwin/arm64`，与 `go.mod` 一致。

最终代码已执行：

```bash
GOTOOLCHAIN=go1.26.6 go test ./...
GOTOOLCHAIN=go1.26.6 go vet ./...
GOTOOLCHAIN=go1.26.6 go test -race ./proxy -run 'TestTraeCNManifest|TestTraeCNDiagnostic|TestTraeToolCalls|TestTraeCNStreamEmptyTopLevel|TestTraeCNEmptyAliases|TestTraeCNFinishReasonSource|TestTraeCNStreamDuplicate' -count=1
```

全部通过；一次未命中缓存的全仓 proxy 运行用时 61.565 s，最终定向 race 用时 5.918 s，vet 退出 0。
日志：`traecn-final-all-tests.log`、`traecn-final-race.log`、`traecn-final-vet.log`。
离线回归覆盖正常函数调用、空别名、镜像去重、分片参数、结束帧调用、文本最终答复、纯思考结束、截断、诊断限量/脱敏/原始流保持和模型清单。
真实测试在无环境变量时跳过，不会在普通 `go test ./...` 中消耗账号。

真实矩阵先单独运行 `minimal`，再运行其余 9 项，分别记录 `first` 与 `matrix` 日志。可用下列命令重现同等的完整矩阵；路径替换为本地已授权导出文件：

```bash
TRAECN_TOOL_PROBE_FILE=/absolute/path/traecn-accounts.json \
TRAECN_TOOL_PROBE_POOL=code \
TRAECN_DIAGNOSTIC_DIR=logs/traecn-tool-matrix-new \
TRAECN_DIAGNOSTIC_REQUESTS=10 TRAECN_DIAGNOSTIC_RAW=1 \
GOTOOLCHAIN=go1.26.6 go test ./proxy -run '^TestLiveTraeCNToolMatrix$' -count=1 -v

TRAECN_TOOL_PROBE_FILE=/absolute/path/traecn-accounts.json \
TRAECN_TOOL_PROBE_POOL=code \
TRAECN_DIAGNOSTIC_DIR=logs/traecn-tool-tasks-new \
TRAECN_DIAGNOSTIC_REQUESTS=20 TRAECN_DIAGNOSTIC_RAW=1 \
GOTOOLCHAIN=go1.26.6 go test ./proxy -run '^TestLiveTraeCNRepositoryTasks$' -count=1 -v
```

需要逐步对照时，`TRAECN_TOOL_PROBE_CASES` 可选逗号分隔的 `minimal,max,codex_prompt,tool_set,namespace,additional_tools,custom,tool_search,history,final_answer`。
已强化旧的 `TestLiveTraeCNWorkStreamWithTools`，要求实际得到 `get_weather(city=上海)`，不能只打印事件名称判成功；该 Work/Doubao 探针本次未联网执行，因为它不是本次 K3/Code 对照组。
未运行前端构建/浏览器测试，前端没有修改。

## 仍缺少的证据

- 原故障 Codex App 同一次请求的入站全文、实际出站、原始 SSE 与转换事件；本次合成任务成功不能替代该样本。
- 原 App 收到的 model manifest 和本轮实际工具声明，以确认其确实采用 direct，并排除已有客户端缓存。
- 同账号、同模型、同模式的 Trae 官方客户端成功样本；本次没有取得。
- `required` / 指定工具的上游支持证据：本次未把它们作为生产配置，也没有在缺乏协议依据时测试并假定支持。
- 上游自报 K3→K2.6 的内部原因，以及它是否与失败请求相关；目前两份变化样本的工具调用均成功。

上述缺口无需再提供 token；已提供账号足以运行现有探针，原故障需用限量诊断捕获当时请求才能进一步归因。
