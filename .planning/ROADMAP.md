# Roadmap: 端域 DeskRealm

## Overview

按设计文档 M0–M3 完成首版开发，随后执行目标环境验收。设计文档的 M4 扩展暂列后续。阶段内执行本地自动化检查，目标环境仍由最终验收阶段记录真实证据。

## Phases

- [x] **Phase 1: M0 安装与传输基线** - 交付可配置、可重复运行且保留中心身份的 OSS 试点部署。 (completed 2026-11-22)
- [x] **Phase 2: M1 用户、设备与管理面** - 管理员通过中文控制台初始化、管理和委派组织资源。 (completed 2026-11-22)
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
**Plans**: 4/4 plans complete

Plans:
**Wave 1**
- [x] 02-01: 首次初始化、本地会话和中文概览

**Wave 2** *(blocked on Wave 1 completion)*
- [x] 02-02: 账号生命周期、身份源与作用域授权

**Wave 3** *(blocked on Wave 2 completion)*
- [x] 02-03: 设备登记审批与通讯录

**Wave 4** *(blocked on Wave 3 completion)*
- [x] 02-04: 审计、任务和角色入口

### Phase 3: M2 受管连接与策略

**Goal**: 受管 Windows 终端对直连和中继执行同一准入、权限、租约及策略授权。
**Depends on**: Phase 2
**Requirements**: FR-08, FR-09, FR-10, FR-11, FR-12, NFR-01
**Success Criteria**:
1. 直连和中继路径都要求已认证用户、设备及有效授权。受控端核销一次性授权。
2. 受控端按操作范围及确认执行会话，撤销或租约到期后停止控制。
3. 可查策略版本、应用失败和撤销状态，能将状态与端点事件关联。
**Plans**: 6/6 plans complete。Windows 双架构构建和[同候选全量复验](phases/03-m2/03-VERIFICATION.md)通过。
- [x] 03-01: 固定受管源码、协议和设备运行身份
- [x] 03-02: 连接 ACL、版本化策略与设备回执
- [x] 03-03: 会话签发、核销、续租、撤销与审计
- [x] 03-04: RustDesk 两路径受管接入和端点原生构建
- [x] 03-05: 中文策略与会话工作台及完整开发回归
- [x] 03-06: Windows 通讯录连接、无人值守发起和本机确认权限缩减

### Phase 4: M3 部署与自动化交付

**Goal**: 管理员可部署中心和双区域中继，签名发布、免填纳管设备并管理升级恢复。
**Depends on**: Phase 3
**Requirements**: FR-01, FR-05, FR-06, FR-14, FR-15, FR-16, FR-17, FR-19, NFR-02, NFR-04, NFR-05
**Success Criteria**:
1. 管理员可从空机开始安装，重跑可复用身份及数据库，并从任务记录查实际进度。
2. 两区候选由中心筛选，使用双端 RTT 选点并记录回退和故障原因。
3. 校验过的签名包按组发布。设备连接、登记、审批及实际策略回执可查。
4. 可重试失败项，验证升级暂停、回滚与备份恢复流程。
**Plans**: 3/6 plans complete。依赖详见 [M3 实施顺序](phases/04-m3/04-CONTEXT.md)。
- [x] 04-01: [正式中心安装、配置验活及中断重跑](phases/04-m3/04-01-PLAN.md)
- [x] 04-02: [可恢复任务与安装结果导入](phases/04-m3/04-02-PLAN.md)
- [x] 04-03: [区域中继安装、健康与容量](phases/04-m3/04-03-PLAN.md)
- [ ] 04-04: [双端 RTT 与受管多中继](phases/04-m3/04-04-PLAN.md)
- [ ] 04-05: [签名发布、分组下载与免填登记](phases/04-m3/04-05-PLAN.md)
- [ ] 04-06: [升级恢复、试点迁移及开发门禁](phases/04-m3/04-06-PLAN.md)

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
| 2. M1 用户、设备与管理面 | 4/4 | Complete    | 2026-11-22 |
| 3. M2 受管连接与策略 | 6/6 | 开发验证完成 | 2026-11-23 |
| 4. M3 部署与自动化交付 | 3/6 | 04-04 中心探测轮次与双端 RTT 报告通过，接续网络范围和原生入口 | - |
| 5. 首版目标环境验收 | 0/TBD | Not started | - |
