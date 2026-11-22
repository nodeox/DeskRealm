# Phase 02 · M1 用户、设备与管理面研究

**日期** 2026-11-22
**置信度** HIGH（阶段需求、依赖版本、容器镜像）／MEDIUM（基于官方规范的实现建议）

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions

- **D-01:** 架构沿用 Go 模块化控制面、PostgreSQL 和 Vue/TypeScript。实例服务一个组织，组织初始化、HTTP API 和迁移脚本形成可复跑的交付入口。
- **D-02:** 第一条使用路径从一次性初始化首个本地管理员开始，随后本地密码、TOTP 和 OIDC 登录。密码和 TOTP 密钥按系统设计存储，停用或归档账号时保留审计关联。
- **D-03:** 组织、组及本人作用域同时约束单资源读写与列表。后台管理权限、控制角色和连接 ACL 分层，API Token 有明确用途、有限作用域、有效期及撤销。
- **D-04:** 设备纳管通过限时入网凭据和设备持钥证明。设备 UUID 稳定，RustDesk ID 可变，审批和连接状态各自维护。审批、任务重试和幂等回执以事务保证一致性。
- **D-05:** 中文控制台遵循 UI 设计和 PDF 原型。优先贯通首个管理员、登录、用户与权限、设备审批、通讯录、审计及任务入口，状态以接口返回的实际结果呈现。
- **D-06:** 组织数据的业务变更和审计在一个事务提交。审计状态区分已确认、待回执和重复回执，查询与导出继承作用域。会话审计在 M2 接入真实授权和受控端事件。

### Later Phases

受管连接实时授权、策略生效回执、会话撤销与 Windows 对话框由 M2 和 M3 交付。中心最终空机安装、签名分发和跨阶段真实验收按既定路线图执行。
</user_constraints>

## Summary

第一份计划交付完整首登路径。管理员在主机生成一次性初始化链接，浏览器创建首位本地管理员，再以密码进入中文控制台。Go 提供同源 API 和前端静态文件，PostgreSQL 持久化组织、账号、浏览器登录会话及审计。以真实数据库和浏览器完成这条路径，再增加身份源及作用域，实现设备审批、通讯录和任务入口。[VERIFIED: .planning/phases/02-m1/02-CONTEXT.md:12-17；.planning/ROADMAP.md:28-36；docs/DeskRealm-UI-Design-v0.1.md:48-56]

## Architectural Responsibility Map

| 能力 | 主责 | 协作 |
| --- | --- | --- |
| 中文初始化、登录、组织资源和任务页面 | Vue 同源浏览器端 | Go API 返回认证状态、可用操作及实际结果 |
| 初始化、本地与联邦认证、登录会话 | Go 身份模块 | PostgreSQL 保存票据、身份和会话，OIDC 服务签发身份凭证 |
| 管理授权和查询作用域 | Go 授权模块 | PostgreSQL 按组织、组和本人范围过滤 |
| 设备、通讯录、任务、审计 | Go 业务模块 | PostgreSQL 约束稳定 UUID、原子写入、幂等和审计 |

上述分工对应单组织控制面、持久化模型和界面约束。[VERIFIED: .planning/phases/02-m1/02-CONTEXT.md:12-17；docs/DeskRealm-System-Design-v0.1.md:166-196；docs/DeskRealm-UI-Design-v0.1.md:13-30]

## Project Constraints (from AGENTS.md)

- 按 GSD 入口安排改动，沿用 Linux Compose、Go、PostgreSQL、Vue/TypeScript 和现有 M0 传输部署边界。[VERIFIED: AGENTS.md:11-17,49-61]
- 管理授权、会话准入和受控端执行分层。重试保留身份和业务数据。公开连接配置与按设备发放的入网凭据各走独立交付路径。开发期完成构建与自动化检查，首版真实目标环境验收仍在 Phase 5。[VERIFIED: AGENTS.md:13-17；.planning/ROADMAP.md:62-70]

<phase_requirements>
## Phase Requirements

