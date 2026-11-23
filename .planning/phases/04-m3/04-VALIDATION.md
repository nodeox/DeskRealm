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

接续 04-01 的 Compose 生成、镜像准备、中心持久身份、迁移、HTTPS 验活及 Chromium 首管登录。实际部署、中继和双 Windows 仍按 Phase 5 验收。
