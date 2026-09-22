# sub2api 来源说明

Mihomo 管理模块、日志脱敏模块及采票控制、节点排序算法移植自
[ranxi2001/sub2api v2.8.0](https://github.com/ranxi2001/sub2api/releases/tag/v2.8.0)，
提交 `3719ff895d3d8565b750a40e01a76a57c864a758`。

对应目录：`internal/mihomo`、`internal/util/logredact`、`internal/harvest`。
原代码采用 LGPL-3.0，完整许可见本目录 LICENSE。其余接入代码为本项目适配，
包括 SQLite/PostgreSQL 持久化、React 管理台、账号调度和 WebSocket 采票。
采票沿用同账号、同模型仍处于有效窗口的 Cookie，首次或过期后不携带；
只捕获合格响应的 Cookie 用于业务请求。

Mihomo 内核由管理台按需从 MetaCubeX/mihomo 官方发行版下载，安装前验证 SHA-256。
