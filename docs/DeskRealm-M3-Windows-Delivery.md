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

同一批准输入和构建证据会产生相同的包与清单。打包失败时保留原候选目录，签名服务恢复后可用同一清单重试。签名范围记录为 `development`，使用组织信任的开发公钥验证。当前交付物是带分离签名的开发 ZIP。Windows Authenticode 安装器、分组发布和客户端读取配置在后续开发中接入。

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

成功验证记录保留该次审计证据。后续发布需要再次验证实际交付字节，并应用组范围与分发授权。

```bash
bash scripts/deskrealm-m3-test signed-build
bash scripts/deskrealm-m3-test delivery-tasks
```
