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

## 04-02 目标级租约、外部回执和取消

2026-11-23，在候选 `f423a62edaa42b4d0d7aba33a6ce90e16f814731` 运行 `bash scripts/deskrealm-m3-test delivery-tasks`，退出 0。

| 检查 | 结果 |
| --- | --- |
| PostgreSQL 竞态和授权 | 开发夹具验证创建请求幂等、冲突及越权拒绝。八路并发只领取各目标一次。已领取目标不可取消，混合版本取消按整事务拒绝。 |
| 外部结果恢复 | 独立磁盘夹具保留外部作用和回执，中途丢失中心响应。新数据库连接池及新执行器实例接管过期租约，核对原操作 ID，外部作用仅执行一次。最后一个常规租约丢失响应时，最终核对也能找回成功回执。 |
| 重试和回执 | 外部确认失败后以原操作 ID 和上次回执重试。错误对象、旧租约、已用于另一尝试的回执均被拒绝。当前完成回执可幂等重放，任务、结果和审计在同一事务持久化。 |
| 同源 Chromium | 真实中心和 PostgreSQL 显示外部回执、操作 ID 和失败状态。可重试失败目标和取消尚未领取目标，结果在数据库核对。 |
| M0/M1 回归 | `bash scripts/deskrealm-m1-test all` 退出 0。25 项具名 Go 回归、全仓竞态检查、Vue 类型检查和三个 Chromium 场景通过。 |

外部执行由开发夹具提供。常规中心启动尚未注册外部交付执行器，区域中继等真实任务在后续阶段接入。日志保存在本地 Git 目录的 `deskrealm-evidence/m3/f423a62edaa42b4d0d7aba33a6ce90e16f814731/`。`delivery-tasks.log` 的 SHA-256 为 `588dc1b5349f497c40727d855c2b431f4d4d1358aaefb5d03114976daaa15a8b`，`m1-all.log` 为 `59b22ccd999d28c8ba7b36d9c419dc37ec5f195c20a9696c68e79606072bd6da`。

## 04-03 中继登记和签名健康

2026-11-23，在 `765fb2cb73f27f00a6f7b78abbc866502e278920` 运行 `bash scripts/deskrealm-m3-test relay-identity`，退出 0。测试使用真实 PostgreSQL、中心 HTTP 处理器和两份独立 Ed25519 夹具密钥。

| 检查 | 结果 |
| --- | --- |
| 签发与授权 | 仅平台管理员可签发和读取节点。凭据绑定任务和配置。地址冲突被拒绝，未登记节点可在原任务下补发，旧凭据和挑战失效。 |
| 持钥证明 | 八路并发提交仅一次消费，跨节点凭据、共用节点密钥、错误签名、篡改端点、过期凭据和过期挑战被拒绝。 |
| 健康回执 | 验证节点签名、配置版本、容量、构建版本、时间窗和递增序号。过期健康不影响其他节点。两节点均保持待独立探测。 |
| 任务与审计 | 节点登记关联原任务及唯一登记审计事件。管理员列表不含登记凭据、挑战和节点公钥。 |
| 原有路径 | 同提交的 M0/M1 全量回归通过，包含 Go 竞态检查、Vue 类型检查、首登、资源与角色 Chromium 场景。 |

`relay-identity.log` 位于本地 Git 目录 `deskrealm-evidence/m3/765fb2cb73f27f00a6f7b78abbc866502e278920/`，SHA-256 为 `c0e0225e54580eeeb40d4f153953a826efb10e41f25764fcbf0f53f6e7462acd`。本轮通过中心协议，真实 `hbbr`、节点主机持久身份及独立探测留在 04-03 Task 1。

## 04-03 中继主机身份和 HTTPS 登记

2026-11-23，在 `4c2a785fad6e94da36a2fb06d5c35e617dde17d7` 运行 `bash scripts/deskrealm-m3-test relay-host`，退出 0。使用 macOS ARM64 的实际 CLI 子进程、HTTPS 中心处理器和 Linux Docker 内的隔离 PostgreSQL 18.7。

