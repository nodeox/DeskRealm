# DeskRealm M2 受管协议与构建基线

## 源码准备

从 DeskRealm 仓库根目录运行。Git、Python 3、Go 和 Docker 用于开发检查。Windows 可在安装 Git 后通过 `py -3` 运行 Python 脚本。

```sh
python3 scripts/deskrealm-managed-source prepare ../deskrealm-managed-upstream
python3 scripts/deskrealm-managed-source verify ../deskrealm-managed-upstream
bash scripts/deskrealm-m2-test sources
```

目标的父目录需已存在，目标目录由脚本创建。`prepare` 从远端取发布标签，再核对本地 HEAD、标签和递归子模块的 Git 提交及来源。借助临时目录完成检查后才移动到目标。重复准备时使用新目录。`verify` 核对已有的干净基线，适合在应用补丁和开始构建前运行。

使用 `prepare <目录> --apply-patches` 在新检出的基线上顺序应用补丁。补丁按文件名升序放在 `managed/patches/<client|server>/root/`。子模块补丁放在对应的 `submodules/<子模块路径>/root/`。补丁应同时通过 index 和工作树检查。源清单保持原始 Git 对象，受管补丁不得修改 gitlink 或子模块配置。03-04 的协议补丁已经接入源码准备脚本。脚本同时将同一份 Rust 协议实现装入两份固定的 `hbb_common`。

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
| `05` 策略应用回执 | 接收策略的设备 | 组织 UUID、设备 UUID、交付 UUID、策略版本、载荷 SHA-256、设备密钥版本、一字节应用结果、一字节失败字段掩码 |
| `06` 有人值守确认 | 受控设备 | 授权摘要、会话 UUID、连接 UUID、本次通道绑定摘要、一字节本地确认操作掩码 |

操作掩码从最低位开始依次为画面、键鼠、音频、剪贴板、文件传输，至少请求一项。确认模式 `01` 为有人值守，`02` 为经批准的无人值守。连接证明角色 `01` 为主控，`02` 为受控。路径 `01` 为实际直连，`02` 为实际中继，未知路径在证明前保持未确认。两端随机数必须独立。授权摘要为验证通过后同一签名帧与 64 字节签名直接拼接的 SHA-256。通道绑定摘要来自本次已认证加密连接的实际协商，具体传输适配和一致性证明由 03-04 交付。

中心在同一事务校验有效身份、钥匙版本、期限和首次消费。挑战或绑定重提需与原请求匹配。会话凭据最长 60 秒，只能核销给一个连接 UUID。租约初值为 30 秒，正常每 10 秒尝试续租，受控端在单调时钟截止时停止控制并根据实际停止报告结果。客户端不得把未收到的回执显示为已确认。

| 错误分类 | 语义 |
| --- | --- |
| 无效帧 | 格式、版本或字段非法，拒绝当前请求。 |
| 认证失败 | 缺少有效身份、密钥或签名，返回对外一致的认证错误。 |
| 权限拒绝 | 当前主体或连接条件未获准，范围外资源按不可见处理。 |
| 状态冲突 | 授权同键异载荷或版本已变更，要求按入口重取。身份挑战的重放返回统一认证失败。 |
| 中心不可用 | 无法安全签发、核销或续租，保持拒绝新会话及现有租约的截止时间。 |

协议夹具 [vectors.json](../managed/tests/vectors.json) 包含设备挑战、用户绑定、中心授权、直连主控证明、中继受控证明、策略回执及有人值守确认的签名，以及无效格式用例。所有测试种子仅供夹具使用。Go 使用标准库 Ed25519，Rust 测试使用与上游同版本的 `sodiumoxide`，测试和环境均通过锁文件或镜像摘要固定。运行 `bash scripts/deskrealm-m2-test vectors` 可单独复跑。当前还可复跑原生协议和 TCP 通道绑定检查。完整准入、操作执行与租约回执须由实际受管路径取证。

## 设备运行身份与主控绑定

