# Phase 1: M0 安装与传输基线 — Context

**Gathered:** 2026-11-22
**Status:** Ready for planning

## Phase Boundary

交付 RustDesk OSS 原版参考拓扑的 Linux 安装原型和复跑说明。范围为一个中心与一个可独立安装的中继。后续阶段添加管理平面、受控端强制授权和自动化交付。

## Decisions

- **D-01:** 系统设计锁定 RustDesk Server OSS 1.1.16 与原版客户端 1.5.3，部署引用固定多架构镜像摘要。
- **D-02:** 中心的 RustDesk 身份、状态以及可复跑的拓扑配置持久化。启动、重跑和正常服务重建沿用同一中心公钥。
- **D-03:** 用户要求 M0–M3 首版开发完成后再做目标环境验收。验收先 Linux 中心和单中继，再用两台 Windows 终端验证注册、直连、强制中继和重建后仍以原公钥连接。开发阶段运行静态、构建和自动化检查。
- **D-04:** M0 安装原型使用既有 Docker Compose 运行环境。空机运行时安装、Web 首个管理员、终端强制管控与双区域自动选点按路线图实施。

## Canonical References

- `docs/DeskRealm-PRD-v0.1.md` 第 7、8 节
- `docs/DeskRealm-System-Design-v0.1.md` 第 1、2、7、10、11 节
- `.planning/PROJECT.md` 及 `.planning/REQUIREMENTS.md`（BASE-01、BASE-02、BASE-03、QUAL-01）

## Deferred Ideas

目标环境的网络可达性、实际路径与 Windows 连接成功记录均在 Phase 5 采集。