| ID | 本阶段验收对象 |
| --- | --- |
| FR-02 | 单次初始化、密码/TOTP/OIDC、账号生命周期、受控导入、可追溯恢复及登录会话撤销。 |
| FR-03 | 组织、组、本人范围上的资源和动作授权，限用途、范围、有效期且可撤销的 API Token。 |
| FR-04 | 设备入网和审批，稳定 UUID，独立的登记、在线及策略回执状态，轮换、禁用、退役可追溯。 |
| FR-07 | 私有与共享通讯录、用户/组授权、查看/编辑/管理和 M2 连接 ACL 交接。 |
| FR-13 | 原子审计、范围查询/导出、留存与会话 ID 关联、已确认/待回执/重复回执。 |
| FR-18 | 中文首登及各角色的可操作页面，使用 API 的实际状态与权限，具备任务查询和重试入口。 |

来源为同次读取的 `FR-02`、`FR-03`、`FR-04`、`FR-07`、`FR-13`、`FR-18`。[VERIFIED: .planning/REQUIREMENTS.md:16-23；docs/DeskRealm-PRD-v0.1.md:49-80]
</phase_requirements>

## Standard Stack

| 层 | 可执行基线 | 版本/来源 |
| --- | --- | --- |
| Go HTTP | Go `1.27.4`，标准库 `net/http`、`crypto/rand`、`crypto/ed25519` | 2026-11-10，已安装。[VERIFIED: 本地 go version、Go release history] [发行记录][go-release] |
| PostgreSQL | `postgres:18.7-bookworm@sha256:194e55feb2597b1465dbee81cc9fd8d204566cd2eb42c8d22fd6c931a38781b1` | 受支持的 18 分支，已核对官方 OCI 索引含 amd64/arm64。卷挂在 `/var/lib/postgresql`，此镜像的 `PGDATA` 为 `/var/lib/postgresql/18/docker`。[VERIFIED: PostgreSQL 版本表、Docker Hub OCI 索引和固定 Dockerfile] [支持矩阵][pg-versions] · [镜像源][pg-image] |
| PostgreSQL 客户端和迁移 | `github.com/jackc/pgx/v5` `v5.13.0`，`github.com/pressly/goose/v3` `v3.30.1` | 分别发布于 2026-11-11、2026-11-12。运行 Go 池和事务，迁移通过 Goose 独立入口调用。[VERIFIED: Go Module Proxy 与官方仓库] [pgx][pgx] · [Goose][goose] |
| 密码与 TOTP | `golang.org/x/crypto` `v0.59.0`，`github.com/pquerna/otp` `v1.5.0` | 分别发布于 2026-11-11、2024-12-31。应用负责持久策略和重放防护，密码 KDF 与 TOTP 计算使用库。[VERIFIED: Go Module Proxy 与官方仓库] [Argon2][argon2] · [TOTP 库][totp-lib] |
| OIDC | `github.com/coreos/go-oidc/v3` `v3.21.0`，`golang.org/x/oauth2` `v0.38.0` | 分别发布于 2026-09-01、2026-11-06。发现、验签及 OAuth 授权码兑换使用维护中的库。[VERIFIED: Go Module Proxy 与官方仓库] [OIDC 库][oidc-lib] · [OAuth2 库][oauth-lib] |
| 浏览器构建 | Node LTS `26.13.1`，Vue `3.5.46`、Vue Router `4.6.5`、Vite `8.3.8`、Vue 插件 `6.0.11`、TypeScript `6.0.3`、vue-tsc `3.3.14` | 使用 Vue 官方 TypeScript/Vite 结构。Vite 和插件声明的 Node 约束为 `^20.19.0 \|\| >=22.12.0`，Vue Router 4.6.5 声明 Vue `^3.5.0`，Vue 插件接受 Vite 8，vue-tsc 声明 TypeScript `>=5.0.0`。[CITED: vuejs.org/guide/quick-start.html、vite.dev/guide/、router.vuejs.org/guide/] [VERIFIED: Node 发行索引、npm 候选版本元数据] |
| 浏览器验收 | `@playwright/test` `1.65.1` | 用生产构建及同源 HTTP 入口执行关键用户路径。[CITED: playwright.dev/docs/test-webserver] [VERIFIED: npm 候选版本元数据] |