在中心部署配置中设置 `DESKREALM_SESSION_SIGNING_KEY_FILE` 和 `DESKREALM_SESSION_SIGNING_KID`。密钥文件存放由安全随机数生成的 32 字节 Ed25519 种子，使用无填充的 Base64url，一行表示。用部署用户权限保护并备份同一份文件，重建时复用文件和 ID。两个设置均为空时，身份签发入口和签名公钥入口返回 503。配置不完整或密钥格式非法时启动报错，错误不包含文件路径和内容。

以下写入使用 JSON。公钥、签名、随机数及 `flow` 使用无填充 Base64url。`02` 帧的登录流程摘要取 `flow` 解码后的原始 32 字节。认证返回中的 `expires_at` 为 RFC 3339 时间，签名输入使用挑战返回的整数 `expires_ms`。

| API | 请求、验证及结果 |
| --- | --- |
| `GET /v1/managed/signing-key` | 通过预配置的 HTTPS 中心返回 `kid`、`public_key`。授权签名使用该密钥，中心签名密钥轮换由后续计划接入。 |
| `POST /v1/managed/devices/challenge` | 提交 `device_id`、`protocol_version`。当前支持版本 1，仅已批准设备取得 60 秒挑战。相同在途挑战的重试返回同一结果。 |
| `POST /v1/managed/devices/complete` | 提交 `device_id`、`challenge_id`、`signature`，使用当前已注册设备密钥签种类 `01`。一次核销后返回 `credential`、`expires_at`，运行凭据前缀为 `drd_`，有效期 15 分钟。签发结果丢失时重新申请挑战。 |
| `POST /v1/managed/users/challenge` | 使用已有登录 Cookie、同源 Origin 和 CSRF 值，提交 `device_id`、`device_credential`。返回额外的 `user_id` 和 `flow`，绑定同一设备凭据和该次登录。 |
| `POST /v1/managed/users/complete` | 使用同一次登录及 CSRF，提交 `device_id`、`challenge_id`、`signature`，签种类 `02`。返回 `dru_` 主控凭据，有效期最长 15 分钟，且不超过设备凭据或登录会话的截止时间。 |
| `GET /v1/managed/users/session` | 以 `Authorization: Bearer <dru_凭据>` 验证用户、设备、组织与协议版本。退出登录、账号停用或设备失效后拒绝。 |
| `POST /v1/managed/devices/heartbeat` | 以 `Authorization: Bearer <drd_凭据>` 提交同一 `device_id`、`reported_build`。成功返回 204。中心以自身接收时间记录在线，45 秒无合格心跳后台账和概览显示离线。 |

设备写入入口使用无 Cookie 的原生请求。设备凭据和主控凭据仅存摘要，并在请求时重查设备批准状态、密钥版本和有效期。主控身份同时重查原登录会话及账号状态。管理 API Token 保持原有用途。心跳中的构建字段是经过设备身份认证的自报值，未来由受信发布来源参与会话准入。

## 连接规则和预览

平台管理员可通过下列入口管理 ACL。写入及预览需现有登录 Cookie、同源 Origin 和 CSRF 值。编辑产生新版本，显式发布后才用于判定。归档退出判定和活动清单，版本及审计继续保存。

| API | 请求与结果 |
| --- | --- |
| `GET /v1/admin/access-rules` | 返回当前规则的最新草稿、`latest_version` 和 `published_version`。可使用返回的 `next` 作为 `after` 分页。 |
| `GET /v1/admin/access-rules/{id}` | 返回最新版本，可用 `?version=N` 查询该活动规则的历史版本。 |
| `POST /v1/admin/access-rules` | 创建第 1 版草稿。 |
| `POST /v1/admin/access-rules/{id}` | 提交完整规则与当前 `version`，产生下一版草稿。 |
| `POST /v1/admin/access-rules/{id}/publish` 或 `/archive` | 提交当前 `version` 并发布或归档。版本冲突返回 409，同版重复发布保留一个发布事件。 |
| `POST /v1/admin/access-rules/preview` | 提交 `user_id`、`controller_device_id`、`purpose`、`requested_actions` 和一个目标选择方式。响应包含 `allowed`、原因及可用的规则版本。 |

