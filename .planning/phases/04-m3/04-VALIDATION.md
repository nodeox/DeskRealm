---
phase: 04-m3
status: in_progress
---

# M3 开发验证

## 04-01 中心安装预检

候选 `73caf84e3985cfe22f5b14f3042bbd4107b5d8f9` 于 2026-11-23 验证。

| 检查 | 结果 |
| --- | --- |
| `bash scripts/deskrealm-m3-test install-preflight` | 5 项具名 Go 竞态用例通过，覆盖域名和端口归一化、严格 JSON、镜像摘要表示、HTTPS 证书、Docker 上下文和 TCP/UDP 端口冲突。 |
| 实际 CLI 预检 | macOS ARM64 开发机检查 Docker Engine 29.8.2 Linux ARM64 及 Compose 5.5.0。使用临时配置，四个 TCP/UDP 绑定通过，人工占用会合 UDP 测试端口时复跑报冲突。部署目录在预检后仍不存在。 |
| 构建及基础回归 | Linux amd64、arm64 静态交叉编译成功。安装器 `go vet` 和全仓 `go test -count=1 ./...` 通过。 |

## 04-01 中心身份和 Compose 准备

候选 `2881b2ac4bcb813aa273348311ca99051089cc7c` 于 2026-11-23 验证。

| 检查 | 结果 |
| --- | --- |
| `bash scripts/deskrealm-m3-test install-prepare` | 4 项具名 Go 竞态用例通过，覆盖复用部署身份、并发准备、拒绝不完整部署及证书模式。 |
| 实际 Docker Compose 解析 | Compose 5.5.0 解析四项服务。确认 PostgreSQL 使用私有网络且无主机监听，外部反代模式的 API 仅发布到回环地址，TCP/UDP 会合端口符合配置。输出未展开凭据。 |
| 隔离 PostgreSQL 18.7 初始化和重建 | 迁移和运行账号均为非超级用户。迁移账号创建测试表和序列，运行账号可读写记录且无法向业务 schema 创建表。强制重建数据库容器后保留原记录。复跑 `prepare` 后部署 ID、受管签名公钥和会合公钥保持一致。 |
| 基础回归 | 全仓 `go test -count=1 ./...`、中心和安装器 `go vet`、预检套件及 `git diff --check` 通过。 |

## 04-01 固定摘要镜像与隔离首管联调

2026-11-23，在 `b301c57b39b326d260f66f6a5b5434c921fc39cb` 运行 `bash scripts/deskrealm-m3-test install-bootstrap`，退出 0。Linux ARM64 Docker 29.8.2 与 Compose 5.5.0 使用回环 Registry 和隔离 Compose 项目。

| 检查 | 结果 |
| --- | --- |
| 中心与会合镜像 | 静态 Linux Go 二进制与生产 Vue 文件进入中心镜像。会合镜像使用固定上游及补丁通过 `cargo build --locked` 构建的 `hbbs`。中心镜像摘要为 `sha256:38a8821eb5ccd17e02a84219d54a9ccf3b852f9ae1bec4b1ec389c757b3ea98a`，会合镜像为 `sha256:dcbc870229b4d5d67d4fd093175301cb13607e802ea5655c5268c1be050e1981`。 |
| 实际启动与验活 | PostgreSQL 18.7 就绪后执行一次性迁移，再启动中心和受管会合。HTTPS 状态 API 通过测试 CA 校验。会合 TCP 入口返回实际 RustDesk NAT 探测响应，检查端口可达，容器内可解析中心域名。启动后复跑 `prepare` 返回同一部署 ID 和两套公钥。 |
| Chromium 首管 | 通过主机执行中心的 `bootstrap-ticket` 生成一次性链接。Chromium 使用测试叶证书公钥指纹访问临时域名，完成初始化及管理员登录并得到 Secure、HttpOnly 会话 Cookie；随后 API 确认已初始化。 |
| 回归 | M3 预检和准备竞态套件、全仓 Go 测试及 vet、Vue 类型检查和原有 M1 Chromium 初始化场景通过。 |

日志在本地 Git 目录下的 `deskrealm-evidence/m3/b301c57b39b326d260f66f6a5b5434c921fc39cb/install-bootstrap.log`，SHA-256 为 `bcefe9453e5b2dac1febff0cf1565977bc8b12d3fd011016581205e3dbc5a68f`。

04-01 继续把启动、状态核对和首管票据接入安装器命令，验证中断重跑与持久数据保护。实际 Linux 中心、中继和双 Windows 仍按 Phase 5 验收。
