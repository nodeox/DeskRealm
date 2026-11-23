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

本次隔离环境仅启动 PostgreSQL。04-01 继续准备可运行的中心及会合镜像、执行实际中心迁移、HTTPS 验活及 Chromium 首管登录。实际 Linux 中心、中继和双 Windows 仍按 Phase 5 验收。
