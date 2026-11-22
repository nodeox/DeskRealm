---
phase: 02-m1
plan: "02"
status: complete
requirements-completed: [FR-02]
completed: "2026-11-22"
source_commit: e179902
---

# Phase 2 Plan 02 Summary

交付本地账号完整生命周期、受控邀请和恢复、幂等导入、管理员移交、TOTP、OIDC、用户组和管理委派、控制角色及限时 API 令牌。浏览器和自动化凭据分别进入授权流程。用户列表及对象读取共享范围条件，令牌取签发者当前权限和令牌范围的交集。管理变更写审计及 outbox。

| Deliverable | Verification |
| --- | --- |
| 成员状态、会话撤销、邀请、恢复及重复导入 | PostgreSQL `identity-lifecycle` |
| TOTP 加密存储、注册确认、并发及重放防护、受控重置 | PostgreSQL `identity-federation` 与 Chromium |
| OIDC 授权码、PKCE、浏览器绑定、签名及签发方、受众、nonce、到期和已绑定账号状态 | PostgreSQL `identity-federation` 与本地签名身份提供方 |
| 组织、组及本人范围、管理员移交、令牌撤销和控制角色版本 | PostgreSQL `authorization` 与 Chromium |

认证提交 `654328c`，授权提交 `e179902`。未使用子代理。OIDC 账号由中心主机显式绑定既有本地账号。API 令牌当前开放 `users.read`。控制角色记录操作集合和配置版本，供 M2 的连接 ACL 与受控端使用。

FR-03 继续追踪设备组及其作用域授权，由 02-03 接续。设备登记与审批、通讯录、审计查询和任务仍由后续计划交付。按用户安排，真正的 Linux 中心、中继和双 Windows 验收在完成首版开发后执行。