| 检查 | 结果 |
| --- | --- |
| 独立主机身份 | 两个状态目录分别生成 Ed25519 身份，状态文件权限为 `0600`。重新准备、登记和读取状态均沿用各自公钥。更换中心绑定或丢失状态文件时命令拒绝执行。 |
| HTTPS 与恢复 | 严格校验中心证书，拒绝未受信的证书。在中心事务提交后模拟回复丢失，删除一次性凭据，节点用同一密钥签名并取回配置。两个节点共完成两次登记及两条登记审计。 |
| 中心校验 | 错误节点密钥、过期配置读取证明，以及映射到 IPv4 未指定地址、组播地址和带作用域地址的公布端点被拒绝。 |
| Linux 构建及容器重建 | 静态 CLI 构建通过 Linux amd64、arm64。Linux ARM64 的三个独立容器挂载同一状态目录，均返回原节点身份。 |
| 回归 | 同提交 `bash scripts/deskrealm-m1-test all` 退出 0，包含 vet、全仓 Go 竞态回归、Vue 类型检查及三个 Chromium 场景。 |

日志位于本地 Git 目录 `deskrealm-evidence/m3/4c2a785fad6e94da36a2fb06d5c35e617dde17d7/`。`relay-host.log` 的 SHA-256 为 `f5e0ab1040e98b8c4a5ed252c0de877fe28242955df4b5a27942a150c60126dd`，`relay-host-linux.log` 为 `55b46d83c58751dac06b94ed40654624f2164b4320ce51d4afdbea233adcb5db`，`m1-all.log` 为 `3514d1292e94d9c88abd82f5cc575d38cf5fab0126089855995e28b957f16131`。

Task 1 继续启动两套受管 `hbbr`、从中继上报签名健康，并由中心对公布端点做独立协议探测。

## 04-03 区域中继实例与独立入池探测

2026-11-23，在 `8fdaedad6f4f6470933e1c9542d70c09ec03394d` 运行 `bash scripts/deskrealm-m3-test relay-enrollment`，退出 0。重放锁定的 RustDesk 服务端补丁并构建 Linux ARM64 `hbbr`。三项 Go 竞态用例使用临时 PostgreSQL 18.7，区域用例启动两个真实且独立的 `hbbr` 容器。

| 检查 | 结果 |
| --- | --- |
| 主机健康与公布地址 | 两个节点分别以自己的私钥通过 HTTPS 上报健康，随后在不同公布端点应答独立签名挑战。仅有健康时保持待探测。将甲的公布端点转接到乙，即使 TCP 和协议均可达，甲仍无法以乙的公钥入池。 |
| 入池和故障 | 两个正确端点分别通过，登记任务各只完成一次且各有一条入池审计。向已入池节点重复注入错误身份，第三次失败摘除，恢复正确端点后重新入池。 |
| 容量与重启 | 在 PostgreSQL 注入容量上限及过期健康，目录分别排除相应节点。实际重建一个 `hbbr` 容器后，健康序号继续增长，恢复入池，沿用原公钥。 |
| 构建 | Go 中心和节点 CLI 的 Linux amd64、arm64 静态构建通过。区域 `hbbr` 实例在 Linux ARM64 中运行。 |
| 兼容路径 | 锁定补丁的 Linux ARM64 `hbbs` 和 `hbbr` 通过 M2 单中继原生夹具，覆盖有签名端点连接、会合路由与中继配对。同提交的 M1 全量回归和 12 项受管 Go 竞态检查通过。 |

日志位于本地 Git 目录 `deskrealm-evidence/m3/8fdaedad6f4f6470933e1c9542d70c09ec03394d/`。`relay-enrollment.log` 的 SHA-256 为 `af538c4c584e147bd4bae103a4408546f1d1785205e3706cf6b928dbf071f07a`，`linux-builds.log` 为 `eab73106b6cca3b63fb8c7ee349ca41d17fa21d6a1d78aca004e8086c5886726`。

同目录的 `managed-single-relay.log` SHA-256 为 `7dd5b9121d069667f8ecfdba82491543cbf5f52ad45379e7bf078209103561f5`，`managed-db.log` 为 `277e7f8e5671e9389a80d0f8c759eba481dde71a924b771dd6e601ec0d1e178c`，`m1-all.log` 为 `b74cd2114fce6e7e5f5172b91cb97591e87ff361bc4a971e1ca93c361a589bc0`。

