# DeskRealm M1 首次初始化与登录

当前可运行的控制面包括 PostgreSQL 迁移、一次性首管初始化、本地账号与外部身份登录、中文概览、成员管理、用户组委派和 API 令牌。平台管理员可创建、邀请、导入、启用、停用、归档及恢复账号，也可向已启用的本地账号移交完整管理员角色。成员与权限变更均进入审计。

## 本地开发检查

需要 Go 1.26 或更新版本、Node 26、Docker 和 Python 3。在仓库根目录执行。

```sh
bash scripts/deskrealm-m1-test bootstrap-api
bash scripts/deskrealm-m1-test identity-lifecycle
bash scripts/deskrealm-m1-test identity-federation
bash scripts/deskrealm-m1-test authorization
bash scripts/deskrealm-m1-test enrollment
bash scripts/deskrealm-m1-test device-lifecycle
npm --prefix web ci --ignore-scripts
npm --prefix web run typecheck
PLAYWRIGHT_SKIP_BROWSER_GC=1 npm --prefix web exec -- playwright install chromium
bash scripts/deskrealm-m1-test bootstrap-browser
```

两个测试入口使用独立 PostgreSQL 容器和本机临时端口，退出时清理。目标环境的 Linux 中心、中继和双 Windows 验收在首版开发完成后的 Phase 5 执行。

## 在开发数据库上运行

`DESKREALM_DATABASE_URL` 使用开发 PostgreSQL 连接串。迁移专用连接可通过 `DESKREALM_MIGRATION_DATABASE_URL` 指定。

```sh
npm --prefix web run build
export DESKREALM_DEV_HTTP=1
export DESKREALM_PUBLIC_ORIGIN=http://127.0.0.1:8080
export DESKREALM_HTTP_ADDR=127.0.0.1:8080
export DESKREALM_WEB_DIR="$PWD/web/dist"
go run ./cmd/deskrealm migrate
go run ./cmd/deskrealm bootstrap-ticket
go run ./cmd/deskrealm serve
```

打开票据命令打印的链接，设置组织、账号和至少 12 字符的密码。链接有效期为 15 分钟。再签发会替换前一次票据，完成初始化后停止签发。随后用新账号登录并在概览查看当前组织和账号。

平台管理员在「用户与权限」生成邀请或恢复链接，再通过组织渠道交给对应成员。邀请有效期为 1 小时，恢复有效期为 15 分钟。兑换会移除地址栏里的票据并使旧会话失效。归档账号完成密码恢复后处于停用状态，由管理员明确启用。批量导入每次最多 100 个账号，新账号待激活，已有账号返回原状态。

正式入口使用 HTTPS。将中心外部 HTTPS origin 配给 `DESKREALM_PUBLIC_ORIGIN`，用反代连接 Go 的内部监听地址。HTTP 开发模式仅绑定本机回环。浏览器请求使用同源接口，注销需要当前会话的 CSRF 值。

## 两步验证与外部身份

在中心的受控运行环境设置 `DESKREALM_TOTP_KEY`。它需要是 32 字节随机密钥的标准 Base64 表示，可用 `openssl rand -base64 32` 生成。密钥由部署保管并随中心身份一同备份，所有控制面启动使用同一密钥。用户从「账号安全」核对密码、添加验证器并输入一次验证码确认。启用后本地和 OIDC 登录均要求验证码。平台管理员可用当前密码与操作原因重置账号的验证器。重置写审计并撤销现有会话。

配置 `DESKREALM_OIDC_ISSUER`、`DESKREALM_OIDC_CLIENT_ID` 和 `DESKREALM_OIDC_CLIENT_SECRET`。在身份提供方登记回调 `<DESKREALM_PUBLIC_ORIGIN>/v1/auth/oidc/callback`。中心主机运行 `deskrealm oidc-bind USERNAME SUBJECT`，将提供方给出的稳定 subject 绑定至已有启用账号。邮箱用于显示场景，登录身份以签发方和 subject 为准。身份提供方变更时重新核对绑定。成功配置后登录页面显示「使用组织身份登录」。

## 用户组、角色与令牌

在「用户组与委派」建立用户组、维护成员，并为指定账号分配用户查看员或用户管理员角色。作用域可为组织、指定用户组或本人。用户管理员在获准范围更改普通账号状态，平台管理员管理成员关系、发放委派及移交完整管理员角色。移交后双方须重新登录。

「API 令牌」签发 `users.read` 权限，用于可访问账号的列表和详情，有效期 1 分钟至 7 天。调用方以 `Authorization: Bearer <token>` 发送凭据。令牌创建时显示一次，签发者当前权限始终参与服务器判定，撤销和到期后立即失效。

控制角色可维护允许的画面、键鼠、音频、剪贴板和文件传输动作。控制角色独立于后台管理授权，其配置版本供 M2 的会话准入和终端约束使用。

## 设备登记与审批

在「设备管理」创建设备组，并按成员授予查看或审批权限。审批人员可为授权组发放一次性入网凭据，指定 Windows 架构、用途和 1 至 1440 分钟的有效期。通过组织渠道交付凭据。设备提交登记后，在台账中查看或处理待审批记录。登记状态、在线状态、策略期望版本和实际回执分别展示。

