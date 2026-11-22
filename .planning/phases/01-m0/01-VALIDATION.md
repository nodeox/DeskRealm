---
phase: "1"
slug: "m0"
status: validated
nyquist_compliant: true
wave_0_complete: true
created: "2026-11-22"
---

# Phase 1 — Validation Strategy

## Test Infrastructure

Go 标准库测试使用临时目录和替身 Docker，检查安装和恢复分支。真实 Compose 只运行 `config`，检查生成的中心和中继模型。

## Sampling Rate

- 每项代码改动后执行适用的快速测试。
- 阶段收口运行 `go test ./... -count=1 -v`、`go test -race ./... -count=1`、`go vet ./...` 和 Linux amd64/arm64 交叉编译，均已通过。

## Per-Task Verification Map

| Requirement | Automated behavior | Target environment |
| --- | --- | --- |
| BASE-01 | `TestInitAndComposeConfig` 已通过。使用本机 Compose 静态解析两种部署 | Phase 5 核对 Linux 架构、端口和有效中继配置 |
| BASE-02 | `TestLifecycle` 已通过。替身 Docker 覆盖运行配置、重跑、重建与失败 | Phase 5 重建后以旧 Key 重连，核对同一挂载及公钥 |
| BASE-03 | `TestM0RunbookContract` 已通过。检查固定版本、参数、顺序和证据字段 | Phase 5 双 Windows 注册、直连、强制中继与重建重连 |

## Manual-Only Verifications

上述 Phase 5 步骤都需目标主机和客户端证据。按用户安排在 M0–M3 开发后执行。

## Validation Sign-Off

2026-11-22 完成开发阶段检查。目标环境验收仍待 Phase 5。