Task 1 的节点验活及入池开发验证通过。Task 2 接续排空、停用、地址切换和操作台。区域节点的应用会话转发在 04-04 随节点票据启用。

## 04-03 中继生命周期和控制台

2026-11-23，候选 `8977de324e6a50edf30ed93aa735eda190433e35` 运行 `bash scripts/deskrealm-m3-test relay-lifecycle`，退出 0。检查使用临时 PostgreSQL 18.7、受控签名 TCP 端点与真实中心 HTTP 处理器。

| 检查 | 结果 |
| --- | --- |
| 生命周期与权限 | 平台管理员执行排空、停用及验活恢复。成员、缺少 CSRF 和过期修订被拒。六路同时排空只提交一次操作。会话夹具在排空后真实续租到序号 2，节点详情可查看会话及当前状态。 |
| 地址、容量和故障 | 拒绝已占用地址，以及能应答但身份错误的新端点。新端点验证后才提交配置，旧版本健康被拒，当前版本健康才允许重新入池。验证满载、过期健康、三次故障摘除及探测恢复。探测历史、操作任务与审计可按节点关联。 |
| Chromium | 通过任务中心签发和补发凭据并查看主机登记命令。操作排空、停用、恢复和公布地址，查看故障、满载、健康过期、已建立会话、操作任务和按权限显示的概览计数。 |
| 区域节点联调 | 在隔离 Linux ARM64 容器中复跑两套 `hbbr`。中心核对新公布端点并切换地址后，运行中的节点同步配置并签署新版本健康，重新入池并沿用原公钥。 |
| 回归 | M1 全量门禁、Go vet、全仓 Go 测试和 Vue 类型检查通过。 |

日志保存在本地 Git 目录 `deskrealm-evidence/m3/8977de324e6a50edf30ed93aa735eda190433e35/`。`relay-lifecycle.log` SHA-256 为 `6245279a7b98871c8e9c8064559e94042d115d1fe7e29b08823bcdc58bdb2f58`，`relay-enrollment.log` 为 `1dee8b124b6cd556a6d115212d2efbb58e7e12120acb6cef805eedd735f7fbaf`，`m1-all.log` 为 `a105e4bd9abb27821105e72d0d8a5f9c7988776ed4571306932a3e35b07babcd`。

04-03 开发验证完成。04-04 接续区域会话候选、票据与实际转发。

## 04-04 会话探测轮次与双端报告

2026-11-23，候选 `70723e0f1dbaef0514b766547083dea603b123f9` 运行 `bash scripts/deskrealm-m3-test relay-probes`，退出 0。

| 检查 | 结果 |
| --- | --- |
| 会话与候选范围 | 临时 PostgreSQL 18.7 验证主控先建立轮次、受控端签名加入后开启两秒窗口。候选快照按组织、节点生命周期、容量和中心探测筛选。排空、满载及签名健康过期的节点不入集合，轮次中排空的节点退出有效结果。 |
| 结果及回放 | 各端按节点提交三个签名采样。两个成功时取两值平均数，三个成功时取居中值。超时、不可达、缺样和样本不足保留状态。拒绝错端、错轮次、错地址、错配置版本、重复、过窗和撤销后的请求。新连接生成新版本轮次。 |
| Go 与 Rust 签名 | 两套实现均通过加入与采样报告向量，以及无效角色、失败采样夹带 RTT、RTT 越界的反向向量。 |
| 两套实际中继 | Linux ARM64 Docker 启动两套锁定补丁的 `hbbr`。Go 测试中的主控和受控端分别对两节点各做三次真实 TCP 往返并验证节点签名，四组报告通过会话 API 入库，两节点均有双端中位数。 |
| 回归 | `managed-db` 的 12 项具名竞态测试通过，涵盖原有规则、设备身份、会话核销、租约与撤销。M1 全量 Go、Vue 类型检查和 Chromium 门禁通过。 |

