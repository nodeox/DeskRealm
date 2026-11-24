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

## 04-05 中心签名验证任务

2026-11-24，候选 `b07570369924e88faf9759e18645fd0e1feb0f25` 接通中心暂存区、授权提交、后台验证及工作台。平台管理员按清单 SHA-256 提交已暂存候选。中心匹配当前部署身份和设备组，后台以单独配置的开发公钥验证签名、两套归档和包内公开配置。

| 检查 | 结果 |
| --- | --- |
| PostgreSQL 任务与授权 | 重复提交绑定同一请求键及清单。拒绝普通成员、错误中心和组外目标。执行前管理员权限变更时不确认包。错误签名和 ARM64 包字节不符返回明确失败，修复后重试可成功。过期执行租约回滚验证状态。品牌修订发生冲突时保留原验证记录。 |
| 真实原生工件互操作 | Go 验证器通过此前 Python 签名的两套原生开发 ZIP。原生构建来源仍为历史提交 `c37fde79c99c840cfc234b8e17e9fd15499665af`。 |
| 工作台与回归 | Chromium 通过暂存清单缺失、正确提交与查看品牌、源码及两架构摘要。现有交付任务回执、失败重试和执行前取消继续通过。M1 全量回归、Vue 类型检查及三个原有 Chromium 场景通过。新增的执行前权限变更用例由最终签名专项覆盖。 |

本地 Git 证据目录 `deskrealm-evidence/m3/b07570369924e88faf9759e18645fd0e1feb0f25/`。四份日志的 SHA-256 分别为 `signed-build.log` 的 `53bb72370ec4656cf8990175a41df2c03264c8d631e37e4b9e6168b7311bf2a8`、`delivery-tasks.log` 的 `b295c402f836337bfc8c4b8dff74105b2618e059b09ac6e1625be50f5924b3d6`、`python-native-bundles.log` 的 `463b8b697dac23646a5df8ff633f8482daa5f3097955be4f10bee29215632001` 和 `m1-all.log` 的 `e40dab798c14cc0fd640b9b7a05013db836a6dc46652f873b01c18aabef46959`。

