# DeskRealm M0 部署与验收手册

本手册用于 M0 的 RustDesk OSS 试点。按项目路线图，在 **M0–M3 开发完成后的 Phase 5** 才执行目标环境验收。开发期间用 `go test ./...` 静态解析两份 Compose 配置，并用替身检查启动、状态和重建流程。

## 准备安装入口

从本次候选提交构建。两台 Linux 主机分别使用与自身架构匹配的产物。

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/deskrealm-m0-amd64 ./cmd/deskrealm-m0
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/deskrealm-m0-arm64 ./cmd/deskrealm-m0
```

主机应已安装本地 Docker Engine 和 Compose 插件。固定镜像为 RustDesk Server OSS **1.1.16**，OCI 索引摘要为 `sha256:8ecdab65deb7c84652a626380e31d11a8f1fbafd97916d57f95c20628f943c00`。文件由能访问 Docker 和部署目录的同一运维身份维护。准备专属路径和已存在的父目录，部署路径可含空格，应避开美元符号和控制字符。

| 主机 | 角色 | 原生端口 |
| --- | --- | --- |
| 中心 | `hbbs` | `21115/tcp`、`21116/tcp`、`21116/udp` |
| 中继 | `hbbr` | `21117/tcp` |

选定两台 Windows 与中心容器均可解析并访问的中继地址。验证各主机的防火墙、名称解析及 NAT 回环。M0 使用原版客户端和公开的中心 Key。将试点端口限定于获授权的测试网络。受管授权和访问控制由后续阶段交付。

## Phase 5 先部署 Linux 中心和中继

将构建的对应架构产物以 `deskrealm-m0` 命令名安装到两台 Linux 主机的运维 PATH。以下示例使用各自主机上的 `/var/lib` 子目录。替换示例域名和公钥。

中心先生成配置和公开身份。

```sh
deskrealm-m0 init --role center --dir /var/lib/deskrealm-m0-center \
  --center-host id.example.test --relay-host relay.example.test
deskrealm-m0 config --dir /var/lib/deskrealm-m0-center
```

记录输出中的 `project`、`public_key` 和 `public_key_sha256`，供中继配置、Windows 客户端和重建取证使用。重复运行同一个 `init` 命令核对公钥指纹和项目名。中心私钥留在中心的 `data/id_ed25519`，取证只记录公钥及指纹。

在中继主机上执行。

```sh
deskrealm-m0 init --role relay --dir /var/lib/deskrealm-m0-relay \
  --center-key '<中心 public_key 字段原值>'
deskrealm-m0 config --dir /var/lib/deskrealm-m0-relay
deskrealm-m0 up --dir /var/lib/deskrealm-m0-relay
deskrealm-m0 status --dir /var/lib/deskrealm-m0-relay
```

回到中心启动会合。

```sh
deskrealm-m0 up --dir /var/lib/deskrealm-m0-center
deskrealm-m0 status --dir /var/lib/deskrealm-m0-center
```

两边状态应报告 `running: true` 和 `ready: true`。中心还报告 `database_ready: true` 与实际 `center_bind`。该状态核对进程、容器配置和本地数据。网络验收继续核对中心启动日志中 `relay-servers` 实际列出的中继地址，以及 Windows 与中继之间的实际 TCP 可达性。

在每台 Linux 主机将 `deployment`、`project`、`service` 替换为该主机对应的部署路径、`init` 输出的项目名与服务名（`hbbs` 或 `hbbr`），然后采集运行证据。

```sh
docker compose -p "$project" -f "$deployment/compose.json" \
  --project-directory "$deployment" --env-file /dev/null ps
docker compose -p "$project" -f "$deployment/compose.json" \
  --project-directory "$deployment" --env-file /dev/null logs --timestamps "$service"
docker inspect --type container "$container_id" \
  --format '{{.Id}} {{.Image}} {{json .Mounts}} {{json .NetworkSettings.Ports}}'
docker buildx imagetools inspect \
  docker.io/rustdesk/rustdesk-server:1.1.16@sha256:8ecdab65deb7c84652a626380e31d11a8f1fbafd97916d57f95c20628f943c00