`relay-probes.log` 和 `managed-db.log` 保存在本地 Git 目录 `deskrealm-evidence/m3/70723e0f1dbaef0514b766547083dea603b123f9/`，SHA-256 分别为 `0c7291f0bfd87e78480ded6250aaa953c55e7f214b632b2c0ff9ad760464527e` 和 `277e7f8e5671e9389a80d0f8c759eba481dde71a924b771dd6e601ec0d1e178c`。

同目录的 `m1-all.log` SHA-256 为 `df734bffa1f5966cc1cdaa1a3c470995f78d4763df6a963b142d663afd5a17a0`。

Task 1 接续站点与 CIDR 许可范围、Rust 端点采样入口和节点侧探测凭据。最低双端 RTT 选点与区域转发由 Task 2 接入。

## 04-04 站点与网络策略

2026-11-23，候选 `1b6a28190893121ec8c2bcf1edddac4a1947c143` 的 `relay-probes`、M1 全量回归、`install-preflight` 与 `install-prepare` 均退出 0。

| 检查 | 结果 |
| --- | --- |
| 候选范围 | PostgreSQL 检查 IPv4 与 IPv6 站点、最长前缀优先、方向性站点对、许可区域及节点必须覆盖双方网段。反向站点对和未匹配的网络无候选。 |
| 修改与权限 | 仅平台管理员可读写规则，同源写要求 CSRF 和正确修订。拒绝未归属节点和带主机位的 CIDR。新策略提交后，旧轮次拒绝读取和上报，已提交历史样本依旧可持久化核对。 |
| 来源地址 | 只接受显式配置的直接代理的单个转发地址，忽略其他来源的转发头。来源地址变化要求新轮次。安装器将外部 HTTPS 的信任代理设置注入中心 Compose 服务。 |
| 兼容与构建 | 初始使用组织模式，显式恢复该模式可重获组织级候选。已有两套真实 Linux `hbbr` 的双端往返、Go/Rust 签名向量及 M1 Chromium 回归通过。中心和安装器的 Linux amd64 与 arm64 静态构建通过。 |

日志保存在本地 Git 目录 `deskrealm-evidence/m3/1b6a28190893121ec8c2bcf1edddac4a1947c143/`。其中 `relay-probes.log` 的 SHA-256 为 `2b7c7e8fc3574ac1477038850195d4376bdc73f59add748820b5ddb399bc74f7`，`m1-all.log` 为 `c0cc1c148a003e347549ee9e216fa53e2d09a604b88942bb65b4dcf34bfee16f`。

Task 1 接续受管客户端的原生采样入口，以及绑定会话、角色和节点的探测凭据。规则目前通过管理员 API 配置。

## 04-04 中继侧探测授权

2026-11-23，候选 `3672befa982631e3e2a7fd9c812bf37165f1c632` 的 `relay-probes`、`relay-enrollment`、`managed-db`、M1 全量回归、全仓 Go 测试和 `go vet` 均通过。

| 检查 | 结果 |
| --- | --- |
| 会话和节点边界 | 中心为当前端点角色签发与会话、授权、连接、轮次、节点、配置版本及公布地址绑定的凭据，窗口最长两秒。`deskrealm-relay run` 经中心 HTTPS 取得验签公钥并交给 `hbbr`。 |
| 有效探测 | 两套 Linux ARM64 `hbbr` 上，两个 Go 端点夹具各获得自己的凭据，分别对两台中继完成三个带随机挑战的持钥往返。四组签名报告入库，两台节点均有双端中位数。 |
| 拒绝和重放 | 实际节点拒绝错节点、错端点私钥、错中心签名、错公布端点、错配置版本、已消费序号和过期凭据。Go/Rust 签名向量包含超长有效期、零配置版本以及序号越界。轮次结束后中心不再下发凭据。 |
| 现有路径 | 中心独立健康探测和当前区域中继地址切换用例继续通过。M1 PostgreSQL 和三个 Chromium 场景通过。 |

日志保存在本地 Git 目录 `deskrealm-evidence/m3/3672befa982631e3e2a7fd9c812bf37165f1c632/`。`relay-probes.log` 的 SHA-256 为 `6a41ebbb4068f9e2f0c5b2b51ce1c0efa9941d726a3fea1fc05e0b3d852f4150`，`m1-all.log` 为 `dae597156ae2376e3f8addbe353c35d437dcbdf8d3f5b6479ec734018266f439`。