规则包含 `name`、一个来源 `source_user_id` 或 `source_group_id`、一个目标 `target_device_id` 或 `target_group_id`、可选 `controller_device_id`、`purpose` 和 `effect`。允许规则需要 `control_role_id`、`control_role_version` 和 `allowed_actions`，且所选操作均在该版本角色内。拒绝规则匹配来源、目标、主控限制及用途。角色变更后，旧版本引用的允许规则停止授予能力，管理员可编辑并发布引用新角色版本的规则。

预览目标可选 `target_device_id`、`target_rustdesk_id` 或 `book_id` 与 `entry_id`。通讯录入口按被模拟用户实时复查册权限和目标设备可见性。连接判定还检查用户活跃、两端已批准、当前密钥认证心跳及协议版本。任一匹配拒绝优先。允许时须有一条已发布规则和它引用的同版角色完整覆盖所申请动作，后台管理员身份不会跳过 ACL。预览及规则变更写入审计。

预览返回 ACL 判定。会话签发在同一事务评估已认证主控主体的 ACL 和受控设备已应用的策略。受信发布来源准入随原生接入交付。

## 设备策略、灰度与回执

平台管理员发布不可变策略版本。分配优先级为设备、归属用户、主设备组、组织默认。一个范围保留一个分配 ID 和递增修订。灰度按 `SHA-256(分配 UUID || 设备 UUID)` 前四字节的大端无符号整数除以 100 的余数分桶。小于灰度百分比的设备接收灰度版本，其余使用本范围的基线。基线为空时继续查找低优先级范围。重复应用相同分配或灰度参数时，内容未变的设备保留交付 ID。回滚生成新修订并记录来源和目标修订，受影响设备重新回执。

| API | 请求与结果 |
| --- | --- |
| `GET /v1/admin/strategies` | 列出发布版本。可用 `before` 按策略版本分页。 |
| `POST /v1/admin/strategies` | 提交 `name`、`allowed_actions` 和可选 `unattended_purposes`，返回不可变 `policy_version` 和载荷摘要。空动作列表可表示禁止全部动作。 |
| `GET /v1/admin/strategies/assignments` | 列出分配和当前修订。可用 `after` 按 ID 分页。 |
| `GET /v1/admin/strategies/assignments/{id}` | 返回当前修订，使用 `?revision=N` 查历史。 |
| `POST /v1/admin/strategies/assignments` | 提交 `scope` 和 `object_id`，组织范围省略后者。指定基线 `policy_version`，或指定 `canary_version` 及 1 至 99 的 `canary_percent`。 |
| `POST /v1/admin/strategies/assignments/{id}` | 提交当前 `revision` 和完整分配参数。两种策略版本均为空时撤销本范围分配。 |
| `POST /v1/admin/strategies/assignments/{id}/rollback` | 提交当前 `revision` 与 `rollback_to_revision`。版本冲突返回 409。 |
| `POST /v1/admin/devices/{id}/policy-owner` | 提交 `device_version` 和 `owner_user_id`。归属账号应启用，空字符串清除归属。旧设备初始无归属。 |
| `GET /v1/admin/devices/{id}/policy` | 按设备查看权限返回期望和已应用版本、当前回执状态、失败字段及中心接收时间。 |
| `POST /v1/admin/devices/{id}/policy-retry` | 提交当前失败的 `delivery_id`，只重新交付该设备。 |
| `GET /v1/managed/devices/{id}/policy` | 使用该设备的 `drd_` Bearer。返回 `policy` 对象或空值。对象包含交付、组织、设备、密钥版本、策略和载荷字段。 |
| `POST /v1/managed/devices/policy-receipts` | 使用同一设备的 `drd_` Bearer，提交 `device_id`、`delivery_id`、`policy_version`、`payload_sha256`、`result`、`failed_fields` 和签名。 |

策略格式版本 1 的载荷摘要为 `SHA-256`，输入依次为 ASCII `DR-M2-policy-v1`、零字节、一字节操作掩码、字典序排序的无人值守用途。每个用途以零字节结束，用途遵循协议标识格式。策略标题和显示字段不参与摘要。含无人值守用途的版本只能分配到明确指定的设备。

