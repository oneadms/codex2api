# sub2api 来源说明

Mihomo 管理模块、日志脱敏模块及采票控制、节点排序算法移植自
[ranxi2001/sub2api v2.8.0](https://github.com/ranxi2001/sub2api/releases/tag/v2.8.0)，
提交 `3719ff895d3d8565b750a40e01a76a57c864a758`。

对应目录：`internal/mihomo`、`internal/util/logredact`、`internal/harvest`。
原代码采用 LGPL-3.0，完整许可见本目录 LICENSE。其余接入代码为本项目适配，
包括 SQLite/PostgreSQL 持久化、React 管理台、账号调度和采票协议接入。

本次门票流程参考同仓库 `production` 分支，固定提交
[`3c54ff2a32b681a846162af2da68063978f7b1e1`](https://github.com/ranxi2001/sub2api/tree/3c54ff2a32b681a846162af2da68063978f7b1e1)，
对应上游文档 `docs/changes/targeted-ticket-probe.md`、`docs/codex-parallel-harvest.md`、
`docs/astra-ticket-acquisition.md`。适配涉及 `auth/codex_ticket*`、`proxy/codex_*harvest*`、
`internal/mihomo`、`internal/harvest` 及 React 采票工作台。

本次对齐内容包括 780 票同会话续接验收、SSE / WebSocket 协议隔离、托管 Mihomo 独立多通道、
共享实际请求预算、节点预留和业务回选，以及来源票换发目标新票的快速复用。
自采不沿用旧 Cookie；快速复用只借来源启动材料，目标票仍需独立完整验收。
没有引入上游独立网关、BPS 或其他与本次采票流程无关的模块。

Mihomo 内核由管理台按需从 MetaCubeX/mihomo 官方发行版下载，安装前验证 SHA-256。
