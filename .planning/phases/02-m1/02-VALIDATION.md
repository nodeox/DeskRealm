---
phase: "2"
slug: m1
status: draft
nyquist_compliant: false
wave_0_complete: false
created: "2026-11-22"
---

# Phase 2 — Validation Strategy

使用 Go `net/http` 测试、临时 PostgreSQL、受控本地 OIDC 夹具及 Vue 类型检查和构建。浏览器回归通过真实 API 驱动首登与权限关键流程。临时开发数据库与 Phase 5 目标 Linux/Windows 验收独立。

| Requirement | Automated check |
| --- | --- |
| FR-02 | 迁移重跑与回滚、并发与过期初始化、密码及身份源、会话撤销和账号恢复。 |
| FR-03 | 管理作用域在列表与对象一致、API Token 过期及撤销、委派边界。 |
| FR-04 | 设备持钥、重复入网、稳定 ID、审批、轮换、启停与状态独立呈现。 |
| FR-07 | 私有与共享通讯录，对象和设备授权一致，分享撤回有效。 |
| FR-13 | 业务与审计回滚一致、关联事件可查、范围导出和回执结果。 |
| FR-18 | 中文首登、登录、授权后页面、窄屏与角色操作端到端路径。 |

每个计划列出快速验证命令及必须失败的分支。阶段收口运行完整单元、集成、构建与浏览器检查。

2026-11-22 通过 `bootstrap-api`、`identity-lifecycle`、`identity-federation`、`authorization` 和 `bootstrap-browser`。认证与授权数据库测试包含竞态检查。验证覆盖并发初始化和成员停用、邀请重放、密码及验证器恢复、幂等导入、验证码一次性消费、本地 OIDC 提供方、用户组与本人授权、管理员移交、令牌范围及撤销、控制角色乐观版本和中文浏览器流程。

`enrollment` 和 `device-lifecycle` 通过 PostgreSQL 回归与竞态检查。设备生命周期覆盖来源组和目标组授权、并发版本冲突、稳定 UUID、ID 和密钥历史、旧凭据失效、恢复与退役。Chromium 从实际台账发放凭据，使用 Ed25519 夹具提交并批准设备，再修改 ID、禁用、用新密钥恢复。

`addressbooks` 通过 PostgreSQL 竞态检查，覆盖个人册、用户和组共享、三种权限、条目设备范围、审计事务回滚、版本冲突和即时撤权。`resources-browser` 在 375 像素宽的 Chromium 中复跑组共享、编辑和撤销，并核对未获授权设备与字段不可见。连接入口联合 ACL 的验证随 M2 进行。M1 审计查询和任务仍待对应计划。