构建与签名仍由独立操作环境完成，中心负责已暂存的候选验证。本轮 x64 和 ARM64 的 CI 桥接均已通过，原生编译和测试仍在进行。正式安装器、分组发放与免填配置继续开发。[Windows 原生构建](https://github.com/nodeox/DeskRealm/actions/runs/45368970255)

## 04-05 开发包分组发布

2026-11-24，候选 `789a51eb463842ad833deabf29d03ec17d37ffa7` 通过分组发布 PostgreSQL、交付任务 Chromium、签名构建、M1 全量和历史原生工件持久化验证。中心在生成发布记录前重新验签，并将两个架构按清单摘要保存到独立持久目录。创建发布、授权下载和撤回均有审计事件。

| 检查 | 结果 |
| --- | --- |
| 发布与替换 | 并发发布同一清单返回同一发布 ID，并只写一条发布审计。未验证任务和修改过签名的后续候选被拒绝，先前发布继续下载。暂存归档变化后，持久归档仍可通过中心新处理器下载。 |
| 下载与撤回 | 当前设备组读取委派决定列表和直接链接的访问范围。未登录、组外、授权撤销、未知架构和已撤回的链接被拒绝。目标组的 x64、ARM64 全包及 Range 检查通过。 |
| Chromium | 从任务详情确认发布，在 Windows 发布页下载 ARM64 ZIP，文件 SHA-256 与页面一致。撤回后下载入口消失。 |
| 原生归档 | Go 重新验签并持久化源提交 `c37fde79c99c840cfc234b8e17e9fd15499665af` 的两套历史工件。本轮 CI 构建输入的提交 `6acfe3f62c3da5bd9d2021865fb3db8ab780da99` 的两架构桥接已通过，原生编译和测试仍在运行。 |

本地 Git 证据目录 `deskrealm-evidence/m3/789a51eb463842ad833deabf29d03ec17d37ffa7/`。日志 SHA-256 为 `managed-release.log` 的 `638f63b5408cb69170442ec73d699fd4d949b502e6e082fc12fb1b996661d565`、`delivery-browser.log` 的 `f421b972b7ea446aecb380174df659526dec3465d233cf76739c495351d7144e`、`signed-build.log` 的 `44039f6b69d7dc9ddb6a4e648723c2818de6662044efd0459d7b04def1f4ea6d`、`historical-native-bundles.log` 的 `51c41d3382d386fac1e577022c0b984129f715e0c2b3127b0dea4242768bad1e` 和 `m1-all.log` 的 `87de1b3304a352e1dfd65c5cc628b00ce8a72db2d451dfac9b3aa356c34bb222`。

本次验证的授权下载使用控制台会话，签名范围为开发 ZIP。原生 CI 工件核验、Windows Authenticode 安装器、部署工具限时凭据及免填登记继续开发。Linux 部署和双 Windows 真机验收仍安排在开发完成后。[Windows 原生构建](https://github.com/nodeox/DeskRealm/actions/runs/45368970255)

## 04-05 限时部署凭据

2026-11-24，候选 `3311e12a7ca9729c966f5e27fa43b4d8eba9314c` 接通中心发放、撤销、部署查询与现有设备登记。部署凭据关联已发布版本、批准组、架构、RustDesk ID 和目标设备公钥。持久化摘要用于中心验证。保留原中心受管签名种子即可在原发放人重新提交相同部署时恢复有效凭据。开发包签名继续使用独立材料。

| 检查 | 结果 |
| --- | --- |
| 发放权限与重试 | 拒绝只读成员、未发布组及普通组审批人申请自动审批。重建 API 处理器并重新加载同一受管种子后，原发放人可恢复相同凭据及到期时间。替换设备公钥的同一请求被拒绝。 |
| 登记与撤销 | 错误设备公钥、RustDesk ID、架构、组及签名均被拒绝。凭据过期、撤销单台凭据、撤回发布或撤销发放人审批权均阻止完成登记，已发出的挑战也受此约束。 |
| 审批与策略 | x64 测试对象进入待审批，目标组审批人可批准。ARM64 测试对象按管理员授权自动审批。重复完成返回原始登记回执。测试客户端经设备运行时认证获取组策略并签名上报，中心部署状态查询显示所应用版本。 |

首次浏览器回归有一次现有凭据表单超时。设备组加载过程中，界面允许点击，但处理函数会忽略点击。候选 `28ea2739ad9d8aebb504673661b14a8f9b6d1bff` 将对应按钮在请求期间设为禁用。该候选通过限时部署凭据专项、M1 Go 回归、Vue 类型检查及三个 Chromium 场景。

本地 Git 证据目录 `deskrealm-evidence/m3/28ea2739ad9d8aebb504673661b14a8f9b6d1bff/`。日志 SHA-256 为 `managed-release.log` 的 `db9773b3a7b6460adccb5d3e61cdd0840e3e76fab17beed304c26f7fa98718b0` 和 `m1-all.log` 的 `52bbf44861a39f7e1b09fa74dab14eb329a572a85894d0cfeb0f2419f059b2be`。

本轮通过 API 和 PostgreSQL 模拟两种 Windows 架构的端点登记。Windows 安装器、终端读取公开配置及免填登记继续开发。实际 Windows 部署和 Linux 中心重建验收留在 Phase 5。

当前原生 CI 提交 `6acfe3f62c3da5bd9d2021865fb3db8ab780da99` 的完整 Windows 构建输入与候选 `3311e12` 一致。x64 原生构建和测试通过，已下载归档并复核 PE 架构、工件摘要及具名测试日志。`manifest.json` SHA-256 为 `f86c691c6e3d8a2c5f6d370005aaffbc4b861ec223b0098d472f603a85bf1a5d`，`client.zip` SHA-256 为 `9a0132cc24695dcf8ea92478e5a4c2560ef4024a2db529be8c91f135dac8a8cb`。核验日志保存在 Git 证据目录 `deskrealm-evidence/windows/6acfe3f62c3da5bd9d2021865fb3db8ab780da99/verify-x64.log`。ARM64 桥接已通过，原生编译仍在运行。[Windows 原生构建](https://github.com/nodeox/DeskRealm/actions/runs/45368970255)

## 04-05 Windows 首次启动程序

2026-11-24，候选 `737454d480be763754aeb9db2262dd36e28d5be8` 将 `DeskRealm.exe` 纳入双架构原生归档验证。启动程序从同目录读取公开配置，通过 Windows 信任的 HTTPS 核对中心签名身份。设备密钥保留在当前用户的本机目录，部署工具写入单台设备凭据。程序完成持钥登记后保存回执，清理部署文件，再用已配置的中心、会合、中继和设备身份启动现有受管客户端。

| 检查 | 结果 |
| --- | --- |
| 启动与恢复 | Go 测试覆盖保留原密钥、拒绝不同架构和中心签名身份、中心已完成登记但响应中断时复用已保存挑战、下次启动沿用登记回执，以及清理继承的中心环境变量。 |
| 中心与交付 | PostgreSQL 发布、登记、审批和策略回执通过。公开配置解析拒绝凭据字段及非规范中心地址。双架构证据和签名打包的 14 个 Python 测试与中心签名验证专项通过。全 Go 检查和 actionlint 通过。 |
| 双架构开发构建 | macOS 交叉编译产生 x64 和 ARM64 启动程序并复验 PE 架构。新 CI 源提交 `71079b6e5d24cea6f018eb41cab0c5b121ab55a6` 的 Windows 构建输入与本地候选一致。[新 Windows 原生构建](https://github.com/nodeox/DeskRealm/actions/runs/45380636466) |
| 上一轮原生归档 | `6acfe3f` 的双架构 CI 与本地工件核验均通过。证据为加入启动程序前的原生客户端。[上一轮 Windows 原生构建](https://github.com/nodeox/DeskRealm/actions/runs/45368970255) |

证据保存在 Git 目录 `deskrealm-evidence/m3/737454d480be763754aeb9db2262dd36e28d5be8/`。双架构交叉编译日志 `launcher-cross-build.log` 的 SHA-256 为 `20184bcf8964f597dee44280f2a292d9aea6f6de6a0865911db3cb10fbb1f8ee`，`launcher-go.log` 为 `a9708e382f8dcaf29e63f96e58375c52c48f27da873aaac08d7e61dff70f5d00`，`managed-release.log` 为 `db9773b3a7b6460adccb5d3e61cdd0840e3e76fab17beed304c26f7fa98718b0`，`signed-build.log` 为 `a3a4ca8db559dd3ea89ae476afaef0ed2ae6343b97032b216fdba5edf072eb1d`。

开发包的启动入口已约定从已验证归档手动解压布置。签名安装器、含启动程序的原生 CI 工件验签和目标环境的首次启动、直连及强制中继验证继续推进。

## 04-05 Authenticode 开发安装器

2026-11-24，候选 `35a2a4e02359f0e82c545ee735148a478b387abe` 支持从批准 ZIP 构建 x64、ARM64 自包含安装器。构建端复核原清单签名、归档摘要及内部 PE 架构。独立核验端重建未签安装器，核对签后原始映像、SHA-256 Authenticode、批准 CA 和发布者证书指纹。

| 检查 | 结果 |
| --- | --- |
| 实际签名 | Go 交叉编译的两个 PE 使用隔离容器内的 osslsigncode 2.5 和临时开发 CA 签名。独立测试通过两个架构的证书链、证书指纹和映像摘要，并拒绝同 CA 下另一发布者证书、互换的架构包、SHA-1 签名及修改过的映像。 |
| 解压和重复安装 | Go 测试验证原归档及公开配置按内容摘要安装，重复安装沿用同一目录并核对已有文件，拒绝路径穿越、Windows 保留名称、重名、覆盖公开配置及本地文件变更。 |
| 支持范围 | 开发验证使用合成客户端归档和真实 Authenticode 签名。Windows 安装器使用系统 WinTrust 后才解压，并在完整解压后更新开始菜单入口。Windows 上的系统调用及首次启动仍待运行验证。 |

中心分组发布当前提供 ZIP。含启动程序的原生 CI 已通过两架构桥接，原生构建和验证还在运行。下一步将签名安装器证据及分组下载接入中心。 [Windows 原生构建](https://github.com/nodeox/DeskRealm/actions/runs/45380636466)

2026-11-24，候选 `1f0b09286ffe6dc335de2706eabbb324bf20b29a` 使用临时 CA 和非生产发布者证书完成 Authenticode 用例。核验通过 x64、ARM64、同受信 CA 另一张证书拒绝、安装器互换拒绝、SHA-1 摘要拒绝和归档摘要复验。Go 全量测试通过。真实签名校验日志 `deskrealm-evidence/m3/1f0b09286ffe6dc335de2706eabbb324bf20b29a/windows-installer.log` 的 SHA-256 为 `6f60dc88c7e646c9205fb3bccbffdd8330b687586941d97c866dbbc0c9936500`。正式安装器签名须使用组织发布者证书并配置 RFC 3161 时间戳。

含启动程序的 Windows CI 候选 `71079b6` 的 ARM64 原生归档已下载并通过本地复验。该归档 `client.zip` 的 SHA-256 为 `51533d6bb73589d88d9f7cab9c9b47d807108b80442784848696f64dc5971a53`。x64 原生构建仍在运行。Go 全量检查和 14 项 Python 构建与打包回归通过。当前提交的 PostgreSQL 专项复跑因本机 Docker 无响应中断。

## 04-05 安装器分组发布

2026-11-24，候选 `5d273db895a54365dd7319d2f275bca942089697` 将双架构安装器接入中心发布。独立环境先验证原 ZIP、Authenticode 签名、证书链和安装器文件，再用专用 Ed25519 密钥签署证据。中心验证证据签名、批准的证书指纹、ZIP 发布清单及实际工件摘要。按证据摘要保存安装器和分离签名，发布及下载使用原 ZIP 的设备组和撤回状态。

| 检查 | 结果 |
| --- | --- |
| PostgreSQL 分组发布与登记 | 正常、并发同证据重试、处理器重建后的下载、分组权限及撤回通过。拒绝未经授权的发放人、无 CSRF、伪造签名、错误证书、错误 ZIP 发布、换包和已发布文件篡改。既有登记专项复跑通过。 |
| Chromium | 从已通过复验的任务发布 ZIP，从 Windows 发布页提交安装器证据，下载 ARM64 EXE。浏览器记录的文件摘要与控制台一致。撤回发布后 ZIP 和安装器下载入口均消失。 |
| 原生工件与真实签名 | 含启动程序的原生源提交 `71079b6e5d24cea6f018eb41cab0c5b121ab55a6` 两架构 CI 和证据验证通过，下载后本地复核通过。使用这两套原生工件重新生成开发 ZIP 和安装器，临时开发 CA Authenticode 签名通过，Python 生成的签名证据与安装器由 Go 校验并持久化。[Windows 原生构建](https://github.com/nodeox/DeskRealm/actions/runs/45380636466) |

证据保存在 Git 目录 `deskrealm-evidence/m3/5d273db895a54365dd7319d2f275bca942089697/`。`managed-release.log` SHA-256 为 `db9773b3a7b6460adccb5d3e61cdd0840e3e76fab17beed304c26f7fa98718b0`，`delivery-tasks.log` 为 `8d3dabe18dd637c43e7363a4942d5477ccd10c076fda0443ba16d4461788e414`，`windows-native-installer.log` 为 `c356e0396251d95ada1609edff860579503212178190b88002397c9d5469178b`。`go vet`、Go 全量包测试和 Vue 类型检查通过。

下一步接通组织部署工具的限时凭据交付。实际 Windows 安装、双端连接和 Linux 中心重建按 Phase 5 验收。

## 04-05 组织部署工具

2026-11-24，候选 `6833e931fc28c56625c211e6391d36b0b0fd5b01` 接通组织部署工具。控制台生成单设备组部署令牌。工具通过 HTTPS 用该令牌下载已发布安装器并验证摘要，使用目标机准备的公钥发放单设备限时凭据，将凭据写到私有文件。安装器支持机器可读的安装目录输出。

| 检查 | 结果 |
| --- | --- |
| 中心与 PostgreSQL | 越组、读取账号令牌访问部署 API、同组不同令牌访问已有部署、无审批权限和要求自动审批均被拒绝。撤销部署、令牌撤销及到期能阻止完成登记。原部署 ID 及相同输入重试返回原凭据和到期时间。 |
| 持钥登记及策略 | 组织工具凭据进入待审批。审批后设备领取预期组策略并签名提交回执，部署状态显示预期和已应用版本。 |
| 工具及控制台 | HTTPS 测试通过摘要校验、私有凭据落盘、同文件重试、标准输出清除敏感字段、查询和撤销。Chromium 通过设备组令牌签发和撤销，既有账号读取令牌通过回归。 |
| 工件 | 使用两套已经复验的原生客户端工件，对当前源码构建的安装器复跑 Authenticode 开发签名和 Python 到 Go 的证据持久化。 |

证据保存在 Git 目录 `deskrealm-evidence/m3/6833e931fc28c56625c211e6391d36b0b0fd5b01/`。`managed-release.log` SHA-256 为 `db9773b3a7b6460adccb5d3e61cdd0840e3e76fab17beed304c26f7fa98718b0`，`bootstrap-browser.log` 为 `ac75ce903243e09a155d784d16381454153cf7662116d81e2fee76a50478a9e1`，`windows-native-installer.log` 为 `92a084ed5bc0a493e63d19ddf85424394966ff4d48298f1e996a9297aac2eff3`，`signed-build.log` 为 `9ec7da04c9016c55320ca0de94010ee13777b3816c53869312fe6f918da4ec48`。Go 全量包、授权专项、命令行竞态测试和 Vue 类型检查通过。

04-05 两项开发验证完成。组织侧继续通过已有设备管理通道交付安装器和单设备凭据，目标 Windows 上的完整安装及连接过程在 Phase 5 验收。

## 04-06 中心备份和隔离恢复

2026-11-24，候选 `46d294abc34964a2363099cc9e0f06648eca1a3c` 添加 `deskrealm-install backup` 和 `restore-check`。中心主机操作员通过已记录的 Compose 部署采集 PostgreSQL 18 物理备份、WAL、两套身份密钥、TOTP 加密材料及标准部署配置。恢复命令使用外部保存的清单摘要核对全部成员。临时容器无网络和公开端口，使用 PostgreSQL 原生校验、实际启动和应用角色查询检查恢复结果。恢复报告与目录一并保存。

| 检查 | 结果 |
| --- | --- |
| PostgreSQL 恢复与重启 | 实际 Compose 容器中运行中心迁移并写入用户、设备、策略、任务及审计样本。恢复检查通过 WAL 校验和正常启动，保留两套公钥、原系统标识、业务记录 UUID 和策略版本。恢复目录可沿普通安装器数据库流程重启。 |
| 故障注入 | 独立清单摘要不一致、同尺寸归档篡改、缺受管签名种子和试图覆盖原部署均被拒绝。失败目录未发布。备份后源库新增审计记录留在源库。 |
| 安装器回归 | Go 全包测试、`go vet`、安装预检、私有材料与准备阶段的稳定身份检查通过。 |

证据保存在 Git 目录 `deskrealm-evidence/m3/46d294abc34964a2363099cc9e0f06648eca1a3c/`。`center-restore.log` SHA-256 为 `163b82cf03fb1b447fb808f11cf22d19f4552a926b739c5b7c6a490a7d65d266`，`go-install-regression.log` 为 `d5701358e6692c861b398ec93a25d0a4b23b5051be7fc4ed2ee8c735951d80c6`。

隔离开发检查覆盖标准中心部署及 PostgreSQL，外部代理和附加挂载按各自部署流程保留。升级、回切及发布批次仍在 04-06 开发范围内。

## 04-06 中心 API 升级和兼容回滚

2026-11-24，候选 `7b38d30ad1a1373c598fc69ab960591ad71c5aa5` 添加 `deskrealm-install upgrade` 和 `upgrade-status`。升级覆盖在线、同架构、同数据库结构的中心 API 镜像。切换前绑定已经隔离恢复的备份、原镜像和数据库结构。候选使用不可变摘要，升级和回滚均只更换 API 容器。状态落盘后可按原候选摘要继续。

| 检查 | 结果 |
| --- | --- |
| PostgreSQL 18 和隔离恢复 | 恢复副本与源库的系统标识、迁移版本和结构摘要一致。直接修改表结构后，迁移版本保持原值且旧结构的恢复报告被拒绝。 |
| 切换和故障演练 | Linux ARM64 临时 Compose 使用本地仓库中的摘要镜像。缺恢复报告或候选声明的迁移版本不符时拒绝切换。候选启动失败回切原摘要，健康候选切换后保留管理员状态、两类公钥及 PostgreSQL、迁移和会合容器 ID。 |
| 重试或恢复 | 重复提交返回原记录。状态机测试覆盖恢复中断的回滚以及结构改变后停止中心并进入 `recovery_required`。升级后的备份和隔离恢复保留活动镜像记录。 |
| 安装器回归 | Go 全量包竞态测试和 `go vet` 通过。 |

候选的 `bash scripts/deskrealm-m3-test restore-upgrade` 完整复跑通过。首次门禁尝试的隔离恢复曾在业务记录检查阶段失败一次，随后启用诊断的四次专项恢复及完整门禁复跑均通过，原因未复现。

证据保存在 Git 目录 `deskrealm-evidence/m3/7b38d30ad1a1373c598fc69ab960591ad71c5aa5/`。`restore-upgrade.log` SHA-256 为 `d869d2143babdd626169c16c86d3f19fd38190282e63fd18c5aceffbf3e5b030`，`go-regressions.log` 为 `8a4e3728de36b60b9a6787c4e529112675c54fca6ea9b385ec0c9c993eb72e32`。首次失败及专项重试的日志分别为 `restore-upgrade-attempt-1.log` 和 `restore-repeat-diagnostic.log`。

数据库迁移、离线中心升级、中继排空升级和 Windows 终端分批发布各由对应流程处理。04-06 接续中继排空升级，目标 Linux 与双 Windows 的验收顺序沿用 Phase 5。

## 04-06 区域中继排空升级

2026-11-24，`3f29bc9ac953f0580a8542fa46a3c4dcc16c09b6` 在区域中继详情中接入按候选二进制 SHA-256 授权的一节点升级任务。中心以事务锁定排空状态并阻止并发恢复。配对票据到期、中心无有效会话租约、公布地址和本机端点均验证零转发后，节点才切换。候选及原二进制在节点私有目录按摘要持久保存。切换和回滚结果都需原身份签名健康与中心独立探测，并关联任务和审计。终态后由管理员恢复入池。

| 检查 | 结果 |
| --- | --- |
| PostgreSQL 中心状态 | 普通成员和不同节点的签名均不能操作。候选摘要不符、票据未到期、有效租约、仍有转发、旧健康回执和公布端点探测失败分别阻止对应步骤。已建立会话可继续续租，重复签收只写一个终态审计。 |
| Chromium 控制台 | 通过区域中继授权、待领取任务取消、锁定期间隐藏恢复和终态保持排空。既有登记、排空、停用、恢复和地址切换回归通过。 |
| 中继宿主机 | 本机文件摘要不符或端口由其他进程占用时拒绝启动候选。 |
| 双 Linux ARM64 中继 | `4732ce1d6681dae07683069e45b4c8b023e7a221` 用两个运行锁定源码构建的 `hbbr` 实例验证。宿主机测试启动器注入失败候选并确认原摘要和签名验活，兼容候选切换后重启保留状态和原公钥。同一测试的第二节点通过独立探测。首次试跑修正了测试夹具的模拟探测刷新与两次排空的总时限。 |
| 连接与安装器回归 | 沿用既有 `deskrealm-relay run` 方式的登记及双节点探测、中继候选选择、独立签名配对、Go 包竞态测试、`go vet` 和 Vue 类型检查通过。 |
| 中心恢复回归 | `6b4b257350a7d966383c4fe2f5e08abf72e485b6` 在隔离恢复中等待 PostgreSQL 真正接受连接。诊断复现了 WAL 回放期间 `pg_ctl` 已返回但禁用了热备查询的现象。修复后四轮带竞态检查的物理备份恢复通过。 |

本地证据位于 Git 目录 `deskrealm-evidence/m3/` 下各源码提交目录。`4732ce1` 的 `relay-upgrade.log` SHA-256 为 `ac8e2ea875f4b22e2cda2e8689f4f97669d949947b66a3a39c2456cb62bef8e6`，`relay-enrollment.log` 为 `563ed90a62061b5e7ec283f8cd7395fdd2f84b372f6618f888177f5807df1d06`。`3f29bc9` 的 `relay-lifecycle.log` 为 `985c005d93f1c8a51b391743991fa8eb905adb6873b99b30e804e6fd2d4c9673`。`6b4b257` 的 `center-restore-repeat.log` 为 `1f19a922a55614924ce7494176d0aa83cc47420b45171a46a1617631377b0f99`。

升级覆盖与已有配置和会话协议兼容的同构建版本二进制。中心采用迁移 29。接续 Windows 终端分批发布和失败暂停，目标 Linux 和 Windows 环境验收留待 Phase 5。

## 04-06 Windows 分批发布

2026-11-24，候选 `5c336120e166460a9865a0e8e95d19bb7ccb2c51` 接入已登记 Windows 设备分批更新。中心采用迁移 30。平台管理员发布仅通过批次分发的版本，按组和设备 UUID 创建任务。部署工具使用分组令牌领取当前批次的签名安装器。终端通过组织现有设备管理渠道安装，继续使用原设备密钥。管理员根据下载授权、同身份的安装包摘要心跳和策略状态核对设备结果。当前批有失败时保留后续批的等待状态，重试保留已成功设备和过往尝试。指定原签名发布后，可核对失败设备回滚到故障前的包。

| 检查 | 结果 |
| --- | --- |
| PostgreSQL 分批发布 | 拒绝组外设备及分组令牌、普通成员、重复请求键的不同内容、普通下载入口和提前领取后续批。通过 x64 与 ARM64 签名安装器元数据绑定、同请求去重、失败暂停、重试保留成功设备、回滚后新心跳核对以及已有心跳和超时隔离。 |
| Chromium | 发布仅显示批次入口，分批任务提供批次、设备结果和旧版恢复信息。暂停时下一批操作不可用。 |
| 构建及静态回归 | Go 部署工具、启动器与安装器均交叉编译到 Windows x64 和 ARM64，核对 PE 架构。锁定 Rust 源码成功应用变更并在 Linux 构建互操作测试程序。Go 竞态、`go vet` 和 Vue 类型检查通过。 |
| 实际 Authenticode 签名 | `ca17281329d8c8854f3072be171f149f05ab1d7e` 修复 osslsigncode 2.5 输出缩进导致算法记录漏读的问题。用临时开发 CA 为两架构夹具安装器签名，验证实际签名并确认拒绝 SHA-1 和不匹配证书。 |

`5c33612` 的本地证据保存在 Git 目录 `deskrealm-evidence/m3/5c336120e166460a9865a0e8e95d19bb7ccb2c51/`。`windows-rollout.log` SHA-256 为 `966c8f3edc76fc33451b1a8bfa5398014097cda149933f4975c54352e3e4a80a`，`windows-go-cross-build.log` 为 `5b6becd1ff32eae1ee1b9608156f090c62527937fa956af6d6b6149df1d8ed3d`，`rust-linux-compile.log` 为 `4c4902ec384c6d70e0e38fe1ca1d2899c3e7bb9062d30fc9e284503d5c8e722c`。

`ca17281` 的 `windows-installer.log` SHA-256 为 `60cb517e0700a7943e59cfdc7bef16813b5248d994b0fc96354dc2ec71015eec`。

现行代码通过现有组织渠道部署，不包含无人值守自更新。带新心跳字段的 Windows Rust 客户端未在本轮生成原生安装包。目标 Linux 及双 Windows 的验收仍由 Phase 5 按原顺序执行。04-06 接续授权样本迁移与 M0–M3 聚合开发门禁。
