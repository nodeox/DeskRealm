# Windows 开发交付包

受管 Windows 原生构建为 x64 和 ARM64 分别生成 `client.zip`、构建证据清单和测试日志。开发交付命令使用已批准的构建证据，生成两个 ZIP 和一份共同的清单。每个 ZIP 携带原生客户端归档、版本化品牌资料与中心公开配置。

## 批准输入

批准方从已核对的 Windows 构建与中心安装信息生成独立的 `approval.json`。受信构建来源和中心身份由批准方从自己的部署记录和 CI 工件核对。打包端使用实际构建的 Git checkout 校验锁定源码、工具链、PE 架构、工件摘要和具名测试，再验证归档的 Windows 路径安全。

| 字段 | 内容 |
| --- | --- |
| `schema` | 整数 `1` |
| `version` | 版本号，例如 `0.1.0-dev.1` |
| `brand` | `id`、正数 `revision`、显示 `name` 和 Windows ICO 的 `icon_sha256` |
| `center` | 从已验活的部署记录核对 `public_origin`、`id_server`、`rendezvous_public_key`、`signing_key_id`、`signing_public_key`。固定中继可另填 `default_relay` 的 `id` 与 `address` |
| `group_ids` | 本候选拟发布范围的组 UUID，排序并去重 |
| `source_commit` | Windows 构建证据对应的完整 Git 提交 |
| `builds` | `x64` 与 `arm64` 各自的原生证据 `manifest_sha256` 和 `client_sha256` |

每个 ZIP 中的 `deskrealm-public.json` 只携带版本、品牌和公开配置。组范围保存在外部签名清单。设备凭据、中心私钥和签名私钥由各自的管理流程持有。

## 构建、签名和独立验证

使用 Python 3.9 以上与支持 Ed25519 的 OpenSSL 3 或以上。证据目录使用 `TARGET/COMMIT/` 结构，与原生构建脚本一致。打包输出的父目录应已存在。

```bash
python3 -B managed/windows_package.py prepare approval.json evidence brand.ico candidate \
  --source-tree /path/to/windows-source
```

把相同的批准文件及生成的 `release.json` 送到独立的开发签名环境。签名以 `DeskRealm/windows-delivery/v1` 加零字节作为用途前缀，覆盖清单原始字节。签名环境返回独立的 `release.sig`。

```bash
python3 -B managed/windows_package.py sign approval.json release.json developer-private.pem release.sig
```

验证环境使用独立保存的原始批准文件与开发公钥，并核对两架构的包及内部原生客户端字节。

```bash
python3 -B managed/windows_package.py verify approval.json candidate release.sig developer-public.pem
```

同一批准输入和构建证据会产生相同的包与清单。打包失败时保留原候选目录，签名服务恢复后可用同一清单重试。签名范围记录为 `development`，使用组织信任的开发公钥验证。该 ZIP 可用于中心分组下载，也可作为 Authenticode 安装器的输入。

## 中心任务验证

中心进程使用独立的开发验签公钥，以及本次部署的公开身份。设置以下环境变量启用工作台入口。

| 环境变量 | 内容 |
| --- | --- |
| `DESKREALM_WINDOWS_CANDIDATE_DIR` | 中心可读的暂存目录绝对路径 |
| `DESKREALM_WINDOWS_DEVELOPMENT_PUBLIC_KEY_FILE` | 独立 Ed25519 开发公钥的 PEM 文件 |
| `DESKREALM_WINDOWS_ID_SERVER` | 已核对的会合端点，含端口 |
| `DESKREALM_WINDOWS_RENDEZVOUS_PUBLIC_KEY` | 部署记录中的会合公钥，标准 Base64 |
| `DESKREALM_WINDOWS_DEFAULT_RELAY_ID` 与 `DESKREALM_WINDOWS_DEFAULT_RELAY_ADDRESS` | 若批准记录包含固定中继，填写同一 ID 与端点 |

中心继续使用运行中的 HTTPS 公开地址和受管签名公钥。Compose 部署可通过服务覆盖配置传入这些变量，并把暂存目录和验签公钥挂载为只读。暂存目录由构建操作员管理，子目录以 `release.json` 原始字节的 SHA-256 命名。

```text
CANDIDATE_DIR/<manifest-sha256>/
  release.json
  release.sig
  bundle/
    DeskRealm-x64.zip
    DeskRealm-arm64.zip
```

平台管理员复核清单及目标设备组后，在任务中心选择「验证 Windows 开发包」，输入清单摘要。同一摘要的重复提交返回同一任务。工作台显示品牌修订、源码、目标组、两架构原生证据和 ZIP 摘要，成功后记录验签时间和公钥指纹。中心对两个架构完成验证后才登记验证结果。预期文件缺失、签名不符或归档损坏时，修正暂存文件后可在原任务重试。中心核对同一品牌修订与已验证资料一致。任务中断后由现有租约领取机制恢复。