`05` 帧的应用结果 `01` 表示 `applied`，失败字段掩码为零。`02` 表示 `failed`，掩码按从低到高依次记录 `allowed_actions`、`unattended_purposes`、`schema_version`，至少一位有效。签名以当前登记设备密钥生成。中心只接受当前交付的身份、版本和摘要。重复同一结果可重试，过期或冲突回执返回 409。失败回执保留上一已应用版本。会话判定所用策略只取当前交付且经同一密钥版本确认成功的能力，密钥轮换会重新交付。

## 中心会话签发与核销

会话入口使用无 Cookie 的 Bearer 请求。主控和受控端的实际钥匙来自中心设备登记。会合和中继各使用独立的凭据。在中心配置中，`DESKREALM_MANAGED_RENDEZVOUS_TOKEN` 和 `DESKREALM_MANAGED_RELAY_TOKEN` 各为独立随机的 32 字节，使用无填充 Base64url。启用中继时同时配置 `DESKREALM_MANAGED_RELAY_ID`，格式与 `kid` 一致。向会合和中继入口发送 Bearer 时，分别在配置值前加 `drr_` 和 `drl_`。省略中继两项时只支持直连会合。

| API | 认证及结果 |
| --- | --- |
| `POST /v1/sessions` | 主控的 `dru_`。提交 `request_id` UUID、`purpose`、`requested_actions` 和一个目标选择方式，默认 `confirmation_mode` 为 `attended`。首次签发返回 201，同主体同绑定同载荷重试返回 200 和原签发结果。 |
| `POST /v1/internal/managed/rendezvous/authorize` | 会合的 `drr_`。提交 `session_id`、`grant_id`、`grant_signature`、`controller_device_id`、`target_device_id`、`target_rustdesk_id`、`connection_id`、`path`，中继路径另提交 `relay_id`。中心核对授权、两端和调度使用的目标 RustDesk ID，再保存候选路径，成功返回 204。 |
| `POST /v1/internal/managed/relay/paired` | 中继的 `drl_`。以相同字段报告该中继已配对的双方，配对须对应已有的会合候选路径。成功返回 204。 |
| `POST /v1/sessions/{id}/redeem` | 授权目标设备的 `drd_`。提交 `grant_id`、`grant_signature`、`connection_id`、`path`、可选 `relay_id`、`channel_sha256`、双方随机数 `controller_nonce` 和 `target_nonce`、双方种类 `04` 的签名 `controller_signature` 和 `target_signature`。有人值守还需 `confirmed_actions` 及种类 `06` 的 `consent_signature`。 |

签发响应返回种类 `03` 的所有字段及 `signature`。两端公钥摘要和通道摘要使用小写十六进制，随机数与签名使用无填充 Base64url。响应中的 `controller_rustdesk_id` 和 `target_rustdesk_id` 作为 HTTPS 路由元数据返回。中心在会合与核销时复查登记，RustDesk ID 之外的会话身份绑定使用种类 `03` 签名内的 UUID 和公钥摘要。申请动作须先由一条 ACL 完整覆盖，再与当前已应用策略求交。无人值守须由受控设备应用的策略明确允许该用途。有人值守确认的有效动作是授权动作的非空子集。

受控端核销成功返回 `session_id`、`connection_id`、`path`、`relay_id`、`allowed_actions`、`lease_sequence`、`redeemed_ms`、`lease_until_ms` 和 `session_credential`。直连和中继均要求两端对同一次加密通道签名。中继另要求对应的配对记录。核销只允许一个连接 UUID。精确重试在权限和初始租约有效时返回原结果和截止时间。竞争连接或同连接不同有效载荷返回 409。签发、会合和首次核销都会复查当前账号、两端、ACL 和已应用策略。签发及核销结果与审计及 outbox 在同一事务提交。

运行 `bash scripts/deskrealm-m2-test sessions` 可在本机 PostgreSQL 验证中心事务。测试使用两端的签名夹具和独立服务凭据。真实 RustDesk 通道绑定及中继配对上报由 03-04 交付。

## 续租、撤销与停止回执

成功核销后只向受控端返回以 `drc_` 开头的会话凭据。中心将其摘要绑定到已经核销的会话和连接，凭据可用于恢复状态查询和补交回执。设备停用或密钥轮换后仍可用它完成该连接的回执。中心的会话读取、审计和通知正文均省略凭据。

