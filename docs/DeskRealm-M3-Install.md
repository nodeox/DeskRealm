# DeskRealm M3 中心安装

## 预检

使用 `deskrealm-install preflight` 检查准备安装的主机。预检以只读方式读取中心配置，核对 HTTPS 模式、镜像摘要引用、Docker Engine、Compose 和主机端口，并输出归一化的公开端点。安装、迁移和首管票据流程在 [04-01](../.planning/phases/04-m3/04-01-PLAN.md) 的后续检查点交付。

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

## 开发验证

```sh
bash scripts/deskrealm-m3-test install-preflight
```

检查配置规范化、镜像引用、TLS 证书、Docker 上下文、端口冲突和命令构建。