同目录的 `relay-enrollment.log` SHA-256 为 `8fca02e7cc548dffe667adceb7ff0333d11dfdec49665e665c3b0c08645e6d78`，`managed-db.log` 为 `277e7f8e5671e9389a80d0f8c759eba481dde71a924b771dd6e601ec0d1e178c`。

Task 1 接续 Rust 主控和受控端的采样入口。节点在本地校验，中心撤销后，之前签出的凭据可用到两秒窗口结束。

## 04-04 Rust 双端原生探测

2026-11-24，候选 `1c492ce446e53aa67a2fb3b7e4f6f8371c693cd6` 的 `relay-probes`、Linux ARM64 受管桌面编译检查、M2 原生准入回归和 M1 全量回归均通过。

| 检查 | 结果 |
| --- | --- |
| 原生会话入口 | 主控发起中继会话时以当前连接创建探测轮次。受控端处理会合转发的中继请求时加入。双方使用现有 Rust `Authority` 的 HTTPS 与设备签名。桌面接入通过 `cargo check --locked --lib`。 |
| 双端真实采样 | 隔离 Linux ARM64 Rust 测试经真实 Go 中心和测试 CA，与两套实际 `hbbr` 通信。双方验证中心签发的节点凭据及中继响应，分别提交每节点三个成功 RTT。在 PostgreSQL 验证共 12 个成功样本，关联同一会话。 |
| 原有路径 | M2 直连、单中继端点准入和四项租约检查通过。M1 全仓 Go 竞态检查、Vue 类型检查和三个 Chromium 场景通过。 |

日志保存在本地 Git 目录 `deskrealm-evidence/m3/1c492ce446e53aa67a2fb3b7e4f6f8371c693cd6/`。`relay-probes.log` 的 SHA-256 为 `2994d411060ce2df2ddfa1d8678ad140f75536220bdb31a295ef6e5cbfcd5a17`，`desktop-check.log` 为 `59ecb96c0ee6578434061b09edaa86a2f20d7b81dc5e5b5df29b0f72525fb324`。

同目录的 `native-admission.log` SHA-256 为 `76be8b2bac92b127c50ef3c9e071ec0674999ef16fd22013469dfd27c4cdabd6`，`m1-all.log` 为 `3b68f329646ce3cc149029b1bd69f278bad8e13fa107765172083d0b14b4217b`。

Task 1 的开发验证完成。当前连接仍使用配置的单中继。Task 2 接续按双端样本选点、回退与区域会话转发。

## 04-04 中继选点与回退

2026-11-24，候选 `2c645512405ac517c48ef22a1ab1422dc3a213c2` 的中心选点回归、双 Linux ARM64 `hbbr` 原生测试、Go 全量测试和 vet，以及 M1 全量回归通过。

| 检查 | 结果 |
| --- | --- |
| 双端 RTT 选点 | 两套实际 Linux ARM64 `hbbr` 分别接受 Rust 双端和 Go 双端夹具的持钥探测。Go 夹具使用双方各三次测量的中位数之和验证中心排序，并完成确认。受控端随后读到已确认的节点。 |
| 当前状态复核 | 预览后模拟首选节点满载，中心拒绝该次确认。主控刷新后取得新的首选。确认时锁定候选节点行并检查健康、配置修订、端点及容量。 |
| 回退和重试 | 覆盖站点、管理员静态 CIDR GeoIP、默认区域、负载和节点 ID 排序，完全并列稳定排序，明确失败样本排除，连接失败后取下一节点、重试幂等、候选耗尽、过期窗口及新连接轮次。 |
| 其他回归 | `relay-selection` 专项（含双端签名向量）、M1 全量回归、`go vet ./...`、`go test -count=1 ./...` 均通过。原生凭据重放、错误节点、错误端点密钥、错误签名、错误端点和过期凭据均拒绝。 |