| API | 认证及结果 |
| --- | --- |
| `POST /v1/sessions/{id}/renew` | 受控设备当前的 `drd_` Bearer，同时提交 `connection_id`、`session_credential` 和上一份已取得的 `lease_sequence`。返回递增的序号、`lease_until_ms` 和核销时确定的 `allowed_actions`。 |
| `GET /v1/sessions/{id}/status?connection_id=…` | 连接的 `drc_` Bearer。持久查询可在中心重建或断线后恢复，返回当前状态、已发序号、截止时间和已登记的撤销及停止时间。 |
| `POST /v1/sessions/{id}/stopped` | 连接的 `drc_` Bearer。受控端停止操作后提交 `connection_id`、最后应用的 `lease_sequence` 和 `reason`，原因可为 `local_end`、`revoked`、`lease_expired` 或 `connection_lost`。成功返回 204，同载荷可重试。 |
| `POST /v1/sessions/{id}/file-events` | 连接的 `drc_` Bearer，并要求已核销的文件传输权限。提交 `connection_id`、`lease_sequence`、`event_id` UUID、`direction`（`upload` 或 `download`，以主控方向为准）、`result`（`completed`、`failed` 或 `cancelled`）和非负整数 `size_bytes`。成功返回 204，仅保存这些元数据和中心接收时间。 |
| `GET /v1/admin/sessions` 或 `GET /v1/admin/sessions/{id}` | 登录 Cookie。平台管理员查看组织范围，设备组授权用户查看其组范围，主控用户查看本人会话。列表包含当前范围的总量、活动数和两路核销数。用 `before` 和响应的 `next` 分页。 |
| `POST /v1/admin/sessions/{id}/revoke` | 登录 Cookie、同源 Origin 和 CSRF 值。平台管理员或具有目标组管理权限的用户提交 `reason`（1 至 120 字）。返回当前撤销或停止状态。 |

活动租约只在有效期内续订。事务复查绑定的原登录、账号、两端当前密钥、ACL 和当前已应用策略。短时主控凭据的正常更新保留同一次原登录。权限扩大需重新申请。相同续租请求仅可在最近一次成功仍有效时重试，返回原截止时间。其他旧序号与超前序号返回 409。已撤销、已停止或已到期的会话拒绝续租。

原生端点库从首次发出核销或当前序号续租请求时计算本地截止时间，给 30 秒窗口预留 50 毫秒计时精度余量。运行时钟和计入睡眠的系统时钟任一达到截止时间即停止授权，中心截止时间可进一步缩短窗口。重试保留首次尝试的时间上限。续租响应必须匹配会话、连接和已核销操作，并递增一个序号。收到响应时旧租约必须仍有效。查询状态不会修改本地截止时间。

手动撤销和影响授权的业务变更在同一事务写入失效状态、审计及 outbox。中心查询分别保留撤销请求时间、已跨过的租约截止时间和实际停止回执的接收时间。晚到回执保留早先的到期事实。收到实际回执前，已核销会话的撤销审计保持待回执。续租、撤销和回执的 PostgreSQL 检查入口为 `bash scripts/deskrealm-m2-test leases`。

原生受控端独立发起状态查询和续租。状态查询间隔为 1 秒，单次超时为 2 秒。续租间隔以 10 秒起步，失败时重试原序号。中心暂时不可达时沿用最后一份已应用租约。每 250 毫秒核对本地截止时间和服务开关。检测到系统启动时钟的检查间隔达到 2 秒时暂停权限，重新取得中心活动状态后才恢复，并继续受原租约约束。撤销、到期、主动关闭或断线时清除本地权限，释放流后提交最后应用的序号及原因。回执在当前进程内最多尝试三次。

## RustDesk 原生消息和通道绑定

会合消息在两个锁定的 `hbb_common` 上使用同一 `DeskRealmSignal` 字段号 40。该消息使用 `DeskRealmRoute` 携带中心签名帧、签名、候选连接 UUID 和目标 RustDesk ID，并分别承载直连与中继协调消息。端点的 `Message` 使用字段号 40 携带 `DeskRealmHandshake`。端点先交换受控端签名的临时密钥和密封流密钥，再在加密通道按会话 UUID 和连接 UUID 交换随机数、角色证明和核销结果。会话凭据留在受控端。