成功验证记录保留该次审计证据。

## 分组发布与撤回

设置 `DESKREALM_WINDOWS_RELEASE_DIR` 启用开发包发布。这是中心可写、独立于暂存区的绝对路径。容器部署将其挂载到独立持久目录。平台管理员在验证任务详情选择「发布已验证开发包」，中心重新验签并校验两个架构，复制通过验证的字节至持久目录，再提交发布记录和签名所绑定的设备组范围。重复发布同一清单会返回原发布 ID。

持有目标设备组读取委派的成员可在「Windows 发布」下载 x64 或 ARM64 ZIP。每次请求会核对当前会话、组权限、撤回状态和归档摘要。删除委派或撤回后，后续下载请求会被拒绝。工作台公开各架构的 SHA-256，下载支持 HTTP Range。平台管理员可以查看已撤回条目，审计保留发布、授权下载和撤回记录。

## 单台设备的部署凭据

平台管理员或目标设备组的审批人可以为该组已发布的开发包发放凭据。发放、查询和撤销使用中心管理会话。写操作同时要求 CSRF 校验。

1. 部署端在目标设备生成并保留设备 Ed25519 密钥，收集 RustDesk ID 和架构。向授权渠道提供 ID、架构和标准 Base64 编码的设备公钥。
2. 向 `POST /v1/windows-releases/{release_id}/deployments` 提交下表字段。中心返回 `credential`、`expires_at` 及供查询的 `deployment_id`。通过组织的部署渠道将凭据单独传给该设备。公开归档只携带中心公开配置。
3. 设备向 `POST /v1/enrollment/challenge` 提交凭据及 `group_id`、`platform`、`rustdesk_id`、`public_key`。其中 `platform` 值为 `windows-amd64` 或 `windows-arm64`。使用本机私钥签署既有登记挑战，再向 `POST /v1/enrollment/complete` 提交相同数据、`attempt_id` 和标准 Base64 编码的签名。
4. 通过 `GET /v1/windows-releases/{release_id}/deployments/{deployment_id}` 查询凭据状态、设备登记结果、期望策略版本及已应用版本。待审批设备可从设备管理页审批。设备以既有协议上报策略应用回执。

| 发放字段 | 内容 |
| --- | --- |
| `deployment_id` | 部署方为单次单台设备部署保留的 UUID |
| `group_id` | 该发布已批准的目标组 UUID |
| `arch` | `x64` 或 `arm64` |
| `rustdesk_id` 与 `public_key` | 目标设备的 RustDesk ID 与 Ed25519 公钥 |
| `purpose` | 审批用途文字 |
| `lifetime_minutes` | 1 到 1440 分钟 |
| `auto_approve` | 默认进入待审批。平台管理员可选择自动审批，设备提交有效持钥证明时中心复核发放人的权限 |

发布有效且发放人仍有审批权限时，使用原输入和部署 UUID 重试会保留原到期时间，并在凭据可用时返回原凭据。需要重新发放时使用新的部署 UUID。审批人可通过 `POST /v1/windows-releases/{release_id}/deployments/{deployment_id}/revoke` 提交空 JSON 对象撤销待用凭据。撤回发布也会使对应的待用凭据失效。已登记设备的后续停用和回收通过设备管理执行。中心持久化凭据摘要和登记回执，部署渠道应保护凭据传递和设备私钥。

## 开发包首次启动

含启动程序的原生归档将 `DeskRealm.exe` 与 `rustdesk.exe` 打包。验证签名及摘要后，把 `client.zip` 解压到安装目录，并将外层归档中的 `deskrealm-public.json` 放到同一目录。用目标 Windows 用户执行 `DeskRealm.exe prepare RUSTDESK_ID`。此命令在本机创建并保留设备密钥，输出架构、RustDesk ID、设备公钥和 `deployment_file` 路径。相同 ID 的重复执行返回同一公钥。

获授权的发放人使用输出字段申请上一节的凭据。部署工具将发放接口返回的 JSON 放到 `deployment_file`，文件应与密钥目录使用同一 Windows 用户的访问权限。部署工具应在该用户第一次启动前完成写入。直接启动 `DeskRealm.exe` 时，程序从公开配置获取中心地址和公钥，通过 Windows 信任的 HTTPS 连接核对中心签名身份，用本机密钥提交挑战，并将登记回执保存到当前用户的 `LOCALAPPDATA\DeskRealm\deployments`。保存回执后删除部署文件。中心已登记但回执丢失时，程序在下次启动重试原挑战。后续启动沿用本机身份与回执，并将中心、会合和中继设置传给受管客户端。