本地 Git 证据目录 `deskrealm-evidence/m3/2c645512405ac517c48ef22a1ab1422dc3a213c2/`。`relay-selection.log` SHA-256 为 `aa51100ad42138d4ebb6391d3ec21a4cff9cdf4eb5e58a25a80ad193d0c0f35d`，`native-relay-selection.log` 为 `686efb8746efcf117856f03dbed594faaac46053fcaf04148e44a117b7fc8e00`，`m1-all.log` 为 `1a64690d927511ca652e3126d27b50f66977eb915c6ea603d999f2ff5255a94d`，`go-vet-test.log` 为 `60be818331e6267067735b01ede3ea5e728c4ff8e892b2c4c152a17569a619a3`。

已实现中心确认、当前状态复核及回退记录。会话实际连接仍未消费该选择，节点级会话票据、`hbbs` 协调与 `hbbr` 区域转发、失败后的真实重连和工作台选点明细仍待开发。Linux 中心与中继的目标环境安装及两台 Windows 的注册、直连、强制中继和服务重建保留公钥验收尚未开始。

## 04-04 区域票据与实际转发

2026-11-24，候选 `e4b0b20620bb402f9d6986e344ffb95236d8ea9a` 通过 `bash scripts/deskrealm-m3-test multi-relay`、Linux ARM64 受管桌面编译检查、原有直连和单中继原生回归、受管 PostgreSQL 回归、Go 全量测试与 vet。

| 检查 | 结果 |
| --- | --- |
| 会合与实际转发 | Rust 主控和受控端经实际 Linux `hbbs` 加入同一轮次。两套 `hbbr` 提供 RTT 样本。两端通过所选区域中继配对，使用端到端加密流双向交换数据。PostgreSQL 核销记录中的节点 UUID 与确认选择一致，租约由 1 续到 2。 |
| 首选失败 | 夹具在首选节点阻断票据连接，保留两节点的验活与 RTT 采样。主控记录首选失败，两端在同一轮次切换至另一节点，完成配对、核销、续租及双向传输。 |
| 票据和回执约束 | 实际 `hbbr` 拒绝错节点、错持钥者与已消费票据，并通过签名验活报告当前配对会话数。PostgreSQL 回归拒绝未确认节点、错签名及主控提交回执，核销时拒绝已被新连接替换的轮次。 |
| 保留路径 | 原生直连和固定单中继准入、受管端点恢复检查及四项租约回归通过。区域模式在组织已登记区域节点或启用受限策略时生效。 |

本地 Git 证据目录 `deskrealm-evidence/m3/e4b0b20620bb402f9d6986e344ffb95236d8ea9a/`。`multi-relay.log` SHA-256 为 `99be7e5737bd84324844e493ebe6e71165b227b2a612ba29cf4494638f088105`，`legacy-native-admission.log` 为 `1f8bc348749fb0ad82e0dbea0fe3baed7180c806add22c66a31dfb67a98e4b69`，`desktop-check.log` 为 `a4b3d7c5c84b2e1dfd99a2a2ce755a517e912bd7ec23192fdcac35d986344ee0`，`managed-db.log` 为 `277e7f8e5671e9389a80d0f8c759eba481dde71a924b771dd6e601ec0d1e178c`，`go-vet-test.log` 为 `8d8a23a6411802c8dd386c01ac2442cb7bd2aad44255f1ffdb0d2c2bb52e5358`。

Task 2 接续已建立会话断线后的新授权重选、会话页明细和跨设置验证。Linux 与双 Windows 目标环境验收仍安排在 M0–M3 开发完成后。

同一候选的区域中继生命周期 PostgreSQL 和 Chromium 用例通过，`relay-lifecycle.log` SHA-256 为 `0b6681a707b2f9d46c427e1f16f8a696b4d77b21f691152f0afbd9001557a94e`。

## 04-04 断线重选与工作台

2026-11-24，候选 `09b335f8123fdb4f7c9fab36080d0aa5df97c187` 通过多中继原生回归、受管 Chromium、Go 全量测试及 vet。Go 和 M1 全量回归启动后的两项原生夹具改动是将两个中继设为不同区域，以及等待中心确认探测窗口关闭。两项改动均由最终多中继回归覆盖。

