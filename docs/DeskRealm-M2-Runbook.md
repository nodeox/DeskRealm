# DeskRealm M2 开发运行手册

本手册用于受管连接、策略及中文工作台的开发验证。部署和权限模型见 [M2 协议](DeskRealm-M2-Protocol.md)。M0–M3 开发完成后再开始 Phase 5 的目标环境验收。

## 准备开发环境

在干净的候选 Git 工作树中运行。需要 Bash、Git、Go 1.26 或更高版本、Node 26、npm、Python 3、OpenSSL、`protoc`、Docker Engine 与 Compose。首次构建需要访问锁定的上游仓库、镜像及依赖源。

```sh
npm --prefix web ci --ignore-scripts
npm --prefix web exec -- playwright install chromium
```

按原生 Windows 工作流为两个架构构建和测试。下载 `windows-x64` 与 `windows-arm64` 的内容，把两棵目标目录放到同一个证据根目录。每个目标的目录结构为 `<目标 triple>/<构建提交>/`，含 `manifest.json`、`client.zip`、bridge 清单及六份构建和测试日志。单架构只用于诊断。

```sh
bash scripts/deskrealm-m2-test windows-builds "$windows_evidence"
bash scripts/deskrealm-m2-test all "$windows_evidence"
```

默认要求 Windows 工件来自运行命令时的候选提交。已有旧提交的两套工件且 Windows 原生输入逐项一致时，可以显式传入构建提交。

```sh
bash scripts/deskrealm-m2-test all "$windows_evidence" "$windows_build_commit"
```

聚合验证要求构建提交是当前候选的祖先，逐项检查上游锁、补丁、受管协议源文件、源码准备脚本、Windows 工作流和验证器输入。聚合脚本属于测试入口，原生编译结束后才调用它验证制品。兼容模式在构建提交的临时工作树使用原验证器，并保留实际构建提交、两份工件清单和校验结果。

## 开发门禁

| 门禁 | 检查内容 |
| --- | --- |
| `m0-m1` | M0 配置、生命周期及手册约定，M1 PostgreSQL、Go 竞态和静态检查、命令构建、Vue 及三组 Chromium 流程。 |
| `managed-db` | 具名 Go 用例检查设备和主控身份、规则与策略、授权签发和核销、续租、撤销和范围审计。 |
| `sources` | 空目录检出锁定上游及子模块并重放补丁，验证移动标签、错版子模块和脏源码的拒绝，核对 Go 和 Rust 的协议向量。 |
| `endpoint` | 实际 `hbbs`、`hbbr` 和原生端点在独立 Docker 回环网络运行。TLS 中心夹具核对策略、确认、两种 TCP 路径、核销、能力、租约、停止和文件回执。另有 11 项原生 Linux 桌面测试、4 项租约测试和 1 项恢复复核。 |
| `managed-browser` | 生产 Vue、Go 与临时 PostgreSQL 核对四类角色、规则、策略、设备回执、撤销和按会话导出。心跳及已存在的会话由夹具提供，端点停止回执经过 API。 |
| `windows-builds` | 两个原生架构的 bridge、构建工具链、PE 架构、ZIP 及其中发布库的摘要、具名桌面和租约测试与恢复测试。 |

脚本在终端打印证据目录。可提前建好 `DESKREALM_M2_REPORT_ROOT` 指向的目录以长期保留证据。默认使用系统临时目录。保留 `environment.txt`、`results.tsv`、各门禁日志、通过的 Chromium JSON 和 Windows 工件清单。原生日志为已确认停止的夹具会话记录实际路径、会话 ID、连接 ID 和使用的中继。失败门禁保留退出状态，发生失败时聚合命令返回非零。

单独运行 `identity`、`rules`、`policies`、`sessions`、`leases`、`endpoint` 或 `managed-browser` 可定位对应问题。已填充的 Linux Cargo 缓存可用于复跑端点。

