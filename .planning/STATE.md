---
gsd_state_version: "1.0"
current_phase: 4
current_phase_name: M3 部署与自动化交付
status: executing
last_updated: "2026-11-24T10:27:38Z"
last_activity: 2026-11-24
last_activity_desc: M3 04-05 双架构开发 ZIP 分组发布、下载和撤回通过
state_head: 789a51eb463842ad833deabf29d03ec17d37ffa7
progress:
  total_phases: 5
  completed_phases: 3
  total_plans: 17
  completed_plans: 15
  percent: 60
---

# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-11-22)

**Core value:** 从自托管部署到 Windows 纳管，提供可撤销且可审计的授权连接。
**Current focus:** Phase 4，M3 部署与自动化交付。

## Current Position

Phase: 4 — M3 部署与自动化交付
Plan: [04-05](phases/04-m3/04-05-PLAN.md) Task 1–2
Status: In progress，开发 ZIP 的验签、分组发布和下载撤回通过。接续当前原生工件、安装器与免填登记。
Last activity: 2026-11-24 — 中心将通过复验的两架构归档复制到独立持久目录，按获准设备组提供下载。[04-05 验证](phases/04-m3/04-VALIDATION.md#04-05-开发包分组发布)

## Accumulated Context

M0–M3 开发完成后进入目标环境验收。先 Linux 中心和一台中继，再双 Windows 终端。开发阶段执行本地构建及自动化检查。

## Session Continuity

获取当前阶段计划和证据时使用本仓库的 `.planning/phases/`。

M0、M1、M2 已完成开发验证。M2 交接包括账号停用后的续租拒绝、通讯录连接 ACL、实际会话回执和 Windows 受管入口。

[04-04 小结](phases/04-m3/04-04-SUMMARY.md) 记录双端探测、短期票据、实际转发、断线后新授权及会话详情。04-05 已接通中心验签、开发包分组发布和撤回。后续核验本轮 CI 工件，并接入签名安装器、部署工具凭据、客户端公开配置读取和免填登记。[Windows 原生构建](https://github.com/nodeox/DeskRealm/actions/runs/45368970255)