## Authenticode 开发安装器

用已批准且包含启动程序的 x64 和 ARM64 ZIP 生成两个自包含安装器。构建端使用已提交的安装器源码和 `Go 1.27.4`，复验原发布签名、归档摘要与内部 PE 架构，再将原 ZIP 嵌入对应架构的安装器。

```bash
python3 -B managed/windows_installer.py prepare \
  approval.json candidate release.sig developer-public.pem unsigned
```

独立签名环境以组织批准的代码签名证书和 SHA-256 为 `unsigned/DeskRealm-Setup-x64.exe`、`unsigned/DeskRealm-Setup-arm64.exe` 签名，将签后文件以相同文件名放在 `signed`。为让签名在证书过期后继续可验证，正式签名同时使用组织批准的 RFC 3161 时间戳服务及 SHA-256 时间戳摘要。发布者证书及私钥由签名环境持有。验证端独立保存批准的 CA 及发布者证书，重建未签安装器，比较签名前后的字节，核对签名链、摘要算法及发布者证书 SHA-256。

```bash
python3 -B managed/windows_installer.py verify \
  approval.json candidate release.sig developer-public.pem signed \
  code-signing-ca.pem publisher.pem > installer-verification.json
```

安装器以当前 Windows 用户运行。它通过 Windows Authenticode 信任检查后，按归档摘要安装到本地用户的 `Programs\DeskRealm\releases`，并创建指向 `DeskRealm.exe` 的开始菜单入口。重新安装相同归档会检查已有文件。新版本解压成功后才更新入口，旧安装目录及登记状态继续保留。部署凭据仍通过上一节的流程单独下发。

### 中心发布签名安装器

独立验签环境可以在验签通过后生成分离签名证据和两个安装器文件。验签证据使用专用的 Ed25519 密钥签名，密钥与 ZIP 签名密钥、代码签名证书和中心密钥分别管理。中心只配置证据验证公钥及组织批准的发布者证书 SHA-256 指纹。

```bash
python3 -B managed/windows_installer.py verify \
  approval.json candidate release.sig developer-public.pem signed \
  code-signing-ca.pem publisher.pem \
  --attestation-key installer-attestation-private.pem \
  --attestation-directory verified
```

将命令生成的 `verified/<evidence_sha256>/` 目录整体放到中心暂存区的 `installers/` 下。目录包含 `installer-verification.json`、`installer-verification.sig`、`DeskRealm-Setup-x64.exe` 和 `DeskRealm-Setup-arm64.exe`。在启用 ZIP 发布配置后，给中心配置以下两项。

| 环境变量 | 内容 |
| --- | --- |
| `DESKREALM_WINDOWS_INSTALLER_ATTESTATION_KEY_FILE` | 独立验签环境的 Ed25519 公钥 PEM 文件 |
| `DESKREALM_WINDOWS_INSTALLER_SIGNER_SHA256` | 预先批准的 Authenticode 发布者证书 DER SHA-256，小写十六进制 |

平台管理员在已发布版本的「Windows 发布」页选择「发布签名安装器」，填入证据 SHA-256。中心验证证据签名、ZIP 发布清单关联、证书指纹及两个安装器的实际摘要，再把验签证据和文件保存至发布目录。相同证据重复提交返回同一记录。每个已发布版本关联一套安装器，替换安装器时发布新版本。获准下载 ZIP 的设备组可下载对应安装器。撤回发布后两个下载入口都失效，发放、下载与撤回由中心审计。

开发验证使用临时开发 CA。正式签名及目标 Windows 的证书信任按组织交付策略配置。

## 组织部署工具

管理员可在「API 令牌」选择「部署 Windows 终端」并指定设备组。自动化运行环境使用此限时令牌，目标 Windows 只收到单设备的入网凭据。每次中心操作都会核对令牌、发放人当前审批权限和发布组。部署 API 可下载该组的签名安装器，发放、查询及撤销本令牌创建的部署。设备登记后进入待审批，审批可在设备管理页完成。部署凭据到期时间受令牌的到期时间约束，撤销令牌会立即阻止其待用凭据完成登记。

在自动化主机构建工具，将 API 令牌存入仅运行账号可读的文件。以下命令中的发布 UUID、组 UUID 和部署 UUID 由组织侧保存。同一台设备重试时使用相同部署 UUID。

```bash
go build -o deskrealm-deploy ./cmd/deskrealm-deploy

./deskrealm-deploy fetch --center "$center" --token-file "$token_file" \
  --release "$release" --arch arm64 --output DeskRealm-Setup-arm64.exe
```