```sh
export DESKREALM_NATIVE_CACHE_ROOT=/absolute/path/to/deskrealm-native-cache
export DESKREALM_NATIVE_OFFLINE=1
bash scripts/deskrealm-m2-test endpoint
```

离线选项仅作用于原生端点 Cargo 容器；源码取得和其他门禁仍可能访问镜像、仓库和依赖源。

## 隔离构建与工作台

```sh
python3 scripts/deskrealm-managed-source prepare /absolute/path/to/managed-source --apply-patches
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/deskrealm-linux-amd64 ./cmd/deskrealm
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/deskrealm-linux-arm64 ./cmd/deskrealm
npm --prefix web run build
```

从重放补丁后的 `server` 目录，在锁定的 Linux 服务端工具链和依赖环境执行 `cargo build --locked --release --bin hbbs --bin hbbr`。原生开发检查使用固定镜像，运行配置和启动约束见 [M2 协议](DeskRealm-M2-Protocol.md)。

按 [M1 启动与迁移手册](DeskRealm-M1-Runbook.md) 准备隔离 PostgreSQL 和工作台。运行未配置受管中心的界面回归时使用 `managed-browser`。运行实际受管会合时，中心启用 HTTPS 和持久签名种子。会合、中继和两端都配置可信中心公钥；会合和中继分别用各自的服务凭据。重建保留中心 PostgreSQL、受管签名种子与 `kid`，以及 M0 会合私钥和数据。

在中文工作台内先审批设备，创建连接规则草稿并发布。发布设备策略版本并分配范围，从设备详情核对期望版本、已应用版本和实际回执。连接后从「会话管理」按会话 ID 查看实际路径、租约和停止状态。具备权限的管理员可提交撤销，并跳转到按同一会话 ID 过滤的审计和导出。

## M1 交接及原生入口范围

| 交接 | 本次可核对的结果 |
| --- | --- |
| FR-02 | PostgreSQL 续租用例验证账号停用、登录失效及撤权的会话拒绝。 |
| FR-07 | 控制面支持以通讯录条目申请授权，并在后续准入及续租重新检查访问。Windows 主控当前按 RustDesk ID 发起，原生通讯录条目入口待接入。 |
| FR-13 | 会话路径、核销、撤销、停止和文件事件使用会话 ID 关联。真实 Go API 支持按授权查询与导出。中继健康随 M3 接入。 |
| FR-18 | 中文控制台和 Windows 受管直连、强制中继及文件入口已有开发证据。安装分发和免填配置随 M3 接入。 |

Windows 原生主控当前发起有人值守申请。中心和受控库可验证经设备策略批准的无人值守请求。Windows 本机确认显示主控用户、设备、用途和申请的操作，提供接受或拒绝。主控无人值守请求入口与逐项缩减操作仍待补齐。

M3 接续多中继健康与双端 RTT 选点、部署和签名发布、免填安装、升级暂停与恢复、迁移和重试。Windows ZIP 是开发构建工件。发布验收使用 M3 生成并签名的发布产物。

## Phase 5 现场取证顺序

1. 先按 [M0 目标环境手册](DeskRealm-M0-Runbook.md) 验证 Linux 中心和一台独立中继，记录镜像与架构、运行端口、持久挂载、会合公钥指纹及中继可达性。
2. 两台 Windows 依次验证注册、实际直连、强制中继和重建后沿用原公钥并重新连接。记录每条实际连接的时间、端点版本、路径和中继配对。受管环境同时比较签名 `kid` 与公钥指纹，并保留连接及审计的会话 ID。
3. 基础拓扑通过后，执行受管能力、双区域选点、发布和迁移。两种路径都实测在线撤销收到端点回执的耗时和隔离链路后端点实际停控的耗时，分别与 5 秒和 35 秒比较。加入睡眠恢复，核对恢复前暂停操作和恢复后的重新校验。

每份现场证据记录候选提交及产物 SHA-256、操作系统与架构、UTC 时间窗、角色、会话和连接 ID、实际结果及预期结果。共享前移除私钥、签名种子、Bearer、票据、数据库连接串和会话内容。
