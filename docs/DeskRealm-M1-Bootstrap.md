# DeskRealm M1 首次初始化与登录

当前可运行的控制面包括 PostgreSQL 迁移、一次性首管初始化、本地账号登录、中文概览、服务端注销和成员管理。初始化、成功登录、注销和成员状态变更均进入审计。后续计划接入邀请、恢复、身份源、作用域授权、设备和任务。

## 本地开发检查

需要 Go 1.26 或更新版本、Node 26、Docker 和 Python 3。在仓库根目录执行。

```sh
bash scripts/deskrealm-m1-test bootstrap-api
bash scripts/deskrealm-m1-test user-status
npm --prefix web ci --ignore-scripts
npm --prefix web run typecheck
PLAYWRIGHT_SKIP_BROWSER_GC=1 npm --prefix web exec -- playwright install chromium
bash scripts/deskrealm-m1-test bootstrap-browser
```

两个测试入口使用独立 PostgreSQL 容器和本机临时端口，退出时清理。目标环境的 Linux 中心、中继和双 Windows 验收在首版开发完成后的 Phase 5 执行。

## 在开发数据库上运行

`DESKREALM_DATABASE_URL` 使用开发 PostgreSQL 连接串。迁移专用连接可通过 `DESKREALM_MIGRATION_DATABASE_URL` 指定。

```sh
npm --prefix web run build
export DESKREALM_DEV_HTTP=1
export DESKREALM_PUBLIC_ORIGIN=http://127.0.0.1:8080
export DESKREALM_HTTP_ADDR=127.0.0.1:8080
export DESKREALM_WEB_DIR="$PWD/web/dist"
go run ./cmd/deskrealm migrate
go run ./cmd/deskrealm bootstrap-ticket
go run ./cmd/deskrealm serve
```

打开票据命令打印的链接，设置组织、账号和至少 12 字符的密码。链接有效期为 15 分钟。再签发会替换前一次票据，完成初始化后停止签发。随后用新账号登录并在概览查看当前组织和账号。

正式入口使用 HTTPS。将中心外部 HTTPS origin 配给 `DESKREALM_PUBLIC_ORIGIN`，用反代连接 Go 的内部监听地址。HTTP 开发模式仅绑定本机回环。浏览器请求使用同源接口，注销需要当前会话的 CSRF 值。

| 接口 | 返回 |
| --- | --- |
| `GET /v1/bootstrap/status` | 初始化状态 |
| `POST /v1/bootstrap` | 成功创建组织和管理员后返回 201 |
| `POST /v1/auth/login` | 成功创建不透明会话后返回 204 |
| `GET /v1/auth/session` | 当前账号、组织、角色、会话绑定的 CSRF 值 |
| `POST /v1/auth/logout` | 撤销会话并清理 Cookie 后返回 204 |
| `GET /v1/overview` | 当前组织及管理能力 |
| `GET /v1/admin/users` | 平台管理员可分页查看账号、角色和状态 |
| `POST /v1/admin/users` | 创建成员账号，需当前会话的 CSRF 值 |
| `POST /v1/admin/users/{id}/{enable,disable,archive}` | 记录原因并变更状态。停用或归档会撤销现有登录会话 |
