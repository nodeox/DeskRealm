---
phase: 04-m3
plan: "01"
status: complete
completed: "2026-11-23"
source_commit: 4caf823ab5e3e3a9750f2fefa530f09d553bd86c
---

# 04-01 中心安装、验活及中断重跑

| 工作 | 开发验证 |
| --- | --- |
| 中心首装 | 固定摘要启动 PostgreSQL、迁移、Go 中心和受管会合。HTTPS 核对中心身份，Chromium 完成首管初始化和登录。 |
| 空机与离线 | Ubuntu 24.04 和 Debian 12 的 ARM64 与模拟 amd64 通过 Docker 首装和重跑；Linux ARM64 验证签名离线镜像导入及移除介质后的重建。 |
| 可恢复步骤 | 中断后阶段、任务 ID、原 PostgreSQL 集群和两套公钥保持可核对；缺失记录或数据冲突会阻止启动。 |
| 已有默认中继 | 中心和会合容器配置核对、独立受管 `hbbr` 的 TCP 探测及停机恢复通过。 |

04-02 使用 `deskrealm-install install-record` 输出的版本化脱敏记录导入任务中心。目标环境的 Linux 中心、中继及双 Windows 端到端验收依序在 Phase 5 进行。[开发验证](04-VALIDATION.md) · [安装说明](../../../docs/DeskRealm-M3-Install.md)
