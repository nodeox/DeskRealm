---
phase: 04-m3
status: in_progress
---

# M3 开发验证

## 04-01 中心安装预检

候选 `73caf84e3985cfe22f5b14f3042bbd4107b5d8f9` 于 2026-11-23 验证。

| 检查 | 结果 |
| --- | --- |
| `bash scripts/deskrealm-m3-test install-preflight` | 5 项具名 Go 竞态用例通过，覆盖域名和端口归一化、严格 JSON、镜像摘要表示、HTTPS 证书、Docker 上下文和 TCP/UDP 端口冲突。 |
| 实际 CLI 预检 | macOS ARM64 开发机检查 Docker Engine 29.8.2 Linux ARM64 及 Compose 5.5.0。使用临时配置，四个 TCP/UDP 绑定通过，人工占用会合 UDP 测试端口时复跑报冲突。部署目录在预检后仍不存在。 |
| 构建及基础回归 | Linux amd64、arm64 静态交叉编译成功。安装器 `go vet` 和全仓 `go test -count=1 ./...` 通过。 |

## 04-01 中心身份和 Compose 准备

候选 `2881b2ac4bcb813aa273348311ca99051089cc7c` 于 2026-11-23 验证。

| 检查 | 结果 |
| --- | --- |
| `bash scripts/deskrealm-m3-test install-prepare` | 4 项具名 Go 竞态用例通过，覆盖复用部署身份、并发准备、拒绝不完整部署及证书模式。 |
| 实际 Docker Compose 解析 | Compose 5.5.0 解析四项服务。确认 PostgreSQL 使用私有网络且无主机监听，外部反代模式的 API 仅发布到回环地址，TCP/UDP 会合端口符合配置。输出未展开凭据。 |
| 隔离 PostgreSQL 18.7 初始化和重建 | 迁移和运行账号均为非超级用户。迁移账号创建测试表和序列，运行账号可读写记录且无法向业务 schema 创建表。强制重建数据库容器后保留原记录。复跑 `prepare` 后部署 ID、受管签名公钥和会合公钥保持一致。 |
| 基础回归 | 全仓 `go test -count=1 ./...`、中心和安装器 `go vet`、预检套件及 `git diff --check` 通过。 |

## 04-01 固定摘要镜像与隔离首管联调

2026-11-23，在 `b301c57b39b326d260f66f6a5b5434c921fc39cb` 运行 `bash scripts/deskrealm-m3-test install-bootstrap`，退出 0。Linux ARM64 Docker 29.8.2 与 Compose 5.5.0 使用回环 Registry 和隔离 Compose 项目。

| 检查 | 结果 |
| --- | --- |
| 中心与会合镜像 | 静态 Linux Go 二进制与生产 Vue 文件进入中心镜像。会合镜像使用固定上游及补丁通过 `cargo build --locked` 构建的 `hbbs`。中心镜像摘要为 `sha256:38a8821eb5ccd17e02a84219d54a9ccf3b852f9ae1bec4b1ec389c757b3ea98a`，会合镜像为 `sha256:dcbc870229b4d5d67d4fd093175301cb13607e802ea5655c5268c1be050e1981`。 |
| 实际启动与验活 | PostgreSQL 18.7 就绪后执行一次性迁移，再启动中心和受管会合。HTTPS 状态 API 通过测试 CA 校验。会合 TCP 入口返回实际 RustDesk NAT 探测响应，检查端口可达，容器内可解析中心域名。启动后复跑 `prepare` 返回同一部署 ID 和两套公钥。 |
| Chromium 首管 | 通过主机执行中心的 `bootstrap-ticket` 生成一次性链接。Chromium 使用测试叶证书公钥指纹访问临时域名，完成初始化及管理员登录并得到 Secure、HttpOnly 会话 Cookie；随后 API 确认已初始化。 |
| 回归 | M3 预检和准备竞态套件、全仓 Go 测试及 vet、Vue 类型检查和原有 M1 Chromium 初始化场景通过。 |

日志在本地 Git 目录下的 `deskrealm-evidence/m3/b301c57b39b326d260f66f6a5b5434c921fc39cb/install-bootstrap.log`，SHA-256 为 `bcefe9453e5b2dac1febff0cf1565977bc8b12d3fd011016581205e3dbc5a68f`。

## 04-01 安装器首装及中断恢复

2026-11-23，在 `4bf6943cf4a940c1bf40eec8967cfc9a4aaf9d4e` 运行 `bash scripts/deskrealm-m3-test install-bootstrap`，退出 0。Docker 使用 Linux ARM64，HTTPS 使用提供证书模式。

