# M2 实现依据与待验证接入点

**核对日期** 2026-11-22
**仓库基线** `3a42e9c`

## 固定源码与构建

| 组件 | 已核对的来源 | 执行计划 |
| --- | --- | --- |
| RustDesk 客户端 1.5.3 | 标签指向 `95cca3f661f635b3cc85c04935fe8d96d8f432ef`，`libs/hbb_common` 为 `fae47e024dd83cef6aaecde0aa7d11731df11649` | 取完整递归子模块，核对 Git 对象和锁。Windows x64 与 ARM64 使用各自 MSVC 作业与对应 Flutter 配置。 [客户端发布](https://github.com/rustdesk/rustdesk/releases/tag/1.5.3) · [固定构建工作流](https://github.com/rustdesk/rustdesk/blob/95cca3f661f635b3cc85c04935fe8d96d8f432ef/.github/workflows/flutter-build.yml#L157-L206) |
| RustDesk Server OSS 1.1.16 | 发布标签指向 `73523b31cfd25d77dee862e6fc9f5e1fb5e485ef`，`libs/hbb_common` 为 `83419b6549636ee39dacef7776c473f5802e08d6` | 从发布 Git ref 开始维护受管会合补丁，受管协议变更显式同步两份不同的子模块基线。 [服务端发布](https://github.com/rustdesk/rustdesk-server/releases/tag/1.1.16) · [服务端子模块](https://github.com/rustdesk/rustdesk-server/tree/73523b31cfd25d77dee862e6fc9f5e1fb5e485ef/libs) |
| M0 镜像 | `docker.io/rustdesk/rustdesk-server:1.1.16@sha256:8ecdab65deb7c84652a626380e31d11a8f1fbafd97916d57f95c20628f943c00`。本次核对该索引平台条目支持 `amd64`、`arm64`、`arm/v7`，平台条目的 annotations 中未见 revision。M0 手册引用的 `c3a3e0d…` 为源码参考。 | M0 继续由 OCI 摘要固定。若需要声明镜像与 Git commit 对应，再独立核对镜像 provenance。 [M0 手册](../../../docs/DeskRealm-M0-Runbook.md) |

当前开发机的 PATH 未发现 `rustc` 与 Flutter。03-01 建立本地门禁及各架构 Windows runner 的可复跑入口。阶段执行保留真实构建差距，等有构建证据再关闭对应门禁。构建开发制品和 Phase 5 的目标双 Windows 拓扑是各自独立的验收条件。

## 源码落点

| 责任 | 已核对的源码入口 | 待由开发证明 |
| --- | --- | --- |
| 客户端会合与路径 | 主控的 `src/client.rs` 分别处理打洞结果和中继分支。受控端 `src/rendezvous_mediator.rs` 处理通道建立，`src/server.rs` 把普通及中继连接交给 `create_tcp_connection`，随后进入 `Connection::start`。 [主控连接](https://github.com/rustdesk/rustdesk/blob/95cca3f661f635b3cc85c04935fe8d96d8f432ef/src/client.rs#L955-L1015) · [受控端入口](https://github.com/rustdesk/rustdesk/blob/95cca3f661f635b3cc85c04935fe8d96d8f432ef/src/server.rs#L182-L258) · [受控端中继](https://github.com/rustdesk/rustdesk/blob/95cca3f661f635b3cc85c04935fe8d96d8f432ef/src/server.rs#L369-L430) | 逐一确认构建启用的服务、监听和入口汇聚情况。只有测试到实际运行路径后才登记覆盖。未接入受管校验的入口在受管配置中关闭。 |
| 会话执行 | `src/server/connection.rs` 有受控端 `Connection::start`、登录和消息分派；同文件还存在本地 IPC 与权限状态更新。 [受控端初始化](https://github.com/rustdesk/rustdesk/blob/95cca3f661f635b3cc85c04935fe8d96d8f432ef/src/server/connection.rs#L527-L701) · [消息分派](https://github.com/rustdesk/rustdesk/blob/95cca3f661f635b3cc85c04935fe8d96d8f432ef/src/server/connection.rs#L2888-L2922) | 枚举所有能力的启用点、数据方向、挂起队列和结束路径。受管判定先于对应资源启用，并贯穿存续和停止，实际回执从停止路径发出。 |
| 受管会合和中继 | 服务端 `src/rendezvous_server.rs` 的发布源码有 TCP/WS 协调请求入口，`src/relay_server.rs` 处理数据通道配对。 [会合分派](https://github.com/rustdesk/rustdesk-server/blob/73523b31cfd25d77dee862e6fc9f5e1fb5e485ef/src/rendezvous_server.rs#L463-L518) · [中继配对](https://github.com/rustdesk/rustdesk-server/blob/73523b31cfd25d77dee862e6fc9f5e1fb5e485ef/src/relay_server.rs#L408-L455) | 受管会合只接受已认证并绑定会话的协调请求，隔离原版试点。单中继候选的节点级票据、公网池与多区域选点按 M3 交接。受控端两条路径均核销会话。 |

这些是固定源码的静态锚点。操作拒绝、会合限制、端点租约和 Windows 服务行为以加补丁后的构建及运行证据为准。

## M1 复用和缺口

| M1 已存在 | M2 处理 |
| --- | --- |
| `internal/m1/bootstrap.go` 的迁移与数据库池，`authorization.go` 的事务主体和组织写锁，`http.go` 的服务端路由 | 在现有服务内接入，依次落设备运行身份、策略与规则、会话状态。沿用锁顺序，中心的权限变更和会话失效使用同一事务。 |
| `internal/m1/enrollment.go` 的 Ed25519 持钥登记、`device_key.go` 校验及 `device_rotation.go` 生命周期 | 运行身份使用已批准设备的当前公钥。绑定设备 UUID、密钥版本和版本报告，撤销旧密钥对应运行身份。M1 台账暂无可信在线来源及归属用户字段。 |
| `ResolveAddressBookDevice` 单独开启只读事务 | 复用 `bookFor`、`addressEntryFor`。抽取事务内解析，将当前授权检查和签发绑定到同一事务，保留 M1 调用契约。 |
| `control_roles`、`devices.expected_policy_version` / `applied_policy_version`、`audit_events.session_id` 与回执 | 控制角色作能力上限。策略不可变版本与终端回执另外保存，不从 `expected` 推断应用。端点真实事件关联 `session_id`，沿用范围审计和 outbox。 |
| `web/src/App.vue`、`OverviewPanel.vue`、`AuditPanel.vue`、`DevicesPanel.vue` | 增加明确的策略及会话权限，复用 M1 数据加载、记录显示和确认交互。会话数由实际有租约的会话读取模型计算。 |

## 执行中的证据选择

- 03-01 首先固定可复跑源清单、协议、签名和设备上线边界。签名 keyring 与 M0 会合身份分别持久化。用户会话、设备运行身份和管理 API 不互通，并记录拒绝结果。
- 03-02 实现确定性的规则、策略版本和设备回执。重点验证编辑并发、过期与重放、分配优先级、灰度稳定和权限收缩。
- 03-03 先验证 PostgreSQL 中的真实控制面生命周期，再进入 RustDesk 适配。传播延迟、中心并发和端点时间分别取证，服务端不能凭到期时间生成实际断开回执。
- 03-04 以固定源码构建受管件，隔离网络夹具实际验证两路路由和同一受控端安全边界。Windows 原生构建证据记入独立栏。用配置中继完成 M2，节点目录、多区选点及签名发布交 M3。
- 03-05 通过真实 Go API 和生产 Vue 构建验证角色、布局、状态、操作及证据关联，用完整阶段脚本回归 M1 和 M0。
