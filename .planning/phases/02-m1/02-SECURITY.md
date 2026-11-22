---
phase: 02-m1
status: secured
threats_open: 0
source_commit: e7e2c469094592cc5b4fe93c2be45dbc7e6e3c7a
---

# M1 威胁模型核对

核对范围为 02-01 至 02-04 的控制面实现及本地回归。实际终端及公开部署的交付验证分别由后续阶段执行。

| 威胁 | 代码与验证 |
| --- | --- |
| T-02-01、T-02-04 | `bootstrap.go` 通过 `bootstrap_state` 行锁核对票据摘要、期限与消费，组织、首管、审计及 outbox 共用事务。`TestBootstrapPostgres` 覆盖竞争、重复与失败回滚。 |
| T-02-02、T-02-03 | `http.go` 实施同源请求、会话、CSRF 和登录限流，`main.ts` 清除 fragment 中的票据。`TestBrowserSessionPostgres` 与 `bootstrap-browser` 检查真实会话生命周期和浏览器入口。 |
| T-02-05 | `totp.go` 保护持久密钥并锁定一次性时间步。`oidc.go` 校验 state、浏览器绑定、PKCE、nonce 与 issuer/subject。`TestTOTPReplayPostgres`、`TestOIDCBindingPostgres` 验证。 |
| T-02-06 至 T-02-08 | `authorization.go` 与 `api_tokens.go` 交叉核对角色和令牌实时范围，`identity.go` 在状态变更事务中撤销会话。授权、令牌、身份、移交及浏览器用例核对范围和一次性凭据。 |
| T-02-09 至 T-02-12 | `enrollment.go` 验证请求绑定的 Ed25519 持钥证明并事务消费。设备生命周期校验组权限和版本。`addressbook_entries.go` 同时执行设备与通讯录授权，共同审计变更。设备、通讯录及浏览器回归通过。 |
| T-02-13、T-02-14 | `audit.go` 统一列表、详情与导出权限，并保留事件历史组。`audit_events.go` 将业务事件和 outbox 原子追加，对来源和结果回执去重。范围和回执数据库用例及导出浏览器回归通过。 |
| T-02-15、T-02-16 | `tasks.go` 校验当前组织、请求者和对象版本。`task_worker.go` 使用行锁、尝试令牌、租约和事务提交。过期工作者、混合目标、组外请求、回执重放及四角色浏览器回归通过。 |
| T-02-SC | Go 与 npm 锁文件未变。`all` 校验 Go 模块、已安装 npm 依赖树并使用固定摘要的 PostgreSQL 镜像。 |

`TestOverviewScopesPostgres` 另核对在线计数和近期任务随组授权即时变化。四角色浏览器用例覆盖可操作待办筛选和范围审计深链接。完整版本与运行命令见 [M1 开发验证](02-VERIFICATION.md)。