用 `go.mod`、`go.sum` 和 npm 锁文件固定版本。Vue 采用原型的「端域 / DeskRealm」品牌、中文状态、左导航与详情抽屉。开发由 Vite 代理同源 API，生产使用打包后的静态资源；Go 应保证 API 路由优先于 SPA 回退。[VERIFIED: docs/DeskRealm-UI-Design-v0.1.md:13-46,48-63；docs/DeskRealm-UI-Prototype-v0.1.pdf:1-3,6-8] [CITED: vite.dev/guide/]

## Package Legitimacy Audit

| npm 包 | 发布时间 UTC | 周下载 | 权威来源／结果 |
| --- | --- | --- | --- |
| `vue` | 2026-11-20 | 26.8M | [Vue 核心][vue-repo] · SUS |
| `vue-router` | 2026-11-13 | 13.4M | [Vue Router][router-repo] · SUS，选择 Vue 3 Router 4 分支 |
| `vite` | 2026-11-11 | 289.1M | [Vite][vite-repo] · SUS |
| `@vitejs/plugin-vue` | 2026-11-12 | 15.7M | [Vue Vite 插件][vite-vue-repo] · SUS |
| `typescript` | 2026-04-16（选定 6.0.3） | 451.5M | [TypeScript 6][ts] · OK [VERIFIED: npm registry] |
| `vue-tsc` | 2026-10-29 | 9.8M | [Vue 语言工具][vue-tsc-repo] · SUS |
| `@playwright/test` | 2026-11-13 | 138.9M | [Playwright][pw-repo] · SUS |

官方文档和独立 registry 查询已交叉核对。GSD 对上述 npm 矩阵以包名检查，SUS 均由于 `too-new`，各候选的 `scripts.postinstall` 查询未返回脚本。请在一次前端依赖核对 checkpoint 中确认每个 SUS 的固定版本、官方来源和锁文件解析，再安装这些包。包名检查无 SLOP。GSD 1.22.0 包闸门只支持 `npm|pypi|crates`。Go 模块以对应官方项目、Go Module Proxy 固定版本和 `go mod verify` 校验。[VERIFIED: GSD package-legitimacy 输出、npm registry、Go Module Proxy]

## Architecture Patterns

### 1. 首登、迁移和共用身份

- 以显式 `migrate`/启动入口应用 Goose 版本化 SQL，每个建表版本默认由事务应用。持久数据库可反复启动，已经应用的版本按迁移记录判断。部署只允许一个迁移执行者，使用受限应用数据库角色及可用性检查。用镜像匹配的 PostgreSQL 做空库迁移、重复迁移和失败回滚测试。[CITED: github.com/pressly/goose；postgresql.org/docs/18/tutorial-transactions.html]
- 迁移创建唯一的初始化控制行。主机命令用随机高熵单次令牌生成链接，仅保存摘要、到期时间和消费状态。签发和兑换使用同一控制行事务锁。提交时在锁内重查未初始化及未到期，消费票据，创建组织、首个用户、完整管理员角色和审计后提交。事务失败不消费，重试不能创建第二个管理员。首次成功返回标准登录界面。令牌避免出现在服务器访问日志中，并在首登页移除 URL 中的令牌及禁用 Referer 泄露。[VERIFIED: docs/DeskRealm-System-Design-v0.1.md:133-138；docs/DeskRealm-UI-Design-v0.1.md:48-53] [CITED: postgresql.org/docs/18/explicit-locking.html]
- 本地密码用 Argon2id、独立随机盐和带版本参数的编码，最低可用基线按 OWASP `m=19456 KiB, t=2, p=1`，按运行时资源实测并限频/限并发。登录用持久化不透明会话，库内存摘要，登录完成轮换会话；生产 cookie 使用 `Secure`、`HttpOnly`、明确的 `SameSite`，变更请求校验 CSRF。授权时同时核对用户状态、到期与撤销。停用、归档、密码重置和主动撤销在事务内使相关会话失效，账号允许状态和新会话创建在并发下保持一致。通过统一锁或约束避免停用最后一位完整管理员。[VERIFIED: docs/DeskRealm-System-Design-v0.1.md:44-49；docs/DeskRealm-PRD-v0.1.md:53-55] [CITED: OWASP 密码、会话与 CSRF 指南] [密码][owasp-password] · [会话][owasp-session] · [CSRF][owasp-csrf]
- 启用 TOTP 前要求验证首个验证码，再开启二步登录。用专用部署密钥及带版本的 AEAD 密文保护种子，令牌恢复纳入受控主机或管理员路径及审计。凭据行记录最后成功时间步，比较并更新在事务中完成，防止并发重放。OIDC 使用授权码 + PKCE S256，把 `state`、`nonce` 及回跳绑定一次性登录状态，服务端验证令牌签名、签发方、受众、时效和 nonce；唯一身份以 `issuer + subject` 绑定，不能以邮箱自动合并。账号生命周期及恢复统一产生审计。[VERIFIED: docs/DeskRealm-System-Design-v0.1.md:44-49,168-170] [CITED: RFC 6238、OIDC Core、RFC 9700] [TOTP][rfc6238] · [OIDC][oidc-core] · [OAuth 安全][oauth-bcp]

