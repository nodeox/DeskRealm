---
phase: "2"
slug: m1
status: passed
nyquist_compliant: true
wave_0_complete: true
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

`addressbooks` 通过 PostgreSQL 竞态检查，覆盖个人册、用户和组共享、三种权限、条目设备范围、审计事务回滚、版本冲突和即时撤权。`resources-browser` 在 375 像素宽的 Chromium 中复跑组共享、编辑和撤销，并核对未获授权设备与字段不可见。连接入口联合 ACL 的验证随 M2 进行。

`audit` 通过 PostgreSQL 竞态检查，覆盖真实业务事件、历史范围、查询与导出权限、分页、可信回执去重与冲突、清理和在途保护。`resources-browser` 增加审计授权与撤销、保留期设置、范围筛选、详情和 CSV 下载验证。实际终端回执随 M2 验证。

`tasks` 通过 PostgreSQL 竞态检查，覆盖真实导入部分失败、按对象重试、成功项保留、并发领取、租约过期接管、尝试上限、回执幂等、事务回滚、组外拒绝和旧数据回填。Chromium 通过临时 PostgreSQL 的单次写入失败夹具，验证页面确认、中心工作者执行和回执展示，并核对设备审批结果与组范围。四角色入口、作用域、键盘及窄屏已完成综合回归。

`all` 在提交 `e7e2c46` 通过。22 个固定命名 PostgreSQL 回归在竞态检测下执行，`bootstrap-browser`、`resources-browser` 和 `roles-browser` 各有一个必须通过的 Chromium 用例。`TestOverviewScopesPostgres` 验证在线设备计数与近期任务的授权范围和即时撤权。各需求的 M1 证据与后续验收范围见 [M1 开发验证](02-VERIFICATION.md)。
