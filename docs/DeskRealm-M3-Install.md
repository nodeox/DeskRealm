# DeskRealm M3 中心安装

## 空机 Docker 运行时

在 Ubuntu 24.04 或 Debian 12 的 amd64、arm64 主机上，可先查看安装器固定的运行时选择。

```sh
deskrealm-install prepare-runtime --plan
sudo deskrealm-install prepare-runtime
```

已有兼容的本地 Docker Engine 和 Compose 会直接复用。空机准备使用 Docker 官方 APT 源，凭内置发布公钥验证 APT 索引，再安装固定版本的 Engine、CLI、containerd 和 Compose，最后启动并检查 systemd 的 Docker 服务。此步骤需要可达发行版和 Docker APT 源。签名离线镜像包用于已有 Docker 的主机。[Docker Ubuntu 安装说明](https://docs.docker.com/engine/install/ubuntu/) · [Docker Debian 安装说明](https://docs.docker.com/engine/install/debian/)

软件包准备中断后可重跑。安装器保留 `/var/lib/deskrealm-runtime/selection.json` 以核对原选择。发现不可用的既有 Docker 或 containerd 实例时，先修复该运行时，再使用安装器。

## 预检

使用 `deskrealm-install preflight` 检查准备安装的主机。预检以只读方式读取中心配置，核对 HTTPS 模式、镜像摘要引用、Docker Engine、Compose 和主机端口，并输出归一化的公开端点。

```sh
go build -o /tmp/deskrealm-install ./cmd/deskrealm-install
/tmp/deskrealm-install preflight --config /absolute/path/to/center.json
```

配置采用 JSON。将示例中的域名、目录与镜像引用替换为本次部署的值。镜像必须包含实际的 `@sha256:` 摘要。预检检查引用格式，拉取和核对镜像由安装步骤执行。

```json
{
  "version": 1,
  "directory": "/var/lib/deskrealm",
  "public_origin": "https://console.example.test",
  "id_server": "id.example.test:21116",
  "https": { "mode": "external" },
  "images": {
    "center": "registry.example.test/deskrealm/center@sha256:<center-digest>",
    "postgresql": "registry.example.test/deskrealm/postgres@sha256:<postgresql-digest>",
    "rendezvous": "registry.example.test/deskrealm/hbbs@sha256:<rendezvous-digest>"
  },
  "ports": { "api": 8080, "rendezvous_check": 21115, "rendezvous": 21116 }
}
```

- **现有 HTTPS 反代。** 使用 `https.mode: external`。预检检查 API 的本机 IPv4 回环端口，默认 `127.0.0.1:8080`。公开 HTTPS 端口由现有反代负责。
- **提供证书。** 使用 `https.mode: provided`，在 `https.certificate` 和 `https.private_key` 指定 PEM 绝对路径。预检检查证书与私钥是否匹配、证书有效期及管理域名。API 的监听端口应与管理域名中的端口一致，默认 443。
- **会合端口。** 默认检查 TCP 21115 和 TCP/UDP 21116，`id_server` 用于公布给终端。若公网端口由网络设备转发，公布端口可与本机端口不同。
- **指定监听地址。** 可通过 `ports.bind_host` 将会合端口和提供证书模式的 HTTPS 发布到指定的本机 IPv4 地址。开发夹具使用 `127.0.0.1`。已有反代模式的 API 固定发布到 IPv4 回环地址。
- **部署目录与运行时。** 部署目录使用绝对路径，父目录须已存在。预检使用本地 Docker Unix socket，要求 Linux amd64 或 arm64 Engine 以及 Compose 插件。

预检暂时绑定拟使用的本机 TCP 和 UDP 端口，随后释放。域名从目标网络解析、实际代理证书链、跨网可达性、镜像来源和服务健康仍须在安装和验活阶段核对。Linux 中心与中继及双 Windows 目标环境验收按 Phase 5 顺序执行。

## 准备中心配置和身份

```sh
/tmp/deskrealm-install prepare --config /absolute/path/to/center.json
```

首次准备原子写入部署目录。输出部署 ID、Compose 项目与文件位置、会合公钥和受管签名公钥的指纹。后续用同一配置运行会复用部署 ID 和两套公钥。遇到配置不一致、未知内容、原密钥或数据目录缺失时，命令报告冲突，按原部署备份恢复。

Compose 包含 PostgreSQL、一次性迁移、Go 中心和受管会合四个服务。PostgreSQL 18 的数据目录挂载在 `/var/lib/postgresql` 并位于私有服务网。迁移连接使用 `deskrealm_migrate`，Go 中心使用只操作业务数据的 `deskrealm`。Go 中心在迁移成功后启动。中心镜像提供 `/usr/local/bin/deskrealm` 和 `/opt/deskrealm/web`，会合镜像提供从锁定受管源码构建的 `/usr/local/bin/hbbs`。 [PostgreSQL 官方容器数据目录](https://hub.docker.com/_/postgres)

使用自建 CA 时，可通过 `https.ca_certificate` 指定 PEM 绝对路径供受管会合校验中心 HTTPS。已有反代模式发布回环 API 端口，提供证书模式由 Go 服务读取挂载的证书和私钥。

密钥和服务凭据保存在受限权限的部署目录。检查 Compose 时保留服务环境文件的路径，以免在输出中展开凭据。[Compose 配置命令](https://docs.docker.com/reference/cli/docker/compose/config/)

```sh
docker compose --env-file /dev/null -f /var/lib/deskrealm/compose.json \
  config --no-env-resolution
```

## 使用已有默认中继

部署时可选择一台已有的受管 `hbbr`。

```json
"default_relay": {
  "id": "primary-relay",
  "address": "relay.example.test:21117",
  "token_file": "/etc/deskrealm/relay-token"
}
```

`token_file` 使用权限为 0600 的普通文件，内容是与该 `hbbr` 共享的 32 字节随机凭据，按无填充 base64url 编码。首次 `prepare` 会把凭据快照保存到中心私有部署目录，并输出配置该 `hbbr` 所需的公开签名密钥和密钥 ID。已有中继需要使用同一中继 ID、凭据、HTTPS 中心和签名公钥。原始凭据文件用于预检和首次准备，重跑沿用已保存的快照。

设置默认中继后，`install` 和 `status` 会核对运行中中心和 `hbbs` 的中继配置，并从安装器所在主机检查中继 TCP 端口。失败时报告 `default-relay`。跨网可达性、认证配对与端点传输由后续中继及目标环境验收覆盖。

## 启动、验活和首次管理员

```sh
/tmp/deskrealm-install install --config /absolute/path/to/center.json
/tmp/deskrealm-install status --config /absolute/path/to/center.json
/tmp/deskrealm-install bootstrap-ticket --config /absolute/path/to/center.json
```

`install` 准备持久目录，按摘要取得与 Docker Engine 架构匹配的镜像，启动 PostgreSQL、迁移、中心和会合。提供证书模式通过本机绑定地址访问 HTTPS，并按管理域名校验证书。已有反代模式直接访问配置的公开 HTTPS 地址。验活检查数据库初始化状态、中心公开签名密钥和 RustDesk 会合响应。准备好后输出 `phase: ready` 和两套公钥指纹。

`status` 读取部署记录及当前 Compose 服务状态。停机时返回 `stopped`，仅准备完成时返回 `prepared`，验活失败时返回对应阶段和原因。首次启动达到 `ready` 后，由 `bootstrap-ticket` 生成一次性初始化链接。将链接直接交给首位管理员，并在浏览器完成初始化和登录。

中断后用同一配置重新执行 `install`。备份部署目录时保留 `database-ready`、`install-record.json`、原密钥和 PostgreSQL 数据。安装器按记录核对 PostgreSQL 系统标识，遇到数据缺失或不同实例时停止启动。

```sh
deskrealm-install install-record --config /absolute/path/to/center.json
```

安装记录包含稳定的部署与任务 ID、递增修订号、镜像准备、数据库、服务启动和验活的步骤状态及尝试次数。记录使用固定故障码。健康重跑核对服务实际状态并保持已完成的记录。

中心运行并创建首位管理员后，有权操作部署主机的人员可以把当前记录导入任务中心。`--actor` 指定一位当前启用的平台管理员，用于审计归属。

```sh
deskrealm-install import-record --config /absolute/path/to/center.json --actor admin
```

导入返回任务 ID、修订号及是否新写入。同一修订可重复提交，新的修订更新原任务，冲突或从未导入的较旧修订会被拒绝。平台管理员可在任务中心查看最新步骤。查看已导入的某一历史修订时，打开 `/tasks?task=TASK_ID&revision=REVISION`。每个新修订都会产生一条关联任务的审计记录。

## 签名离线包

在有 Docker Engine 和 Compose 的主机上，为原配置增加 `offline`。所用公钥由部署方从独立可信渠道预置，签名私钥保留在发布环境。

```json
"offline": {
  "manifest": "/mnt/deskrealm/manifest.json",
  "signature": "/mnt/deskrealm/manifest.sig",
  "trusted_public_key": "/etc/deskrealm/release-signing.pem"
}
```

在受控发布环境按原配置的摘要拉取目标平台镜像，使用 `docker image inspect --format '{{.Id}}'` 获取每个镜像的完整 ID。按这些 ID 用 `docker image save --output images.tar CENTER_ID POSTGRESQL_ID RENDEZVOUS_ID` 导出。发布环境应使用与部署环境兼容的 Docker 镜像存储后端。打包后可在清洁的同类环境执行 `docker image load` 核对三个 ID 能否还原。[Docker 镜像导出](https://docs.docker.com/reference/cli/docker/image/save/) · [Docker 镜像导入](https://docs.docker.com/reference/cli/docker/image/load/)

清单格式如下。填写归档文件的实际字节数及 SHA-256。每个 `reference` 对应配置 `images` 中的完整摘要引用，`image_id` 填写导出的 ID。

```json
{
  "version": 1,
  "platform": "linux/arm64",
  "archive": {"file": "images.tar", "bytes": 123, "sha256": "<archive-sha256>"},
  "images": {
    "center": {"reference": "<center-reference>", "image_id": "sha256:<center-id>"},
    "postgresql": {"reference": "<postgresql-reference>", "image_id": "sha256:<postgresql-id>"},
    "rendezvous": {"reference": "<rendezvous-reference>", "image_id": "sha256:<rendezvous-id>"}
  }
}
```

以 Ed25519 签署清单的原始字节。将清单、原始二进制签名和归档放到离线介质，归档与清单位于同一目录。

```sh
openssl pkeyutl -sign -rawin -inkey release-key.pem -in manifest.json -out manifest.sig
openssl pkey -in release-key.pem -pubout -out release-signing.pem
```

使用相同的 `preflight` 和 `install` 命令。预检校验签名与平台，安装在交给 Docker 前校验归档字节数及摘要，再核对已加载的镜像 ID 和架构。Compose 使用这些本地 ID 启动。首次安装成功后，重跑可复用已核验的缓存；缓存缺失时重新接入同一签名包。

## 开发验证

```sh
bash scripts/deskrealm-m3-test install-preflight
bash scripts/deskrealm-m3-test install-prepare
```

覆盖配置规范化、镜像引用、TLS 证书、Docker 上下文、端口冲突、已有状态与并发准备。

使用兼容 Docker、Go、Node.js、Python 3 与 OpenSSL 的开发机运行隔离首装检查。

```sh
npm --prefix web ci --ignore-scripts
npm --prefix web exec -- playwright install chromium
bash scripts/deskrealm-m3-test install-bootstrap
bash scripts/deskrealm-m3-test install-offline
bash scripts/deskrealm-m3-test install-resume
bash scripts/deskrealm-m3-test install-relay
bash scripts/deskrealm-m3-test install-task
```

检查构建本机 Docker 架构的 Go 中心、生产 Vue 静态文件和锁定受管源码的会合服务。两个临时镜像经回环 Registry 取得摘要后写入测试配置。安装器使用 Compose 启动四项服务并验活，Chromium 用一次性链接完成首管登录。随后模拟迁移后的 HTTPS 校验失败和安装中断，重跑后核对数据库和原公钥，再验证数据缺失或标识不符会阻止服务启动。数据库、测试 CA、镜像及 Compose 项目均为临时资源。

离线检查创建签名包，关闭临时 Registry 并清除两个测试服务的本地镜像。它先验证归档被篡改时安装停止，再验证从归档导入及移除介质后的服务重建。
