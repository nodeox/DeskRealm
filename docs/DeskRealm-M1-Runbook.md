# DeskRealm M1 中文控制台开发运行手册

## 完整开发检查

在仓库根目录使用 Go 1.26、Node 26、Docker、Python 3 和本机 Chromium。

```sh
npm --prefix web ci --ignore-scripts
npm --prefix web exec -- playwright install chromium
bash scripts/deskrealm-m1-test all
```

`all` 核对 Go 模块和已安装的 npm 依赖树，执行 `go vet`、完整竞态测试、两个 Go 命令构建、Vue 类型检查和生产构建。数据库测试使用固定摘要的 PostgreSQL 18.7 镜像。三条浏览器回归各自使用临时数据库和回环端口，退出时清理。脚本要求命名的 Go 与 Chromium 用例均存在并通过，跳过、空跑和失败都会返回非零状态。

```sh
bash scripts/deskrealm-m1-test bootstrap-browser
bash scripts/deskrealm-m1-test resources-browser
bash scripts/deskrealm-m1-test roles-browser
bash scripts/deskrealm-m1-test overview
```

首登回归验证初始化、账号和身份操作。资源回归验证设备、通讯录、审计及失败对象重试。四角色回归使用真实用户、组和授权，核对直达页面、只读与越权边界、设备审批、审计导出、窄屏和键盘焦点。`overview` 通过数据库夹具核对在线计数随作用域及撤权变化。

## 配置和启动

使用隔离的开发 PostgreSQL。由当前 shell 设置 `DESKREALM_DATABASE_URL`，有独立迁移账号时设置 `DESKREALM_MIGRATION_DATABASE_URL`。浏览器回归会把这两个值指向本次临时库。测试的 OIDC 来自 Go 测试夹具，手工运行时按提供方配置 issuer、客户端和回调。

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

保持 `serve` 运行，再打开票据命令生成的本机链接。已有初始化账号时直接登录。通过「概览」查看当前授权范围内的资源、待办和近期记录。待审批设备和可重试对象链接会打开对应范围的列表。UI 的数字来自服务端，批准、重试或授权变更后可刷新。`serve` 自带导入重试工作者。

外部部署入口使用 HTTPS，并由受控运行环境管理数据库连接、TOTP 密钥和 OIDC 客户端凭据。完整接口和配置说明见 [M1 初始化与登录](DeskRealm-M1-Bootstrap.md)。

## 迁移与回滚验证

```sh
bash scripts/deskrealm-m1-test bootstrap-api
bash scripts/deskrealm-m1-test tasks
```

`bootstrap-api` 验证同一数据库重复执行迁移，以及故意失败的迁移在事务中回滚。`tasks` 在隔离测试 schema 中回退到版本 9 后升至版本 10，确认保留旧导入结果、设备最初的 RustDesk ID 和已保存审批。回退 00010 会丢弃任务及尝试台账。需要验证部署回退时，先用独立备份复本核对数据、迁移版本和恢复路径。

## 记录证据

记录 `git rev-parse HEAD`、工作树状态、Go 和 Node 版本、Docker 版本、完整检查命令及退出状态。将脱敏后的测试日志和必要截图保存在组织批准的证据位置，并注明时间、角色、作用域和预期结果。保护初始化及入网票据、数据库连接串、密码、令牌、签名材料和原始审计导出。留存 CSV 示例时使用独立测试组织及虚构账号。

## 后续集成

M1 提供设备登记、通讯录解析、角色管理、工作台、范围审计及任务能力。M2 在连接入口复用 `ResolveAddressBookDevice`，接入连接 ACL、停用后的续租拒绝、实际终端回执和会话计数。在线设备数使用当前控制面登记的连接状态，真实在线观测随数据面接入。中继健康、客户端分发和升级随 M3 接入。

等 M0 至 M3 开发完成后，按 [M0 实机手册](DeskRealm-M0-Runbook.md) 先核对 Linux 中心及一台中继，再用两台 Windows 验证注册、直连、强制中继及服务重建后保留原公钥。该验收记录在 Phase 5。