TCP 的绑定摘要使用 HMAC-SHA256，密钥为 RustDesk 方向密钥协商中生成的会话秘密。输入按顺序为 ASCII `DeskRealm/M2/RustDesk-channel/v1`、零字节、四字节无符号大端协商公布版本、四字节无符号大端选定版本、发起端临时公钥、响应端临时公钥。两把临时公钥各为 32 字节。原生代码当前只在选定版本 1 的 TCP 流暴露该绑定。流被改为原始模式或装入旧版单密钥时，绑定随之清除。直接 TCP 和经中继原样转发的 TCP 可以使用同一规则。

验签用预先信任的中心 `kid` 和公钥检查标准帧及期限，然后依据授权中的公钥摘要核对端点角色。连接证明使用本地流的绑定摘要和双方随机数。改变连接 UUID、通道摘要、随机数或路径均使原证明失效。有人值守确认由受控设备签名，确认动作只能取授权动作的非空子集。

`bash scripts/deskrealm-m2-test native-wire` 从锁定源码重放补丁，交叉解码两端的 protobuf，运行 Go/Rust 向量及原生 TCP 测试，并检查客户端基础库与 Linux 会合及中继二进制目标的编译。TCP 测试覆盖直接连通与原样转发的字节流。完整端点会话进入受控端核销后再验证。

## 受管会合和中继

用 `DESKREALM_MANAGED_MODE=1` 启动受管 `hbbs` 和 `hbbr`。两个进程分别配置 `DESKREALM_MANAGED_CENTER`（HTTPS 中心源站）、`DESKREALM_MANAGED_CENTER_KID` 和 `DESKREALM_MANAGED_CENTER_PUBLIC_KEY`（无填充 Base64url 的 Ed25519 公钥）。可选 `DESKREALM_MANAGED_CA_FILE` 指向 PEM 格式 CA。服务凭据只配置在各自进程，中心请求验证 HTTPS 且禁止重定向。两套运行配置使用独立的实例、监听地址或端口和数据目录。

| 进程 | 额外配置和行为 |
| --- | --- |
| `hbbs` | 使用持久化 RustDesk 签名密钥和 `DESKREALM_MANAGED_RENDEZVOUS_TOKEN`。需要中继时设置 `DESKREALM_MANAGED_RELAY_ID` 以及 `DESKREALM_MANAGED_RELAY_ADDRESS`（主机与端口），两项均省略时仅协调 TCP 直连。 |
| `hbbr` | 设置同一 `DESKREALM_MANAGED_RELAY_ID` 和独立的 `DESKREALM_MANAGED_RELAY_TOKEN`。只接受受管 TCP 配对。 |

受管模式不处理原版连接请求、原版 UDP 打洞应答或 WebSocket 传输请求。UDP 仍用于设备地址登记和会合通知。主控申请 TCP 直连或中继协调。会合校验主控角色证明、调用中心复查，并核对受控设备登记公钥后才转发。在受控端从 TCP 返回应答时，会合再次复查授权，确认应答与同一个挂起通知绑定。会合从受控端 TCP 连接观察并返回直连候选地址，中继地址来自进程配置。独立试点实例可使用原有配置。

每个会合或中继 TCP 入口生成独立的 32 字节挑战。Ed25519 持钥证明的签名输入依次为 ASCII `DeskRealm/M2/transport-proof/v1`、零字节、一字节用途、一字节角色、32 字节授权摘要、16 字节候选连接 UUID、32 字节挑战、32 字节会合通知随机数、目标 RustDesk ID 的 ASCII 字节。用途按直连申请、中继申请、直连应答、中继应答和加入中继依次为 1 至 5。角色值为主控 1 或受控 2。首次会合申请的通知随机数全零，受控应答和中继加入使用会合转发的随机数。发送端通过 `transport_proof` 传输其 32 字节公钥和 64 字节签名。

中继要求两端分别证明其角色，核对授权、连接 UUID 和会合通知后请求中心登记配对。收到中心 204 后，中继向两端发送 `relay_ready`，随后开始原样转发。客户端必须等待 `relay_ready` 才能开始端到端数据握手。中心拒绝或超时会关闭两个连接。