### 2. 组织、授权与持久状态

- 把用户、外部身份、成员关系、角色/动作、授权绑定、API Token、设备主组及通讯录共享分表，用外键、唯一约束和显式状态表达生命周期。为启用的 RustDesk ID 维护唯一约束与变更历史，审计和通讯录全程引用设备 UUID。管理授权在资源层统一求有效动作和范围，再下推至 PostgreSQL 对象查询、列表、导出和写入。跨组移动验证旧组与新组，委派不得超出授予人自己的管理动作与范围。控制角色和连接 ACL 使用独立模型，由 M2 在连接时执行。[VERIFIED: docs/DeskRealm-System-Design-v0.1.md:44-57,98-108,166-196；docs/DeskRealm-PRD-v0.1.md:26-36]
- API Token 为明确用途的高熵不透明凭据，库内存摘要，限制动作、范围及有效期，检查当前授予人状态与实际权限交集。浏览器会话和自动化 Token 使用各自的凭据入口。通讯录权限与关联设备权限同时筛选条目及敏感字段，独立授权查看、编辑、管理。API 对列表、按 ID、写入及导出使用同一授权语义，Vue 隐藏不可用操作以呈现服务端权限。[VERIFIED: docs/DeskRealm-System-Design-v0.1.md:48-49,104-108,181-196；docs/DeskRealm-UI-Design-v0.1.md:104-110]
- 入网凭据绑定组、用途、平台、期限及一次性消费。在验证设备持钥、绑定挑战与本次请求后，于同一事务消费凭据、创建或归并登记、保存审计和待投递记录。重复请求根据尝试 ID 与规范化负载返回同一结果，负载冲突返回错误，另一设备无权使用已有结果。审批和轮换锁定设备/审批行，旧凭据撤销，退役保留业务 ID。Go 接口与签名测试夹具验证流程，Windows 接入由后续阶段完成。[VERIFIED: docs/DeskRealm-System-Design-v0.1.md:51-57,156-164,168-179；.planning/phases/02-m1/02-CONTEXT.md:15-17,27-29]
- 在同一数据库事务写业务、追加审计和 outbox。唯一来源事件键去重，保留原始事件及收到时间，另存可查询的重复回执计数或记录。以实际异步动作/受信回执推进已确认状态，审计行保存可查询的当时对象归属以保留历史。查询和导出应用相同作用域及留存规则。任务基础使用稳定请求键、按对象步骤和版本校验，重试只挑失败项，释放或接管到期执行租约。M1 优先登记及导入可触发的任务，跨部署和发布编排按 FR-16 进入 M3。[VERIFIED: docs/DeskRealm-System-Design-v0.1.md:104-108,152-164,178-196；docs/DeskRealm-PRD-v0.1.md:70-80]

