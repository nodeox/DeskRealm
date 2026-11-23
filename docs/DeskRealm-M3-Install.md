# DeskRealm M3 中心安装

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
- **部署目录与运行时。** 部署目录使用绝对路径，父目录须已存在。预检使用本地 Docker Unix socket，要求 Linux amd64 或 arm64 Engine 以及 Compose 插件。

预检暂时绑定拟使用的本机 TCP 和 UDP 端口，随后释放。域名从目标网络解析、实际代理证书链、跨网可达性、镜像来源和服务健康仍须在安装和验活阶段核对。Linux 中心与中继及双 Windows 目标环境验收按 Phase 5 顺序执行。

## 准备中心配置和身份

```sh
/tmp/deskrealm-install prepare --config /absolute/path/to/center.json
```

首次准备原子写入部署目录。输出部署 ID、Compose 项目与文件位置、会合公钥和受管签名公钥的指纹。后续用同一配置运行会复用部署 ID 和两套公钥。遇到配置不一致、未知内容、原密钥或数据目录缺失时，命令报告冲突，按原部署备份恢复。

Compose 包含 PostgreSQL、一次性迁移、Go 中心和受管会合四个服务。PostgreSQL 18 的数据目录挂载在 `/var/lib/postgresql` 并位于私有服务网。迁移连接使用 `deskrealm_migrate`，Go 中心使用只操作业务数据的 `deskrealm`。Go 中心在迁移成功后启动。中心镜像提供 `/usr/local/bin/deskrealm` 和 `/opt/deskrealm/web`，会合镜像提供从锁定受管源码构建的 `/usr/local/bin/hbbs`。 [PostgreSQL 官方容器数据目录](https://hub.docker.com/_/postgres)

使用自建 CA 时，可通过 `https.ca_certificate` 指定 PEM 绝对路径供受管会合校验中心 HTTPS。已有反代模式发布回环 API 端口，提供证书模式由 Go 服务读取挂载的证书和私钥。

密钥和服务凭据保存在受限权限的部署目录。检查 Compose 时保留服务环境文件的路径，以免在输出中展开凭据。 [Compose 配置命令](https://docs.docker.com/reference/cli/docker/compose/config/)

```sh
docker compose --env-file /dev/null -f /var/lib/deskrealm/compose.json \
  config --no-env-resolution
```

自动启动、中心健康检查、镜像准备和首管票据流程接续 [04-01](../.planning/phases/04-m3/04-01-PLAN.md)。

## 开发验证

```sh
bash scripts/deskrealm-m3-test install-preflight
bash scripts/deskrealm-m3-test install-prepare
```

覆盖配置规范化、镜像引用、TLS 证书、Docker 上下文、端口冲突、已有状态与并发准备。
