# DeskRealm M2 受管协议与构建基线

## 源码准备

从 DeskRealm 仓库根目录运行。Git、Python 3、Go 和 Docker 用于开发检查。Windows 可在安装 Git 后通过 `py -3` 运行 Python 脚本。

```sh
python3 scripts/deskrealm-managed-source prepare ../deskrealm-managed-upstream
python3 scripts/deskrealm-managed-source verify ../deskrealm-managed-upstream
bash scripts/deskrealm-m2-test sources
```

目标的父目录需已存在，目标目录由脚本创建。`prepare` 从远端取发布标签，再核对本地 HEAD、标签和递归子模块的 Git 提交及来源。借助临时目录完成检查后才移动到目标。重复准备时使用新目录。`verify` 核对已有的干净基线，适合在应用补丁和开始构建前运行。

使用 `prepare <目录> --apply-patches` 在新检出的基线上顺序应用补丁。补丁按文件名升序放在 `managed/patches/<client|server>/root/`。子模块补丁放在对应的 `submodules/<子模块路径>/root/`。补丁应同时通过 index 和工作树检查。源清单保持原始 Git 对象，受管补丁不得修改 gitlink 或子模块配置。03-04 将交付实际受管补丁。

客户端、服务端及各自的 `hbb_common` 由 [upstream.lock](../managed/upstream.lock) 固定。M0 的 RustDesk 镜像摘要和会合密钥继续使用试点配置。M2 的中心会话签名以单独的持久密钥和 `kid` 配置，后续服务从可信配置载入密钥及允许使用的验证公钥。

## 签名帧 v1

