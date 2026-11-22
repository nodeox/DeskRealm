# Phase 2: M1 用户、设备与管理面 — Context

**Gathered:** 2026-11-22
**Status:** Ready for planning

## Phase Boundary

交付中心控制面、持久化组织资源和首个可操作的中文管理台。运维人员用获授权的管理台查询设备和通讯录。受管 Windows 会话和终端交互由 M2、M3 逐步接入。按项目顺序继续保留目标环境验收至 Phase 5。

## Decisions

- **D-01:** 架构沿用 Go 模块化控制面、PostgreSQL 和 Vue/TypeScript。实例服务一个组织，组织初始化、HTTP API 和迁移脚本形成可复跑的交付入口。
- **D-02:** 第一条使用路径从一次性初始化首个本地管理员开始，随后本地密码、TOTP 和 OIDC 登录。密码和 TOTP 密钥按系统设计存储，停用或归档账号时保留审计关联。
- **D-03:** 组织、组及本人作用域同时约束单资源读写与列表。后台管理权限、控制角色和连接 ACL 分层，API Token 有明确用途、有限作用域、有效期及撤销。
- **D-04:** 设备纳管通过限时入网凭据和设备持钥证明。设备 UUID 稳定，RustDesk ID 可变，审批和连接状态各自维护。审批、任务重试和幂等回执以事务保证一致性。
- **D-05:** 中文控制台遵循 UI 设计和 PDF 原型。优先贯通首个管理员、登录、用户与权限、设备审批、通讯录、审计及任务入口，状态以接口返回的实际结果呈现。
- **D-06:** 组织数据的业务变更和审计在一个事务提交。审计状态区分已确认、待回执和重复回执，查询与导出继承作用域。会话审计在 M2 接入真实授权和受控端事件。

## Canonical References

- `docs/DeskRealm-PRD-v0.1.md` 第 1–4、7 节
- `docs/DeskRealm-System-Design-v0.1.md` 第 1–3、5、7–9、11 节
- `docs/DeskRealm-UI-Design-v0.1.md` 第 2–4、6、7 节
- `docs/DeskRealm-UI-Prototype-v0.1.pdf` 第 1–3、6–8 页
- `.planning/REQUIREMENTS.md` 中 FR-02、FR-03、FR-04、FR-07、FR-13、FR-18，以及 M3 的 FR-16

## Later Phases

受管连接实时授权、策略生效回执、会话撤销与 Windows 对话框由 M2 和 M3 交付。中心最终空机安装、签名分发和跨阶段真实验收按既定路线图执行。