| 检查 | 结果 |
| --- | --- |
| 安装器命令 | `install` 依次启动 PostgreSQL、迁移、Go 中心和受管 `hbbs`；HTTPS 校验中心签名身份并通过 RustDesk 探测。状态经过 `prepared`、`ready`、`stopped` 及故障时的 `https` 阶段。 |
| 首次管理员 | `bootstrap-ticket` 产生的一次性链接通过 Chromium 验证初始化和登录。初始化后再次签发被拒绝。 |
| 重跑和重建 | 健康状态重跑保持原容器。移除整组容器后，测试在迁移完成、HTTPS 信任失败时中断安装器；恢复测试 CA 后重跑，保留 PostgreSQL 集群、初始化状态、部署 ID 及原会合和签名公钥。 |
| 数据保护 | 模拟原数据库目录为空和记录标识与实际集群不符，两种情况均拒绝启动 Compose 服务。 |
| 基础回归 | 全仓 Go 测试和 vet、M3 预检和准备竞态用例通过；安装器静态交叉编译为 Linux amd64 和 arm64。 |

日志在本地 Git 目录下的 `deskrealm-evidence/m3/4bf6943cf4a940c1bf40eec8967cfc9a4aaf9d4e/install-bootstrap.log`，SHA-256 为 `8d06f6ff51062f329598b9bcca51e4ff59792f1ed5db04f3591895e7a2941458`。

Task 1 的已有 Docker 首装开发验证通过。

## 04-01 签名离线包

2026-11-23，在 `df132f85516dbe8a0bd5927da92c0facc82b5cd4` 运行 `bash scripts/deskrealm-m3-test install-offline`，退出 0。运行环境为 Linux ARM64 Docker 29.8.2、containerd 镜像存储和 Compose 5.5.0。

| 检查 | 结果 |
| --- | --- |
| 具名竞态用例 | 校验独立可信 Ed25519 公钥、原始签名、配置中的摘要引用、相对归档文件名和引擎架构。签名不匹配、路径越界、重签其他清单、同长度归档篡改及加载后的架构不符均被拒绝。 |
| 实际离线导入 | 归档包含三个镜像，关闭临时 Registry 并清除测试构建的中心和会合镜像。篡改归档时启动被拒绝；恢复后通过本地镜像 ID 导入，实际启动四项服务并完成 Chromium 首管。 |
| 复用与重建 | 移除离线介质，重建整组 Compose 服务并模拟迁移后的安装中断。重跑复用已核验缓存、原 PostgreSQL 集群和两套公钥。 |
| 基础回归 | 通过原 Registry 摘要路径的 `install-bootstrap`、全仓 Go 测试和 vet、M3 预检和准备竞态用例；安装器静态交叉编译为 Linux amd64 和 arm64。 |

验证清单的 SHA-256 为 `b28c07b45c6e5d6421ce27113851f9811813769052342bf8734ef53af2f3c154`。日志在本地 Git 目录下的 `deskrealm-evidence/m3/df132f85516dbe8a0bd5927da92c0facc82b5cd4/install-offline.log`，SHA-256 为 `fc0f5b87c261eafdda9b4c3e87c0c5f17bf4045ab459ded74dc53e9a2ed63fbe`。

## 04-01 空机运行时及可恢复安装记录

2026-11-23，在 `5d327b49c7445bd142b40b5a895214279b53cf2a` 验证。空机检查使用无 Docker 的 OrbStack 隔离 Linux 实例，amd64 通过 Apple Silicon 上的 x86-64 模拟执行。

| 检查 | 结果 |
| --- | --- |
| Ubuntu 24.04 和 Debian 12，各含 amd64、arm64 | 内置公钥验证官方 APT 元数据，下载并核对四项固定版本包；`prepare-runtime` 首次为 `installed`，systemd 上 Docker Engine 29.9.3 和 Compose 5.8.0 可用，重跑为 `reused`。Ubuntu amd64 首次远程执行未返回，重启该隔离实例后从无 Docker 状态复跑通过。 |
| 在线 `install-resume` | 三项具名竞态用例及 Linux ARM64 中心夹具通过。迁移后中断时只记录验活为中断，重跑保留任务 ID。仅恢复会合服务时，已完成的迁移容器 ID 与启动时间未变化。 |
| 离线 `install-offline` | 两项具名竞态用例及真实隔离导入通过。在离线介质移除后，中断重建仍保留原集群、部署 ID、两套公钥及安装任务。 |
| 丢失与冲突 | 已纳入日志版本的部署丢失日志或数据库标记时拒绝启动。原数据库为空或集群不匹配时拒绝重建。历史部署可沿用原身份并建立记录。 |
| 基础回归 | 全仓 Go 测试、vet、预检和准备竞态检查通过，安装器静态构建支持 Linux amd64 和 arm64。 |