将安装器交给组织已有部署通道，在目标 Windows 用户身份下运行。安装器的 `--json` 输出安装目录。以下 PowerShell 生成组织侧发放凭据所需的端点公钥和写入路径。

```powershell
$installed = & .\DeskRealm-Setup-arm64.exe --json
if ($LASTEXITCODE -ne 0) { throw 'DeskRealm installation failed' }
$setup = $installed | ConvertFrom-Json
$launcher = Join-Path $setup.install_directory 'DeskRealm.exe'
$public = & $launcher prepare $rustDeskID
if ($LASTEXITCODE -ne 0) { throw 'DeskRealm identity preparation failed' }
$target = $public | ConvertFrom-Json
```

将 `$public` 作为 `target.json` 交回组织侧工具。信任设备管理通道提供的公钥与 RustDesk ID。以下命令将设备凭据写入私有输出文件，标准输出包含部署 ID 和到期时间。

```bash
./deskrealm-deploy issue --center "$center" --token-file "$token_file" \
  --release "$release" --group "$group" --deployment "$deployment" \
  --target target.json --purpose "受管终端部署" --minutes 30 --output deployment.json
```

通过同一组织通道把凭据交到对应目标机。目标用户先把文件写到 `$target.deployment_file` 同目录的临时文件，再原子移到 `$target.deployment_file`。随后运行 `& $launcher start` 或使用开始菜单入口完成登记。端点成功保存登记回执后清理部署文件。组织可按自身保留策略清理用于传输的临时副本。

```bash
./deskrealm-deploy status --center "$center" --token-file "$token_file" \
  --release "$release" --deployment "$deployment"
./deskrealm-deploy revoke --center "$center" --token-file "$token_file" \
  --release "$release" --deployment "$deployment"
```

状态接口返回部署状态和登记、策略回执。已消费凭据对应的设备通过设备管理执行停用或退役。需要补发过期或撤销的凭据时使用新的部署 UUID。控制台现有浏览器发放和自动审批流程继续按各自权限执行。

## 已登记设备分批升级

在验证任务中发布开发包时选择「仅通过分批任务分发」，并为该发布登记两种架构的签名安装器。中心应用迁移 30 后，平台管理员可以在「Windows 发布」选择「创建分批发布」，填入此发布已批准的设备组 UUID 和每批设备 UUID。每行一批，最多 20 批、150 台，每批最多 50 台。这里使用设备管理页的设备 UUID。该发布仅通过当前批次的部署工具接口分发。

创建任务即放行第一批。由持有此目标组部署令牌的运维人员从中心取得安装器，交给现有组织部署渠道，并由已登记的目标用户执行。部署工具根据设备所属批次与架构取得安装器，复核文件长度及 SHA-256。在 Windows 上走安装器的 Authenticode 校验和现有签名发行目录。安装后由目标用户从开始菜单启动新版本，沿用设备状态目录及原密钥。

```bash
./deskrealm-deploy fetch --center "$center" --token-file "$token_file" \
  --release "$release" --rollout "$task_id" --device "$device_id" \
  --arch x64 --output DeskRealm-Setup-x64.exe
```

在任务中心选择「核对设备结果」。中心核对放行后的安装器下载授权、新版本启动后的设备心跳、原设备密钥版本、当前组和已应用策略。每批审核时限为 30 分钟。超时、发布撤回、设备身份变化或运维记录失败时，下一批继续锁定。重试只放行本批失败设备，已完成对象和历史尝试保留。请先结清已经领取安装器的设备，再取消尚待执行的批次。当前批全部成功后，由管理员放行下一批。

需要兼容回滚时，在创建任务时指定同组、仍有效的原签名发布。中心会先核对每台目标机的现行安装包摘要与该发布一致。失败设备可通过已保存的签名原安装器重新安装，再由目标用户启动旧客户端。若原发布允许普通下载，也能重新领取原安装器。收到失败之后由原设备身份上报的旧包健康心跳，管理员在任务中选择「核对回滚」。中心会记录确认结果并继续暂停下一批。可核对的原版本也需要支持包摘要心跳。

设备心跳报告由受管客户端读取的签名包安装目录标识。开发门禁通过临时数据库和测试设备凭据验证状态流转。实际 Windows 安装、恢复和安全软件兼容性在目标设备验收中确认。

```bash
bash scripts/deskrealm-m3-test signed-build
bash scripts/deskrealm-m3-test delivery-tasks
bash scripts/deskrealm-m3-test managed-release
bash scripts/deskrealm-m3-test windows-installer
bash scripts/deskrealm-m3-test windows-rollout
```