### 3. 建议的执行顺序

1. 本地 PostgreSQL、迁移、主机一次性初始化命令、Go 密码登录/注销、同源 Vue 首登/登录/概览和最小审计。真实数据库并发首登与 Playwright 登录证明完整链路。
2. 本地账号生命周期、邀请、经授权导入与恢复、TOTP/OIDC、会话即时撤销。再上线角色、作用域谓词、分组与 API Token，以及相应中文页面。
3. 设备身份、登记、审批、分组、轮换、禁用/退役、通讯录与共享。使用可重复的持钥证明和两个组的集成夹具。
4. 审计查询、导出和留存、回执状态、任务详情与按对象重试。贯通管理员、组管理员、运维、审计员的工作入口，同时核对跨阶段交接。

## Don't Hand-Roll

| 问题 | 采用 |
| --- | --- |
| PostgreSQL 事务和版本化迁移 | pgx + Goose，应用显式管理事务边界。[CITED: github.com/jackc/pgx、github.com/pressly/goose] |
| 密码 KDF、TOTP、OIDC 令牌与 PKCE | 使用上表标准库及对应 Go 包。应用保留 nonce、重放、恢复和身份映射规则。[CITED: pkg.go.dev/golang.org/x/crypto/argon2、rfc-editor.org/rfc/rfc6238.html、openid.net/specs/openid-connect-core-1_0.html] |
| 浏览器端构建与导航 | Vue/Vite/Router 与 vue-tsc，同一个 Go HTTP 入口服务实际页面及 API。[CITED: vuejs.org/guide/quick-start.html、router.vuejs.org/guide/] |

## Common Pitfalls

- 用一次 `SELECT` 判断空库后在事务外创建首位管理员。以预置控制行串行化初始化，唯一约束保证最终只创建一次。[CITED: postgresql.org/docs/18/explicit-locking.html]
- 通过邮箱匹配 OIDC、本地停用后继续接受旧登录令牌，或者允许同时间步的 TOTP 重放。检查绑定的签发方和 subject，并对认证决定使用实时用户状态与事务化验证码消耗。[CITED: openid.net/specs/openid-connect-core-1_0.html、rfc-editor.org/rfc/rfc6238.html]
- 只过滤列表，或导出和按 ID 更新使用不同的作用域；仅测试管理员看见成功结果。每个角色验证跨组、本人、组内和未登录行为。[VERIFIED: docs/DeskRealm-System-Design-v0.1.md:48-49,104-108,181-196]
- 审计在业务事务后异步追加，或把缺失/重复回执显示为执行成功。故障注入验证事务回滚及独立回执状态。[VERIFIED: .planning/phases/02-m1/02-CONTEXT.md:15-17]

## Code Examples

初始化采用固定单行状态，锁行后在同一事务检查、消费、创建和审计。以下 SQL 展示锁语义，表与字段名称由计划确定。[CITED: postgresql.org/docs/18/explicit-locking.html]

```sql
BEGIN;
SELECT initialized_at FROM bootstrap_state WHERE singleton = true FOR UPDATE;
-- Validate the ticket, write organization, user, role and audit, then mark initialized.
COMMIT;
```

## Validation Architecture

Nyquist 已启用。新建 Go 单元/集成测试及 Playwright 浏览器测试入口。集成库使用临时真实 PostgreSQL，进程使用固定时钟夹具与本地可控 OIDC 测试服务；浏览器测试通过构建后的 Go 同源入口。开发门禁在本机或 CI 正常运行，不要求 Linux 中心、中继或 Windows 环境验收。[VERIFIED: .planning/config.json:21-26；.planning/phases/02-m1/02-CONTEXT.md:6-8,27-29] [CITED: playwright.dev/docs/test-webserver]