终端先在本机生成 Ed25519 设备密钥。向 `POST /v1/enrollment/challenge` 提交 `credential`、`group_id`、`platform`（`windows-amd64` 或 `windows-arm64`）、`rustdesk_id` 和标准 Base64 格式的 32 字节 `public_key`。中心返回 `attempt_id`、`nonce` 和挑战到期时间。将以下各项用单个换行符连接，末尾无需换行，并用设备私钥对所得 UTF-8 字节签名。

```text
deskrealm/device-enrollment/v1
<attempt_id>
<nonce>
<group_id>
<platform>
<rustdesk_id>
<public_key>
```

向 `POST /v1/enrollment/complete` 提交原始字段、`attempt_id` 和标准 Base64 的 64 字节 `signature`。成功返回稳定的 `device_id` 和初始登记状态。已经完成的请求可使用相同输入复取原回执。后续审批结果通过设备台账查询。开发测试用签名夹具模拟终端，原生 Windows 接入与实机验收按路线图进行。

### 设备维护和密钥轮换

设备详情展示固定 UUID、RustDesk ID 历史和密钥 SHA-256 指纹。移组需要来源组和目标组的设备管理权限。修改 RustDesk ID、移组、禁用和退役请求使用详情中的 `version`。若版本冲突，重新获取详情再操作。禁用或退役撤销密钥及未使用的轮换凭据，并保留审批和审计记录。连接观测和策略回执保留原值。退役是终态。

对已批准设备选择「轮换密钥」，或对已禁用设备选择「恢复设备」。控制台返回与设备 UUID 和当前版本绑定的 15 分钟凭据。重新签发会使此前未使用的凭据失效。将凭据通过组织渠道交给目标终端。终端生成新的 Ed25519 密钥，并用新私钥签署以下 UTF-8 字段。字段用单个换行符连接，末尾无需换行。

```text
deskrealm/device-key-rotation/v1
<device_id>
<version 的十进制值>
<nonce>
<标准 Base64 的 32 字节新 public_key>
```

向 `POST /v1/enrollment/rotate` 提交 `device_id`、整数 `version`、`credential`、`public_key` 和标准 Base64 的 64 字节 `signature`。受理后原密钥失效，设备 UUID 保持不变，回执包含新版本和已批准状态。终端在新密钥仍有效期间可以安全重提交相同证明。

| 接口 | 返回 |
| --- | --- |
| `GET /v1/bootstrap/status` | 初始化状态 |
| `POST /v1/bootstrap` | 成功创建组织和管理员后返回 201 |
| `POST /v1/auth/login` | 成功创建不透明会话后返回 204 |
| `GET /v1/auth/session` | 当前账号、组织、角色、会话绑定的 CSRF 值 |
| `POST /v1/auth/logout` | 撤销会话并清理 Cookie 后返回 204 |
| `GET /v1/overview` | 当前组织及管理能力 |
| `GET /v1/admin/users` | 平台管理员可分页查看账号、角色和状态 |
| `POST /v1/admin/users` | 创建成员账号，需当前会话的 CSRF 值 |
| `POST /v1/admin/users/{id}/{enable,disable,archive}` | 记录原因并变更状态。停用或归档会撤销现有登录会话 |
| `POST /v1/admin/user-invitations`、`POST /v1/admin/users/{id}/recovery` | 签发一次性邀请或恢复链接 |
| `POST /v1/auth/accept-invitation`、`POST /v1/auth/recover` | 兑换链接并设置密码 |
| `POST /v1/admin/users/imports` | 使用请求键重复查询同一批导入结果 |
| `GET /v1/auth/totp`、`POST /v1/auth/totp/{setup,confirm}` | 查询及配置当前账号的验证器 |
| `POST /v1/auth/totp/verify`、`POST /v1/admin/users/{id}/totp-reset` | 完成两步登录或由管理员重置验证器 |
| `GET/POST /v1/admin/groups`、`GET/POST /v1/admin/groups/{id}/members` | 查看与管理用户组和成员 |
| `GET/POST /v1/admin/role-grants`、`POST /v1/admin/role-grants/{id}/revoke` | 授予及撤销管理委派 |
| `POST /v1/admin/users/{id}/transfer-platform-admin` | 移交管理员权限，原子撤销双方凭据 |
| `GET/POST /v1/admin/api-tokens`、`POST /v1/admin/api-tokens/{id}/revoke` | 签发、查询及撤销账号读取令牌 |
| `GET/POST /v1/admin/control-roles`、`POST /v1/admin/control-roles/{id}` | 创建、查询及按版本编辑控制角色 |
| `GET/POST /v1/admin/device-groups`、`GET/POST /v1/admin/device-groups/{id}/grants` | 维护设备组及查看或审批委派 |
| `POST /v1/admin/enrollment-credentials` | 为授权的组和平台发放一次性登记凭据 |
| `GET /v1/admin/devices`、`GET /v1/admin/devices/{id}`、`POST /v1/admin/devices/{id}/{approve,reject}` | 查询和处理授权范围内的设备登记 |
| `GET /v1/admin/devices/{id}/history` | 查看设备原有 RustDesk ID 和密钥指纹历史 |
| `POST /v1/admin/devices/{id}/lifecycle/{move,rename-id,disable,retire}` | 按设备版本和作用域变更设备 |
| `POST /v1/admin/devices/{id}/key-rotation`、`POST /v1/enrollment/rotate` | 签发轮换或恢复凭据并验证新密钥 |