运行 `bash scripts/deskrealm-m2-test native-coordination` 可从锁定源码构建和运行两种服务。测试使用本机 TLS 中心夹具和签名端点，覆盖真实会合通知、直连候选回应、中继配对及双向字节转发。

## 原生端点准入

客户端的 RustDesk `base` 库提供中心签发、会合协调、直连 TCP 接入、中继放行等待、方向密钥交换和在线核销。主控以已绑定的 `dru_` 申请授权。受控端以本地设备身份接收会合通知，并校验目标 ID 及授权设备公钥摘要。参与中继时，两端分别验证消息的中继地址与本地配置一致，再向同一中继证明持钥。直连目标由会合返回 TCP 观察地址，受控端在相同本地端口接收连接。

受控端的 `key_offer` 先以当前设备密钥签名。输入依次为 ASCII `DeskRealm/M2/key-offer/v1`、零字节、会话 UUID、连接 UUID、一字节路径、四字节大端协商版本和 32 字节临时公钥。主控验证签名与授权的目标公钥摘要后，使用临时密钥传输密封的会话密钥。端点只接受版本 1 的 RustDesk 方向密钥协商。初始明文握手之后，双方在加密流中验签并等待受控端核销结果。

受控端要求本地策略版本和操作范围匹配授权。有人值守时，受控入口请求本地批准，签名确认动作并用 `drd_` 请求中心核销。拒绝批准、权限不匹配、证明失败或核销失败时，端点不会向主控发送准入结果。中心返回成功后，受控端将经核销的动作和租约信息告知主控，并保留私有会话凭据。

`bash scripts/deskrealm-m2-test native-admission` 从锁定源码构建端点库、`hbbs` 和 `hbbr`，在只含回环网络的容器运行两条 TCP 路径。TLS 中心夹具独立核对两端签名、本地批准签名、中继配对、设备认证、策略应用回执、续租和连接关闭回执。两条连接测试使用夹具提供的本地确认。

## 受管桌面入口

用 `DESKREALM_MANAGED_MODE=1` 启用受管桌面。同一构建也可以用 Cargo 功能 `deskrealm-managed` 将受管模式设为进程默认入口。该构建仍要求显式配置 `DESKREALM_MANAGED_MODE=1`。缺少受管参数时拒绝连接。客户端使用与服务相同的 HTTPS 中心、中心签名公钥和可选 CA 配置。

| 客户端配置 | 用途 |
| --- | --- |
| `DESKREALM_MANAGED_ORGANIZATION_ID`、`DESKREALM_MANAGED_DEVICE_ID` | 已批准设备的组织与设备 UUID。 |
| `DESKREALM_MANAGED_DEVICE_SEED_FILE` | 已登记的 Ed25519 设备种子文件，单行无填充 Base64url 编码的 32 字节种子。 |
| `DESKREALM_MANAGED_RUSTDESK_ID`、`DESKREALM_MANAGED_RENDEZVOUS_ADDRESS` | 与批准记录绑定的 RustDesk ID，以及会合服务的主机和端口。 |
| `DESKREALM_MANAGED_RELAY_ID`、`DESKREALM_MANAGED_RELAY_ADDRESS` | 可选，启用中继时两项同时配置，使用与服务一致的中继 ID、主机和端口。 |
| `DESKREALM_MANAGED_CONTROLLER_CREDENTIAL_FILE`、`DESKREALM_MANAGED_PURPOSE` | 主控读取已绑定到本设备的 `dru_` 凭据文件。用途可选，默认为 `helpdesk`。 |
| `DESKREALM_MANAGED_INPUT` | 设置为 `1` 时附加键鼠请求。默认为 `0`。 |
| `DESKREALM_MANAGED_AUDIO` | 设置为 `1` 时附加受控端系统音频请求。默认为 `0`。 |

受控端用设备签名完成中心挑战并定期发送心跳，按组织、设备和密钥版本核对中心策略。策略校验通过后才应用版本及动作集合，并签发应用回执。遇到支持上报的策略字段错误时签发失败回执，暂停新准入。主控从桌面入口发起有人值守的查看请求，可独立附加键鼠及音频。有效权限以中心策略、本地确认和核销结果为准。界面的强制中继选项决定申请的连接路径。

