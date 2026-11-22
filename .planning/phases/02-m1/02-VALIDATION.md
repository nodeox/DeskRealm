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