日志位于本地 Git 目录的 `deskrealm-evidence/m3/5d327b49c7445bd142b40b5a895214279b53cf2a/`。`install-resume.log` 的 SHA-256 为 `61f0a51985709be79e502f68d8d3a86c3543810e320291c47ad0e511be5c39b3`，`install-offline.log` 为 `3e8b5f04a4c0b5e85543b3f2ee2b19ff2c9656f4e81acd616be21ac0f817d306`。

## 04-01 已有默认中继与安装收尾

2026-11-23，在 `4caf823ab5e3e3a9750f2fefa530f09d553bd86c` 运行 `bash scripts/deskrealm-m3-test install-relay`，退出 0。通过 Linux ARM64 Docker 构建锁定源码的 `hbbs`、`hbbr`；`hbbr` 单独启动，未并入中心 Compose。

| 检查 | 结果 |
| --- | --- |
| 持久配置与凭据 | 默认中继 ID 和地址归一化。中心私有文件保存中继凭据，会合只收到中继 ID 和地址。实际运行配置和凭据隔离通过具名 Go 竞态检查。 |
| 实际启停 | 核对运行中中心和会合的中继配置。独立 `hbbr` 停机后，状态报告 `default-relay` 和 TCP 不可达；恢复后回到 `ready`，Chromium 首管通过。 |
| 失败边界 | 模拟 Registry 缺少固定镜像，安装记录仅镜像步骤失败，未启动任一 Compose 服务。重建进入 HTTPS 验活后中断，记录挂在验活步骤。重跑沿用原任务、数据库和两套公钥，并验证会合恢复未重跑已完成的迁移。 |
| 回归 | 未配置默认中继的 `install-resume` 同提交复跑退出 0。全仓 Go 测试和 vet、M0 与安装器竞态检查、M3 预检和准备回归通过。 |

日志保存在本地 Git 目录的 `deskrealm-evidence/m3/4caf823ab5e3e3a9750f2fefa530f09d553bd86c/`。`install-relay.log` SHA-256 为 `f705f0544109932e486bf87faa9d6520467614e352fa0eb69bf84231c8a98428`，`install-resume.log` 为 `da95abbfd01da1d98c8607b49dc1f3e9625a632b25b5cd2d8e7132acff33d3fa`。首次复跑遇到 Rust 增量链接找不到目标文件，重跑通过链接。随后修正验活记录和中断测试的时序，以上使用修正后的提交取证。

04-01 开发验证完成。04-02 接入安装记录导入。跨网中继认证与实际端点传输按后续计划验证，目标环境 Linux 中心、中继和双 Windows 仍按 Phase 5 顺序执行。

## 04-02 安装结果导入

2026-11-23，在候选 `69e9e41d601e8830c8df381b12b513543c4d2ae9` 运行 `bash scripts/deskrealm-m3-test install-task`，退出 0。

| 检查 | 结果 |
| --- | --- |
| PostgreSQL 竞态 | 六项具名检查通过。六次并发导入相同记录只新增一次快照和审计。冲突修订、未导入的旧修订、缺失记录与未声明字段被拒绝。新的修订复用任务和步骤对象，管理员可读取历史修订，其他成员无法查看或作为导入审计人。 |
| 实际主机命令 | 在 Linux ARM64 隔离 Compose 中，迁移后中断和恢复生成两个本地修订。分别通过 `deskrealm-install import-record` 导入，重复提交均保留原任务且不新增修订。 |
| 同源 Chromium | 中文工作台显示一条中心安装任务。最新详情显示四步成功，历史修订及刷新显示验活已中断。安装步骤由本地安装器继续执行。 |
| 现有路径 | M1 账号导入、任务租约、设备入网和审批 PostgreSQL 回归及资源工作台 Chromium 回归通过。全仓 Go 测试、vet 和 Vue 类型检查通过。 |

日志保存于 `deskrealm-evidence/m3/69e9e41d601e8830c8df381b12b513543c4d2ae9/install-task.log`，SHA-256 为 `534334c6b1b26284e68b6f79333bafdce39f12d6baae9a16d5c8bc2313cec6f8`。Task 2 继续目标级租约、外部回执和取消。
