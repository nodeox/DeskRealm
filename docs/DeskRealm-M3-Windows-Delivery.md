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

同一批准输入和构建证据会产生相同的包与清单。打包失败时保留原候选目录，签名服务恢复后可用同一清单重试。签名范围记录为 `development`，使用组织信任的开发公钥验证。当前交付物是带分离签名的开发 ZIP，Windows Authenticode 安装器、控制台授权发布和客户端读取配置在后续开发中接入。

```bash
bash scripts/deskrealm-m3-test signed-build
```
