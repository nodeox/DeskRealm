---
gsd_state_version: "1.0"
current_phase: 4
current_phase_name: M3 部署与自动化交付
status: executing
last_updated: "2026-11-24T12:04:46Z"
last_activity: 2026-11-24
last_activity_desc: M3 04-05 双架构 Authenticode 安装器接通开发签名与独立核验
state_head: 1f0b09286ffe6dc335de2706eabbb324bf20b29a
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
Status: In progress，双架构开发安装器 Authenticode 签名、内容绑定和证书核验通过。接续含启动程序的原生 x64 工件及中心安装器发布。
Last activity: 2026-11-24 — 安装器拒绝 SHA-1 签名和同受信 CA 中未批准的发布证书。[04-05 验证](phases/04-m3/04-VALIDATION.md#04-05-authenticode-开发安装器)

## Accumulated Context

M0–M3 开发完成后进入目标环境验收。先 Linux 中心和一台中继，再双 Windows 终端。开发阶段执行本地构建及自动化检查。

## Session Continuity

获取当前阶段计划和证据时使用本仓库的 `.planning/phases/`。

M0、M1、M2 已完成开发验证。M2 交接包括账号停用后的续租拒绝、通讯录连接 ACL、实际会话回执和 Windows 受管入口。

[04-04 小结](phases/04-m3/04-04-SUMMARY.md) 记录双端探测、短期票据、实际转发、断线后新授权及会话详情。04-05 已接通 ZIP 分组发布、单设备凭据、首次启动程序和安装器开发签名。新一轮含启动程序的 CI 中 ARM64 原生构建通过，x64 仍在运行。后续将安装器签名证据及分组下载接入中心，并接通组织部署工具的凭据交付。[Windows 原生构建](https://github.com/nodeox/DeskRealm/actions/runs/45380636466)
