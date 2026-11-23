# DeskRealm 区域中继登记

区域中继的中心登记与健康回执使用独立节点身份。中心的 `relay_enrollment` 任务可从任务中心查看。

## 中心 API

平台管理员在同源会话中调用 `POST /v1/admin/relays`，提交 `name`、`site`、`region`、`endpoint`、`build` 和 `capacity`。公布端点使用带端口的 DNS 名称或 IP 地址。成功响应包含节点 ID、任务 ID 和 15 分钟有效的一次性登记凭据。将凭据通过授权渠道交给相应节点的运维人员。

登记未完成时，可调用 `POST /v1/admin/relays/{id}/credential` 为同一节点补发凭据。补发后旧凭据及旧挑战失效。节点完成登记后保持已登记的身份。

节点 API 使用已验证的中心 HTTPS 地址。节点生成并保存独立的 Ed25519 密钥，以无填充 base64url 编码公钥和签名。

1. 使用登记凭据作为 Bearer Token，向 `POST /v1/relays/enrollment/challenge` 提交 `node_id` 和 `public_key`。
2. 按 `relayprotocol.Enrollment.Frame` 对节点 ID、公布端点、站点、区域、版本、容量、挑战、有效期和公钥签名。使用原 Bearer Token，向 `POST /v1/relays/enrollment/complete` 提交 `node_id`、`public_key` 和 `signature`。
3. 通过 `POST /v1/relays/health` 上报 `relayprotocol.Health` 和对应签名。每次报告使用递增序号、当前观测时间和中心配置版本。中心拒绝重放、超出容量和版本不匹配的报告。

`GET /v1/admin/relays` 返回当前登记与健康状态。通过签名健康检查的节点显示为 `awaiting_probe`。独立的公布端点及协议探测仍是入池前置条件。

## 开发验证

```sh
bash scripts/deskrealm-m3-test relay-identity
```

节点主机命令、实际 `hbbr` 启动和探测接续 04-03。目标环境的 Linux 中心、中继和双 Windows 验收安排在 M0–M3 开发完成后。