| 需求 | 自动化通过条件 |
| --- | --- |
| FR-02 | 新库迁移与重跑；双请求首登仅一成功，过期、重放、回滚；密码/TOTP 登录、同时间步重复验证码、OIDC state/nonce/issuer/audience、最后管理员、停用及重置后旧会话失效，受控导入与恢复追溯。 |
| FR-03 | 平台管理员、组 A、组 B、本人和审计角色覆盖列表、对象、更新、范围切换、导出；API Token 发放、越权、过期与撤销。 |
| FR-04 | 限时凭据、持钥证明错误、并发重交与请求冲突、稳定 UUID、主组转换、审批、轮换和禁用；登记、在线和策略回执独立。 |
| FR-07 | 私有与共享册的查看、增删、管理和撤回，设备条目用 UUID，分享权限与设备权限分别检验。 |
| FR-13 | 回滚时域变更与审计一起撤销，重复与缺失回执可辨，时间/会话 ID/范围的检索导出一致，留存按配置验证。 |
| FR-18 | 中文初始化→登录→概览，授权角色完成审批、通讯录、审计与失败项重试；无权、空态、加载及表单反馈均由真实请求驱动。 |

计划新增的运行入口建议为后端 `go test ./...`，数据库集成显式测试目标，前端 `npm run typecheck`、`npm run build` 与固定浏览器测试脚本。任务级执行涉及的测试，阶段级执行完整数据库与浏览器链路。当前本机已核对 Go `1.27.4`、Node `26.13.1`、npm `11.20.0`、Compose `v5.5.0`；未安装所选依赖或启动 M1 服务。[VERIFIED: 本地 CLI]

## Security Domain

ASVS 5.0 适用 V2 输入、V6 认证、V7 会话、V8 授权、V11 密码学、V13 配置、V14 数据保护和 V16 日志。按初始化、账号状态、凭据、越权查询与日志各自的失败分支核对。密码、TOTP 种子、OIDC Secret、初始化链接、入网凭据及 API Token 需避开业务日志和浏览器持久存储。授权规则和审计状态用真实回归覆盖。[VERIFIED: .planning/config.json:50-52，原文 `"security_enforcement": true`；docs/DeskRealm-System-Design-v0.1.md:44-57,162-179] [CITED: OWASP ASVS 5.0] [ASVS][asvs]

## Assumptions Log

依赖 peer 范围已核对。首次实现应以锁文件安装、Go 编译、前端类型检查及集成测试验证整组版本。[ASSUMED] 环境没有固定的组织 OIDC 提供方，先以本地测试服务测协议，实际组织 IdP 与密钥来源按部署配置确认。

[go-release]: https://go.dev/doc/devel/release#go1.27.4
[pg-versions]: https://www.postgresql.org/support/versioning/
[pg-image]: https://github.com/docker-library/postgres/blob/31f4af9d429410ba4ad1f6e394daecd9ae7edfa7/18/bookworm/Dockerfile#L197-L200
[pgx]: https://github.com/jackc/pgx/tree/v5.13.0
[goose]: https://github.com/pressly/goose/tree/v3.30.1
[argon2]: https://pkg.go.dev/golang.org/x/crypto@v0.59.0/argon2
[totp-lib]: https://github.com/pquerna/otp/tree/v1.5.0
[oidc-lib]: https://pkg.go.dev/github.com/coreos/go-oidc/v3@v3.21.0/oidc
[oauth-lib]: https://pkg.go.dev/golang.org/x/oauth2@v0.38.0
[vue-repo]: https://github.com/vuejs/core
[router-repo]: https://github.com/vuejs/router
[vite-repo]: https://github.com/vitejs/vite
[vite-vue-repo]: https://github.com/vitejs/vite-plugin-vue
[vue-tsc-repo]: https://github.com/vuejs/language-tools
[ts]: https://www.typescriptlang.org/docs/handbook/release-notes/typescript-6-0.html
[pw-repo]: https://github.com/microsoft/playwright
[owasp-password]: https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html
[owasp-session]: https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html
[owasp-csrf]: https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html
[rfc6238]: https://www.rfc-editor.org/rfc/rfc6238.html
[oidc-core]: https://openid.net/specs/openid-connect-core-1_0.html
[oauth-bcp]: https://www.rfc-editor.org/rfc/rfc9700.html
[asvs]: https://asvs.owasp.org/
