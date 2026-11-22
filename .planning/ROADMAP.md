# Roadmap: 端域 DeskRealm

## Overview

按设计文档 M0–M3 完成首版开发，随后执行目标环境验收。设计文档的 M4 扩展暂列后续。阶段内执行本地自动化检查，目标环境仍由最终验收阶段记录真实证据。

## Phases

- [x] **Phase 1: M0 安装与传输基线** - 交付可配置、可重复运行且保留中心身份的 OSS 试点部署。 (completed 2026-11-22)
- [ ] **Phase 2: M1 用户、设备与管理面** - 管理员通过中文控制台初始化、管理和委派组织资源。
- [ ] **Phase 3: M2 受管连接与策略** - 受管 Windows 终端对直连和中继执行同一准入、权限、租约及策略授权。
- [ ] **Phase 4: M3 部署与自动化交付** - 管理员可部署中心和双区域中继，签名发布、免填纳管设备并管理升级恢复。
- [ ] **Phase 5: 首版目标环境验收** - 开发完成后核对 Linux 中心、区域中继、双 Windows 和首版发布场景的真实证据。

## Phase Details

### Phase 1: M0 安装与传输基线

**Goal**: 交付可配置、可重复运行且保留中心身份的 OSS 试点部署。
**Depends on**: Nothing
**Requirements**: BASE-01, BASE-02, BASE-03
**Success Criteria**:
1. 配置验证覆盖中心与中继分开安装、端口和镜像引用。
2. 安装入口支持配置检查、启动及状态输出，并保持状态目录。
3. 交付具体、可复跑的实机验收步骤和证据记录字段。
**Plans**: 1/1 plans complete
- [x] 01-01-PLAN.md

### Phase 2: M1 用户、设备与管理面

**Goal**: 管理员通过中文控制台初始化、管理和委派组织资源。
**Depends on**: Phase 1
**Requirements**: FR-02, FR-03, FR-04, FR-07, FR-13, FR-18
**Success Criteria**:
1. 首个管理员可初始化并登录。授权者可创建用户、委派角色和审批设备。
2. 用户只看到获准的组范围。API 单对象、列表与控制台执行一致的授权。
3. 设备历史、审批、通讯录共享和操作审计可追溯。
**Plans**: 2/4 plans executed

Plans:
**Wave 1**
- [x] 02-01: 首次初始化、本地会话和中文概览

**Wave 2** *(blocked on Wave 1 completion)*
- [x] 02-02: 账号生命周期、身份源与作用域授权

**Wave 3** *(blocked on Wave 2 completion)*
- [ ] 02-03: 设备登记审批与通讯录

**Wave 4** *(blocked on Wave 3 completion)*
- [ ] 02-04: 审计、任务和角色入口

### Phase 3: M2 受管连接与策略

**Goal**: 受管 Windows 终端对直连和中继执行同一准入、权限、租约及策略授权。
**Depends on**: Phase 2
**Requirements**: FR-08, FR-09, FR-10, FR-11, FR-12, NFR-01
**Success Criteria**:
1. 直连和中继路径都要求已认证用户、设备及有效授权。受控端核销一次性授权。
2. 受控端按操作范围及确认执行会话，撤销或租约到期后停止控制。
3. 可查策略版本、应用失败和撤销状态，能将状态与端点事件关联。
**Plans**: TBD

### Phase 4: M3 部署与自动化交付

**Goal**: 管理员可部署中心和双区域中继，签名发布、免填纳管设备并管理升级恢复。
**Depends on**: Phase 3
**Requirements**: FR-01, FR-05, FR-06, FR-14, FR-15, FR-16, FR-17, FR-19, NFR-02, NFR-04, NFR-05
**Success Criteria**:
1. 管理员可从空机开始安装，重跑可复用身份及数据库，并从任务记录查实际进度。
2. 两区候选由中心筛选，使用双端 RTT 选点并记录回退和故障原因。
3. 校验过的签名包按组发布。设备连接、登记、审批及实际策略回执可查。
4. 可重试失败项，验证升级暂停、回滚与备份恢复流程。
**Plans**: TBD

### Phase 5: 首版目标环境验收

**Goal**: 开发完成后核对 Linux 中心、区域中继、双 Windows 和首版发布场景的真实证据。
**Depends on**: Phase 4
**Requirements**: NFR-03, NFR-06, QUAL-01, QUAL-02
**Success Criteria**:
1. 首版开发完成后才启动目标环境验收。每条路径的记录有候选版本、拓扑和实际证据。
2. Linux 单中心单中继通过后，两台 Windows 完成要求的四项验证。
3. 受管、多中继、签名和迁移分别达到首版验收条件，支持范围由实测决定。
**Plans**: TBD

## Progress

| Phase | Plans Complete | Status | Completed |
| --- | --- | --- | --- |
| 1. M0 安装与传输基线 | 1/1 | Complete    | 2026-11-22 |
| 2. M1 用户、设备与管理面 | 2/4 | In Progress | - |
| 3. M2 受管连接与策略 | 0/TBD | Not started | - |
| 4. M3 部署与自动化交付 | 0/TBD | Not started | - |
| 5. 首版目标环境验收 | 0/TBD | Not started | - |