签名采用 RFC 8032 Ed25519 的普通模式和 64 字节分离签名。信任来源决定验签公钥。中心通过已配置的 `kid` 选择信任钥，设备以登记保存的当前公钥为准。签名输入按下列字段顺序构造。每个字段只编码一次，字段或签名的传输封装由实现端逐字段求值后重新编码。任何宽度、版本、枚举或字段数量错误导致请求失败。 [RFC 8032](https://www.rfc-editor.org/rfc/rfc8032.html)

帧以七字节 `44 52 2d 4d 32 00 01` 开始，随后是一字节消息种类。每个字段为两字节无符号大端长度加原始字段字节。UUID 为 16 字节，按规范文本去除连字符后的字节顺序。摘要和随机数为 32 字节。时间为 Unix 毫秒，版本计数与时间值使用正值，表中标明两字节的字段除外，其余整数长度为 8 字节。整数按无符号大端编码，8 字节整数上限为有符号 64 位整数的最大值。标识由小写 ASCII 字母或数字开头，后续可加下划线或连字符。

| 种类 | 签发者 | 按顺序签名的字段 |
| --- | --- | --- |
| `01` 设备运行挑战 | 已批准设备 | 组织 UUID、设备 UUID、挑战 UUID、中心随机数、设备密钥版本、挑战到期、两字节协议兼容版本 |
| `02` 用户与设备绑定 | 主控设备 | 组织 UUID、设备 UUID、用户 UUID、绑定 UUID、中心随机数、中心签发的不透明登录流程标识的 SHA-256、设备密钥版本、挑战到期 |
| `03` 会话授权 | 中心 | `kid`（1–32 字节）、组织 UUID、会话 UUID、凭据 UUID、主控用户 UUID、主控设备 UUID、受控设备 UUID、主控当前公钥 SHA-256、受控当前公钥 SHA-256、受控端已应用策略修订序号、一字节操作掩码、一字节确认模式、用途标识（1–64 字节）、签发时间、到期时间 |
| `04` 连接证明 | 连接一方 | 一字节证明方角色、授权摘要、会话 UUID、连接 UUID、本次加密通道绑定摘要、主控随机数、受控随机数、一字节路径 |

操作掩码从最低位开始依次为画面、键鼠、音频、剪贴板、文件传输，至少请求一项。确认模式 `01` 为有人值守，`02` 为经批准的无人值守。连接证明角色 `01` 为主控，`02` 为受控。路径 `01` 为实际直连，`02` 为实际中继，未知路径在证明前保持未确认。两端随机数必须独立。授权摘要为验证通过后同一签名帧与 64 字节签名直接拼接的 SHA-256。通道绑定摘要来自本次已认证加密连接的实际协商，具体传输适配和一致性证明由 03-04 交付。

中心在同一事务校验有效身份、钥匙版本、期限和首次消费。挑战或绑定重提需与原请求匹配。会话凭据最长 60 秒，只能核销给一个连接 UUID。租约初值为 30 秒，正常每 10 秒尝试续租，受控端在单调时钟截止时停止控制并根据实际停止报告结果。客户端不得把未收到的回执显示为已确认。

| 错误分类 | 语义 |
| --- | --- |
| 无效帧 | 格式、版本或字段非法，拒绝当前请求。 |
| 认证失败 | 缺少有效身份、密钥或签名，返回对外一致的认证错误。 |
| 权限拒绝 | 当前主体或连接条件未获准，范围外资源按不可见处理。 |
| 状态冲突 | 授权同键异载荷或版本已变更，要求按入口重取。身份挑战的重放返回统一认证失败。 |
| 中心不可用 | 无法安全签发、核销或续租，保持拒绝新会话及现有租约的截止时间。 |

协议夹具 [vectors.json](../managed/tests/vectors.json) 包含设备挑战、用户绑定、中心授权、直连主控证明和中继受控证明的签名，以及无效格式用例。所有测试种子仅供夹具使用。Go 使用标准库 Ed25519，Rust 测试使用与上游同版本的 `sodiumoxide`，测试和环境均通过锁文件或镜像摘要固定。运行 `bash scripts/deskrealm-m2-test vectors` 可单独复跑。端点实际加密通道、准入、操作执行与租约回执将在后续计划取证。

## 设备运行身份与主控绑定

在中心部署配置中设置 `DESKREALM_SESSION_SIGNING_KEY_FILE` 和 `DESKREALM_SESSION_SIGNING_KID`。密钥文件存放由安全随机数生成的 32 字节 Ed25519 种子，使用无填充的 Base64url，一行表示。用部署用户权限保护并备份同一份文件，重建时复用文件和 ID。两个设置均为空时，身份签发入口和签名公钥入口返回 503。配置不完整或密钥格式非法时启动报错，错误不包含文件路径和内容。

以下写入使用 JSON。公钥、签名、随机数及 `flow` 使用无填充 Base64url。`02` 帧的登录流程摘要取 `flow` 解码后的原始 32 字节。认证返回中的 `expires_at` 为 RFC 3339 时间，签名输入使用挑战返回的整数 `expires_ms`。

| API | 请求、验证及结果 |
| --- | --- |
| `GET /v1/managed/signing-key` | 通过预配置的 HTTPS 中心返回 `kid`、`public_key`。中心的会话授权签发及轮换链路由后续计划接入。 |
| `POST /v1/managed/devices/challenge` | 提交 `device_id`、`protocol_version`。当前支持版本 1，仅已批准设备取得 60 秒挑战。相同在途挑战的重试返回同一结果。 |
| `POST /v1/managed/devices/complete` | 提交 `device_id`、`challenge_id`、`signature`，使用当前已注册设备密钥签种类 `01`。一次核销后返回 `credential`、`expires_at`，运行凭据前缀为 `drd_`，有效期 15 分钟。签发结果丢失时重新申请挑战。 |
| `POST /v1/managed/users/challenge` | 使用已有登录 Cookie、同源 Origin 和 CSRF 值，提交 `device_id`、`device_credential`。返回额外的 `user_id` 和 `flow`，绑定同一设备凭据和该次登录。 |
| `POST /v1/managed/users/complete` | 使用同一次登录及 CSRF，提交 `device_id`、`challenge_id`、`signature`，签种类 `02`。返回 `dru_` 主控凭据，有效期最长 15 分钟，且不超过设备凭据或登录会话的截止时间。 |
| `GET /v1/managed/users/session` | 以 `Authorization: Bearer <dru_凭据>` 验证用户、设备、组织与协议版本。退出登录、账号停用或设备失效后拒绝。 |
| `POST /v1/managed/devices/heartbeat` | 以 `Authorization: Bearer <drd_凭据>` 提交同一 `device_id`、`reported_build`。成功返回 204。中心以自身接收时间记录在线，45 秒无合格心跳后台账和概览显示离线。 |

设备写入入口使用无 Cookie 的原生请求。设备凭据和主控凭据仅存摘要，并在请求时重查设备批准状态、密钥版本和有效期。主控身份同时重查原登录会话及账号状态。管理 API Token 保持原有用途。心跳中的构建字段是经过设备身份认证的自报值，未来由受信发布来源参与会话准入。

## 原生构建入口与证据

受管 Windows 构建沿用固定客户端工作流的原生 MSVC 环境。先按工作流生成对应版本的 Flutter bridge 并安装锁定的 vcpkg 依赖。上游配置为 Rust 1.75、vcpkg `9e593bb18ea69cc5095e012465dcd675a822ed0d`，ARM64 用对应版本的原生 Dart 和 Windows engine，并将 `SODIUM_LIB_DIR` 指向 ARM64 vcpkg 库。 [固定 Windows 工作流](https://github.com/rustdesk/rustdesk/blob/95cca3f661f635b3cc85c04935fe8d96d8f432ef/.github/workflows/flutter-build.yml#L157-L339)

| 目标 | 工作流环境 | 客户端目录中的构建命令 | 原生输出目录 |
| --- | --- | --- | --- |
| Windows x64 | `windows-2022`、`x86_64-pc-windows-msvc`、Flutter 3.24.5、`x64-windows-static` | `python3 ./build.py --portable --flutter --skip-portable-pack --hwcodec --vram` | `flutter/build/windows/x64/runner/Release` |
| Windows ARM64 | `windows-11-arm`、`aarch64-pc-windows-msvc`、Flutter 3.44.9、`arm64-windows-static` | `python3 ./build.py --portable --flutter --skip-portable-pack --hwcodec` | `flutter/build/windows/arm64/runner/Release` |

服务端从固定源码准备后，在对应 Linux 构建环境的服务端目录执行 `cargo build --locked --release --bin hbbs --bin hbbr`。工具链和依赖准备参考固定上游镜像构建配方。 [固定服务端构建配方](https://github.com/rustdesk/rustdesk-server/blob/73523b31cfd25d77dee862e6fc9f5e1fb5e485ef/docker-classic/Dockerfile)

开发工件建议保存到 checkout 外的 `evidence/m2/builds/<target>/<deskrealm-commit>/`。每次记录 DeskRealm 提交、上游与子模块 SHA、补丁摘要、编译器及 SDK 版本、运行命令和工件 SHA-256。当前检查只包含运行在 Linux 容器中的 Rust 签名夹具。Windows x64、ARM64 原生制品与 Linux 受管服务端构建留待 03-04，目标环境注册和联网验收留待 Phase 5。