| 检查 | 结果 |
| --- | --- |
| 实际断线与重新授权 | 本地 Linux ARM64 `hbbs` 与两个分属不同区域的 `hbbr` 完成首次会话核销和续租。停止实际转发的 `hbbr` 后，双端观察到连接关闭，受控端提交 `connection_lost`，旧授权创建新轮次被拒绝。 |
| 新轮次重选 | 中心三次独立探测失败后摘除故障节点。Rust 双端再次走受管连接入口，使用不同的会话 ID、授权 ID、连接 ID 和轮次，重新采样并经健康节点完成双向数据传输和续租。 |
| 会话详情 | 真实断线与重连的两个会话详情分别显示停止与活动状态，并按各自的核销记录关联实际节点与双端中位数。Chromium 在 375 px 视口验证单次 RTT、超时、选点失败、GeoIP 回退及实际核销，未授权角色在详情接口返回 404。 |

本地 Git 证据目录 `deskrealm-evidence/m3/09b335f8123fdb4f7c9fab36080d0aa5df97c187/`。`multi-relay.log` SHA-256 为 `200660ef617c3d40f50ce3986e5f29b9e6a30f208356b78d9bb5a63d6e2ecf62`，`managed-browser.log` 为 `2d025ac137faec7189407e59b536bbca53a75f9184d014c3ef9730c4bcbf84c5`，`m1-all.log` 为 `97cf2a4b10fb5d4515e64df6049fd9d0f3251e830bef15c75f573bececee98cf`，`go-vet-test.log` 为 `1b3be2c0b239c5336075ebb80adb1e5b5676634aa380217866554fb067d3c090`。

04-04 开发验证完成。重连指用户再次发起会话，中心为其签发独立授权。目标环境验收安排在 M0–M3 开发完成后。

## 04-05 Windows 开发包签名

2026-11-24，打包实现 `e830798a8d9326cbf5a3c32eb1a1b6dca60aac5f` 通过 `bash scripts/deskrealm-m3-test signed-build` 的 14 项测试，并通过 Go 全量测试与 vet。

| 检查 | 结果 |
| --- | --- |
| 来源与模板 | 两架构包绑定原生构建提交、证据清单和内层客户端摘要。品牌版本、ICO 摘要、中心公开配置与目标组均进入批准记录，交付包内的配置仅携带公开信息。 |
| 签名与隔离 | 打包环境生成清单，独立 OpenSSL Ed25519 签名进程仅持有清单和开发签名材料。验证器从独立提供的批准记录和开发公钥核对签名及两套归档。 |
| 失败与重试 | 测试覆盖错误源码、修改构建证据、跨架构替换、重新计算外层摘要、错签名、错公钥、违规归档路径和配置中出现凭据字段。签名不可用时无签名输出，重试复用原候选。替换失败保留先前候选。 |
| 实际开发工件 | x64 与 ARM64 使用 `c37fde79c99c840cfc234b8e17e9fd15499665af` 已验证的历史原生工件。重新打包产生相同 ZIP 和清单，独立验签通过。 |

开发 ZIP 的 SHA-256 为 x64 `96441bacf6a1bb1f69d6138de902c7d24d1fe5f74f459f5271581e5aec1563d0` 和 ARM64 `55b4c7c1f9e8b46d9e604742934043b8e55ee97f463f1ab816e3947116804860`。工件、审批输入、开发公钥和证据日志存于本地 Git 目录 `deskrealm-evidence/m3/e830798a8d9326cbf5a3c32eb1a1b6dca60aac5f/`。

当前 Windows 构建的 CI 提交为 `6acfe3f62c3da5bd9d2021865fb3db8ab780da99`，来自单独构建分支。锁定源码和构建输入范围与本地 `e830798` 一致。记录时两架构桥接均通过，原生构建仍在运行。[当前 Windows 构建](https://github.com/nodeox/DeskRealm/actions/runs/45368970255)

本次签名范围为开发 ZIP 的分离签名。Task 1 继续接入中心授权提交、可恢复交付任务及构建证据展示。控制台发布和免填登记仍待开发，Windows Authenticode 安装器与目标环境安装亦待验证。