```

`container_id` 使用 `status` 输出的值。记录容器 ID、引擎报告的镜像 ID、多架构索引及对应架构的清单摘要。中心日志缺少预期中继时，检查容器对中继名称的解析以及公布地址，修好后继续。私钥、SQLite 或初始化记录缺失时，从同一部署的备份恢复，再按原目录执行 `status` 和 `up`。

## Phase 5 再验证双 Windows

在两台获授权的 Windows 主机安装原版 RustDesk **1.5.3**。按两台主机各自架构选择 x64 或 ARM64 官方 MSI 并记录安装产物 SHA-256。安装后在两端填写同一个 `ID Server`（中心地址）和 `Key`（`public_key` 原值），将 `Relay Server` 和 `API Server` 留空，关闭试点无关的 WebRTC/TURN 选项。记录每一步的 UTC 时间窗、两端版本与状态。

### 1. 双端注册

分别确认两端在线且界面显示各自设备 ID。核对对应中心日志的注册或心跳，以及客户端配置中的中心地址。两端均有匹配证据才记录通过。

### 2. 安全直连

主控输入对端普通 ID，受控端确认请求。以会话中的安全和直连状态作为证据，记录客户端报告的实际路径及安全状态。客户端若回退到中继，本项留为未通过，排查拓扑和防火墙后复跑。

### 3. 强制中继

结束直连会话后，在主控输入 `<ID>/r` 重连并在受控端确认。核对客户端安全中继状态和预期 `hbbr` 日志同一时间窗中的 `got paired`。如果客户端使用其他路径或配对记录无法匹配，记为失败或证据缺失。

### 4. 重建后沿用原公钥

先结束测试会话并记录中心 `status`、容器 ID、绑定路径和公钥指纹。用原配置和原部署目录重建中心。

```sh
deskrealm-m0 recreate --dir /var/lib/deskrealm-m0-center
deskrealm-m0 status --dir /var/lib/deskrealm-m0-center
```

确认中心容器 ID 已改变，绑定目录和公钥指纹一致，数据库已就绪。查看中心日志的原 Key 和有效中继公告。保持两端原客户端配置，重新连接并核对注册和实际连接。任何身份、挂载、数据库或连接证据缺失时，记录失败或证据缺失，保留原目录并检查备份。

## 证据记录模板

| 字段 | 实测记录 |
| --- | --- |
| 候选提交，执行人及 UTC 时间窗 | |
| 两台 Linux OS、架构、Docker Engine 和 Compose 版本 | |
| 两端 Windows OS、架构、客户端版本、安装产物 SHA-256 | |
| 中心、中继端点，镜像 OCI 索引和选中平台清单摘要 | |
| 原中心公钥指纹，中心启动日志的 relay-servers | |
| 双端注册状态、设备标识、日志及时间窗 | |
| 直连角色、实际路径、安全状态、界面或日志 | |
| 强制中继实际路径、安全状态、配对日志及时间窗 | |
| 重建前后容器 ID、挂载、SQLite 状态和公钥指纹 | |
| 沿用原配置的重新注册和连接结果 | |
| 每个场景的通过、失败或证据缺失原因和证据路径 | |

备份包含中心部署目录的配置、初始化记录、密钥与 SQLite 状态。备份或复制数据库前停止中心服务，恢复时使用原绝对路径及权限。对外分享日志时移除设备标识、凭据和业务信息。

- 参考版本。[RustDesk Server OSS 1.1.16](https://github.com/rustdesk/rustdesk-server/releases/tag/1.1.16) · [RustDesk 客户端 1.5.3](https://github.com/rustdesk/rustdesk/releases/tag/1.5.3)
- 路径验证参考。[客户端强制中继入口](https://github.com/rustdesk/rustdesk/blob/95cca3f661f635b3cc85c04935fe8d96d8f432ef/src/client.rs#L2942-L2954) · [中继配对日志](https://github.com/rustdesk/rustdesk-server/blob/c3a3e0d1070e29cb28b468cf6ef5b184e65e4051/src/relay_server.rs#L425-L455)
