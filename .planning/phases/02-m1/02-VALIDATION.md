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

2026-11-22 通过 `bootstrap-api`、`identity-lifecycle`、`identity-federation`、`authorization` 和 `bootstrap-browser`。认证与授权数据库测试包含竞态检查。验证覆盖并发初始化和成员停用、邀请重放、密码及验证器恢复、幂等导入、验证码一次性消费、本地 OIDC 提供方、用户组与本人授权、管理员移交、令牌范围及撤销、控制角色乐观版本和中文浏览器流程。设备组与设备纳管、通讯录、审计查询和任务仍待对应计划验证。
