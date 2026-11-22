<!-- GSD:project-start source:PROJECT.md -->

## Project

**端域 DeskRealm**

端域是面向企业和团队的自托管远程桌面与终端管理平台。管理员通过中文 Web 控制台部署中心和区域中继、管理身份与设备、签名发布 Windows 客户端，并查看会话、任务和审计结果。运维人员从获授权的设备列表或通讯录连接 Windows 终端，受控端执行访问确认、会话权限和撤销。

**Core Value:** 管理员能在自有基础设施上完成从部署、签名发布到终端纳管的流程，运维人员能安全连接获授权的设备，管理员能撤销会话并追溯结果。

### Constraints

- **部署与技术栈。** Linux Docker Compose、Go 控制面、PostgreSQL、Vue 与 TypeScript 中文控制台。传输与会合沿用 RustDesk OSS，Windows 为首版受管主控和受控平台。支持的架构以验收结果为准。
- **身份和权限。** 管理授权、连接准入、受控端操作权限各有执行点。默认拒绝未获授的连接。直接路径和中继路径都要求受控端确认有效授权并按租约约束操作。
- **发布安全。** 公共安装包包含经验证的中心连接配置。设备入网凭据按部署对象单独发放。签名和摘要校验通过后才能向获授权组发布，并记录源码、模板与产物来源。
- **恢复。** 重试和重建保留中心身份与业务状态。任务可按对象恢复，重复提交和回执可去重。
- **验收。** 开发阶段使用构建、自动化测试和可复跑检查。Linux 中心、中继和双 Windows 的目标环境验收集中在首版开发完成后。在线撤销回执 5 秒及失联停止控制 35 秒保留为文档中的真机验收目标。

<!-- GSD:project-end -->

<!-- GSD:stack-start source:STACK.md -->

## Technology Stack

Technology stack not yet documented. Will populate after codebase mapping or first phase.
<!-- GSD:stack-end -->

<!-- GSD:conventions-start source:CONVENTIONS.md -->

## Conventions

Conventions not yet established. Will populate as patterns emerge during development.
<!-- GSD:conventions-end -->

<!-- GSD:architecture-start source:ARCHITECTURE.md -->

## Architecture

Architecture not yet mapped. Follow existing patterns found in the codebase.
<!-- GSD:architecture-end -->

<!-- GSD:skills-start source:skills/ -->

## Project Skills

No project skills found. Add skills to any of: `.claude/skills/`, `.agents/skills/`, `.cursor/skills/`, `.github/skills/`, or `.codex/skills/` with a `SKILL.md` index file.
<!-- GSD:skills-end -->

<!-- GSD:workflow-start source:GSD defaults -->

## GSD Workflow Enforcement

Before using Edit, Write, or other file-changing tools, start work through a GSD command so planning artifacts and execution context stay in sync.

Use these entry points:
- `$gsd-fast` for a trivial task inline, with no subagents and no PLAN.md
- `$gsd-quick` for small fixes, doc updates, and ad-hoc tasks
- `$gsd-debug` for investigation and bug fixing
- `$gsd-execute-phase` for planned phase work

Do not make direct repo edits outside a GSD workflow unless the user explicitly asks to bypass it.
<!-- GSD:workflow-end -->

<!-- GSD:profile-start -->

## Developer Profile

> Profile not yet configured. Run `$gsd-profile-user` to generate your developer profile.
> This section is managed by `generate-claude-profile` -- do not edit manually.
<!-- GSD:profile-end -->
