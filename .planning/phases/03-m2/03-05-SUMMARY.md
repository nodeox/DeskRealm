---
phase: 03-m2
plan: "05"
status: complete
completed: "2026-11-23"
source_commit: 302c434f1ae7ab6daad984175cdcc7c6c884c46f
---

# 03-05 中文策略、会话工作台与全量开发门禁

| 工作 | 提交 | 验证 |
| --- | --- | --- |
| 中文策略和会话工作台 | `24c0586` | 真实控制面验证规则、灰度回滚、设备回执、撤销、会话审计、四类授权和窄屏键盘操作。 |
| 全量开发门禁 | `302c434` | M0/M1、受管数据库、跨语言协议、隔离端点、Chromium、Windows 双架构工件六项通过。 |

Windows 工件绑定原构建提交 `f488f2b`。运行时逐项比对原生输入，在产生工件的提交下校验两套构建和测试证据。 [Windows 构建与工件](https://github.com/nodeox/DeskRealm/actions/runs/45168532930)

[运行手册](../../../docs/DeskRealm-M2-Runbook.md) 包含全量复跑、离线 Cargo 缓存、Windows 工件和现场证据交接。[验证记录](03-VALIDATION.md) 保留实际路径和会话 ID。[阶段核对](03-VERIFICATION.md) 记录 Windows 入口的功能缺口。