Windows 本地确认发送到当前解锁的物理控制台，显示此次授权的动作，默认拒绝。无可用控制台、超时或同时已有一条确认时拒绝。有效核销且中心状态为活动后，受控端订阅主显示器原生视频，主控用 RustDesk 解码。当前每台受控端同时接受一个会话。受控端在订阅、帧入队和发送前检查许可，使用有界帧队列。

键鼠要求独立的 `input` 授权。Windows 受控输入只在已解锁的活动物理控制台内执行。Linux 开发验证使用 X11。当前接入物理映射键盘、主屏绝对鼠标移动、左中右键和垂直滚轮。受控端在入队和本地执行前核对权限，丢弃来自暂停前的旧事件，释放本会话仍按下的键和鼠标按钮。

音频要求独立的 `audio` 授权。受控端在已解锁的 Windows 物理控制台使用 WASAPI 回环采集默认系统输出，单向发送到主控。主控依据核销权限和受控端的音频许可接收并播放。受控端丢弃暂停前及排队达 150 毫秒的音频包。撤销、到期、控制台锁定或连接关闭后，受控端停止采集，清理输入及采集线程后提交停止回执。剪贴板和文件传输待接入。

`bash scripts/deskrealm-m2-test managed-interop` 运行隔离传输用例和签名向量，并检查启用受管入口的 Linux RustDesk Rust 库编译。`bash scripts/deskrealm-m2-test managed-view` 和 `bash scripts/deskrealm-m2-test managed-input` 用 Xvfb 分别验证原生采集启停和本地键鼠执行、释放及队列失效。`bash scripts/deskrealm-m2-test managed-audio` 验证音频消息权限、有效格式、Opus 编解码和过期队列丢弃。Windows 原生制品、真实显示、键鼠、WASAPI 采集、播放和实际本地确认仍需 Windows 环境验证。

## 原生构建入口与证据

受管 Windows 构建沿用固定客户端工作流的原生 MSVC 环境。先按工作流生成对应版本的 Flutter bridge 并安装锁定的 vcpkg 依赖。上游配置为 Rust 1.75、vcpkg `9e593bb18ea69cc5095e012465dcd675a822ed0d`，ARM64 用对应版本的原生 Dart 和 Windows engine，并将 `SODIUM_LIB_DIR` 指向 ARM64 vcpkg 库。 [固定 Windows 工作流](https://github.com/rustdesk/rustdesk/blob/95cca3f661f635b3cc85c04935fe8d96d8f432ef/.github/workflows/flutter-build.yml#L157-L339)

| 目标 | 工作流环境 | 客户端目录中的构建命令 | 原生输出目录 |
| --- | --- | --- | --- |
| Windows x64 | `windows-2022`、`x86_64-pc-windows-msvc`、Flutter 3.24.5、`x64-windows-static` | `python3 ./build.py --portable --flutter --skip-portable-pack --hwcodec --vram` | `flutter/build/windows/x64/runner/Release` |
| Windows ARM64 | `windows-11-arm`、`aarch64-pc-windows-msvc`、Flutter 3.44.9、`arm64-windows-static` | `python3 ./build.py --portable --flutter --skip-portable-pack --hwcodec` | `flutter/build/windows/arm64/runner/Release` |

服务端从固定源码准备后，在对应 Linux 构建环境的服务端目录执行 `cargo build --locked --release --bin hbbs --bin hbbr`。工具链和依赖准备参考固定上游镜像构建配方。 [固定服务端构建配方](https://github.com/rustdesk/rustdesk-server/blob/73523b31cfd25d77dee862e6fc9f5e1fb5e485ef/docker-classic/Dockerfile)

开发工件建议保存到 checkout 外的 `evidence/m2/builds/<target>/<deskrealm-commit>/`。每次记录 DeskRealm 提交、上游与子模块 SHA、补丁摘要、编译器及 SDK 版本、运行命令和工件 SHA-256。当前原生协议检查在 Linux ARM64 容器构建及运行受管会合、中继和端点测试。Windows x64、ARM64 原生制品留待 03-04 Task 2，目标环境注册和联网验收留待 Phase 5。
