---
phase: "1"
slug: "m0"
status: draft
nyquist_compliant: false
wave_0_complete: false
created: "2026-11-22"
---

# Phase 1 — Validation Strategy

## Test Infrastructure

Go 标准库测试使用临时目录和替身 Docker，检查安装和恢复分支。真实 Compose 只运行 `config`，检查生成的中心和中继模型。

## Sampling Rate

- 每项代码改动后执行适用的快速测试。
- 阶段收口运行 `go test ./...`、`go vet ./...` 和 Linux amd64/arm64 交叉编译。

## Per-Task Verification Map

| Requirement | Automated behavior | Target environment |
| --- | --- | --- |
| BASE-01 | 模型解析、固定版本和摘要、正确的角色端口、非法参数和中继密钥被拒绝 | Phase 5 核对 Linux 架构、端口和有效中继配置 |
| BASE-02 | 安装重跑、中心指纹一致、持久数据保留、缺失或错配的身份失败、Docker 错误传播 | Phase 5 重建后以旧 Key 重连，核对同一挂载及公钥 |
| BASE-03 | 对照命令和版本检查复跑手册中的参数、顺序和证据字段 | Phase 5 双 Windows 注册、直连、强制中继与重建重连 |

## Manual-Only Verifications

上述 Phase 5 步骤都需目标主机和客户端证据。按用户安排在 M0–M3 开发后执行。

## Validation Sign-Off

待执行开发阶段的自动化检查，记录与目标环境验收分开。
