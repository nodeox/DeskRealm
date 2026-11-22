---
phase: 01-m0
verified: "2026-11-22"
status: passed
score: 3/3
source_commit: 63f4d33
---

# M0 开发验证

| Requirement | Evidence |
| --- | --- |
| BASE-01 | `TestInitAndComposeConfig` 在 Docker Compose v5.5.0 实际解析两个独立角色。测试核对镜像、原生端口、命令、中心持久挂载及中继公开身份。 |
| BASE-02 | `TestLifecycle` 用替身检查实际配置、启动、原身份重建、Docker 失败、停止状态和 SQLite 丢失防护。 |
| BASE-03 | `TestM0RunbookContract` 检查四个取证场景顺序、固定版本和操作命令。手册证据条件与 Phase 5 一致。 |

`go test ./... -count=1 -v`、`go test -race ./... -count=1`、`go vet ./...`、Linux amd64/arm64 交叉编译及 `go run ./cmd/deskrealm-m0 --help` 均已在开发机通过。

目标 Linux、独立中继和双 Windows 的联网与重建取证按 D-03 和 QUAL-01 留待 Phase 5。报告由当前会话写入，阶段验证子任务退出前未生成报告。
